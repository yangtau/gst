// Package cli implements the gst command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/sshconfig"
	"github.com/yangtau/ssh-tunnel/internal/tunnel"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1 // the command could not do its work
	exitUsage   = 2 // invalid command line or configuration
)

// Settings used when neither the tunnel nor the ssh configuration sets them.
const (
	defaultConnectTimeout    = 15 * time.Second
	defaultKeepaliveInterval = 15 * time.Second
	defaultKeepaliveCount    = 3
)

// Execute runs gst with args and returns the process exit code.
func Execute(args []string) int {
	a := &app{
		stdin:      os.Stdin,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		newBackend: func() (backend, error) { return newDaemonBackend(os.Stderr, os.Executable) },
		resolve:    sshconfig.Resolve,
		executable: os.Executable,
		openURL:    openBrowser,
	}
	return a.execute(context.Background(), args)
}

type app struct {
	stdin          io.Reader // nil reads nothing
	stdout, stderr io.Writer
	newBackend     func() (backend, error)
	resolve        func(ctx context.Context, destination string) (sshconfig.Host, error)
	executable     func() (string, error)
	openURL        func(url string) error // nil opens nothing

	configFlag string
	back       backend
	states     map[string]wire.TunnelState // what the daemon reported, fetched once
}

func (a *app) execute(ctx context.Context, args []string) int {
	root := a.rootCommand()
	root.SetArgs(args)
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return exitOK
	}
	var exit *exitError
	if errors.As(err, &exit) {
		return exit.code
	}
	fmt.Fprintf(a.stderr, "error: %v\n", err)
	// Anything but a failure is an argument or flag error, from cobra or usage().
	var failed *failure
	isUsage := !errors.As(err, &failed)
	if isUsage {
		fmt.Fprintf(a.stderr, "usage: %s\n", useLine(cmd))
	}
	var hinted *hintError
	if errors.As(err, &hinted) {
		fmt.Fprintf(a.stderr, "hint: %s\n", hinted.hint)
	}
	if !isUsage {
		return exitFailure
	}
	fmt.Fprintf(a.stderr, "Run '%s --help' for more information.\n", cmd.CommandPath())
	return exitUsage
}

func useLine(cmd *cobra.Command) string {
	if cmd.HasAvailableSubCommands() {
		return cmd.CommandPath() + " <command>"
	}
	return cmd.UseLine()
}

// usageError marks invalid arguments; the command exits with status 2.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func usage(format string, args ...any) error {
	return &usageError{fmt.Errorf(format, args...)}
}

// failure marks an error from a command's work; the command exits with status 1.
type failure struct{ err error }

func (e *failure) Error() string { return e.err.Error() }
func (e *failure) Unwrap() error { return e.err }

// hintError suggests what the user can do about err.
type hintError struct {
	err  error
	hint string
}

func (e *hintError) Error() string { return e.err.Error() }
func (e *hintError) Unwrap() error { return e.err }

func withHint(err error, hint string) error { return &hintError{err, hint} }

// exitError ends the command with code after it has reported the problem itself.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func (a *app) configPath() (string, error) {
	path := a.configFlag
	if path == "" {
		path = os.Getenv("GST_CONFIG")
	}
	if path == "" {
		return config.DefaultPath()
	}
	return config.AbsPath(path)
}

func (a *app) load() (*config.File, error) {
	path, err := a.configPath()
	if err != nil {
		return nil, err
	}
	return config.Load(path)
}

func (a *app) update(fn func(*config.File) error) error {
	path, err := a.configPath()
	if err != nil {
		return err
	}
	return config.Update(path, fn)
}

func lookup(f *config.File, name string) (*config.Tunnel, error) {
	t := f.Tunnels[name]
	if t == nil {
		return nil, namesHint(f, fmt.Errorf("no tunnel named %s", name))
	}
	return t, nil
}

// namesHint adds f's tunnel names to err.
func namesHint(f *config.File, err error) error {
	if len(f.Tunnels) == 0 {
		return withHint(err, "there are no tunnels yet; add one with `gst add <name>`")
	}
	return withHint(err, "tunnels: "+strings.Join(sortedNames(f), ", "))
}

func sortedNames(f *config.File) []string {
	names := make([]string, 0, len(f.Tunnels))
	for name := range f.Tunnels {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (a *app) backend() (backend, error) {
	if a.back == nil {
		b, err := a.newBackend()
		if err != nil {
			return nil, err
		}
		a.back = b
	}
	return a.back, nil
}

// Tunnel statuses, as gst list and show print them.
const (
	statusStopped    = "stopped"
	statusConnecting = wire.StateConnecting
	statusRunning    = wire.StateRunning
	statusDown       = wire.StateDown
)

// status returns the status of tunnel name, configured as t, and its last
// error, with the daemon's states fetched once per command.
func (a *app) status(ctx context.Context, name string, t *config.Tunnel) (string, string) {
	if a.states == nil {
		a.states = map[string]wire.TunnelState{}
		if b, err := a.backend(); err == nil {
			if states, ok := b.States(ctx); ok {
				a.states = states
			}
		}
	}
	return statusOf(name, t, a.states)
}

// statusOf returns the status of tunnel name, configured as t, and its last
// error, given the states the daemon reported. A started tunnel that the
// daemon does not run is down.
func statusOf(name string, t *config.Tunnel, states map[string]wire.TunnelState) (string, string) {
	if t.Stopped {
		return statusStopped, ""
	}
	s, ok := states[name]
	if !ok {
		return statusDown, ""
	}
	return s.State, s.Error
}

// options resolves t's destination with ssh and applies t's overrides.
func (a *app) options(ctx context.Context, t *config.Tunnel) (tunnel.Options, error) {
	host, err := a.resolve(ctx, t.Destination)
	if err != nil {
		return tunnel.Options{}, err
	}
	opts := tunnel.Options{
		Server:           host.Address(),
		User:             host.User,
		IdentityFiles:    host.IdentityFiles,
		KnownHostsFiles:  host.KnownHostsFiles,
		CleanupListeners: t.CleanupListeners,
		Forwards:         t.Forwards,
	}
	if t.IdentityFile != "" {
		opts.IdentityFiles = []string{t.IdentityFile}
	}
	if t.KnownHostsFile != "" {
		opts.KnownHostsFiles = []string{t.KnownHostsFile}
	}
	maxDuration := time.Duration(config.MaxSeconds) * time.Second
	opts.ConnectTimeout = min(firstPositive(seconds(t.ConnectTimeoutSeconds), host.ConnectTimeout, defaultConnectTimeout), maxDuration)
	opts.KeepaliveInterval = min(firstPositive(seconds(t.KeepaliveIntervalSeconds), host.ServerAliveInterval, defaultKeepaliveInterval), maxDuration)
	count := min(firstPositive(t.KeepaliveCountMax, int64(host.ServerAliveCountMax), defaultKeepaliveCount), config.MaxKeepaliveCount)
	opts.KeepaliveTimeout = opts.KeepaliveInterval * time.Duration(count)
	return opts, nil
}

func seconds(n int64) time.Duration { return time.Duration(n) * time.Second }

func firstPositive[T time.Duration | int64](values ...T) T {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

// preflight checks offline that t can start: its destination resolves, an
// identity is usable, and known_hosts records the server. A tunnel through
// the mesh only needs valid settings.
func (a *app) preflight(ctx context.Context, t *config.Tunnel) error {
	if t.Via == config.ViaMesh {
		// No SSH connection to check; the mesh routes each connection.
		if err := t.Validate(); err != nil {
			return &usageError{err}
		}
		return nil
	}
	opts, err := a.options(ctx, t)
	if err != nil {
		return withHint(err, "check the destination and your ssh configuration with `ssh -G "+t.Destination+"`")
	}
	for _, step := range tunnel.Diagnose(ctx, opts, false) {
		if !step.OK {
			return withHint(fmt.Errorf("%s: %s", step.Name, step.Detail), step.Hint)
		}
	}
	return nil
}

// daemonCommand is the command line the daemon's service runs.
func (a *app) daemonCommand() ([]string, error) {
	exe, err := a.executable()
	if err != nil {
		return nil, err
	}
	// "go run" builds into a temporary directory that is deleted on exit.
	if strings.Contains(exe, "/go-build") {
		return nil, withHint(errors.New("gst runs from a temporary go build, which a background service cannot use"),
			"install gst first, for example with `go install github.com/yangtau/ssh-tunnel/cmd/gst@latest`")
	}
	command := []string{exe}
	path, err := a.configPath()
	if err != nil {
		return nil, err
	}
	if def, err := config.DefaultPath(); err != nil || path != def {
		command = append(command, "--config", path)
	}
	return append(command, "daemon"), nil
}

// sync makes the daemon run the started tunnels of the configuration.
func (a *app) sync(ctx context.Context) error {
	path, err := a.configPath()
	if err != nil {
		return err
	}
	b, err := a.backend()
	if err != nil {
		return err
	}
	return config.Apply(path, func(f *config.File) error {
		needed := f.Mesh != nil
		for _, t := range f.Tunnels {
			needed = needed || !t.Stopped
		}
		var command []string
		if needed {
			var err error
			if command, err = a.daemonCommand(); err != nil {
				return err
			}
		}
		return b.Sync(ctx, command, needed)
	})
}

// restart restarts started tunnels with their current configuration.
func (a *app) restart(ctx context.Context, names []string) error {
	if err := a.sync(ctx); err != nil {
		return err
	}
	b, err := a.backend()
	if err != nil {
		return err
	}
	return b.Restart(ctx, names)
}

// applyChange restarts a started tunnel so that a configuration change
// takes effect, and tells the user what happened.
func (a *app) applyChange(ctx context.Context, name string, noRestart bool) error {
	f, err := a.load()
	if err != nil {
		return err
	}
	switch t := f.Tunnels[name]; {
	case t == nil:
		return nil
	case t.Stopped:
		fmt.Fprintf(a.stdout, "Tunnel %s is stopped; the change applies when you start it.\n", name)
	case noRestart:
		fmt.Fprintf(a.stdout, "Restart the tunnel to apply the change: gst restart %s\n", name)
	default:
		if err := a.restart(ctx, []string{name}); err != nil {
			return withHint(fmt.Errorf("restart tunnel %s: %w", name, err), "retry with `gst restart "+name+"`")
		}
		fmt.Fprintf(a.stdout, "Restarted tunnel %s.\n", name)
	}
	return nil
}
