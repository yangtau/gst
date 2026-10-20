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

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/service"
	"github.com/yangtau/ssh-tunnel/internal/sshconfig"
	"github.com/yangtau/ssh-tunnel/internal/tunnel"
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
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		newManager: func() (service.Manager, error) { return service.New(os.Stderr) },
		resolve:    sshconfig.Resolve,
		executable: os.Executable,
	}
	return a.execute(context.Background(), args)
}

type app struct {
	stdout, stderr io.Writer
	newManager     func() (service.Manager, error)
	resolve        func(ctx context.Context, destination string) (sshconfig.Host, error)
	executable     func() (string, error)

	configFlag string
	manager    service.Manager
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
	var hinted *hintError
	if errors.As(err, &hinted) {
		fmt.Fprintf(a.stderr, "hint: %s\n", hinted.hint)
	}
	var failed *failure
	if errors.As(err, &failed) {
		return exitFailure
	}
	// Anything else is an argument or flag error, from cobra or usage().
	fmt.Fprintf(a.stderr, "Run '%s --help' for usage.\n", cmd.CommandPath())
	return exitUsage
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

func notFound(name string) error {
	return withHint(fmt.Errorf("no tunnel named %s", name), "list tunnels with `gst tunnel list`")
}

func lookup(f *config.File, name string) (*config.Tunnel, error) {
	t := f.Tunnels[name]
	if t == nil {
		return nil, notFound(name)
	}
	return t, nil
}

func sortedNames(f *config.File) []string {
	names := make([]string, 0, len(f.Tunnels))
	for name := range f.Tunnels {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (a *app) services() (service.Manager, error) {
	if a.manager == nil {
		m, err := a.newManager()
		if err != nil {
			return nil, err
		}
		a.manager = m
	}
	return a.manager, nil
}

func (a *app) status(name string) service.Status {
	m, err := a.services()
	if err != nil {
		return "unknown"
	}
	status, err := m.Status(name)
	if err != nil {
		return "unknown"
	}
	return status
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
// identity is usable, and known_hosts records the server.
func (a *app) preflight(ctx context.Context, t *config.Tunnel) error {
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

// serviceCommand is the command line the background service runs.
func (a *app) serviceCommand(name string) ([]string, error) {
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
	return append(command, "run", name), nil
}

func (a *app) startService(name string) error {
	command, err := a.serviceCommand(name)
	if err != nil {
		return err
	}
	m, err := a.services()
	if err != nil {
		return err
	}
	return m.Start(name, command)
}

// applyChange restarts a started tunnel so that a configuration change
// takes effect, and tells the user what happened.
func (a *app) applyChange(name string, noRestart bool) error {
	switch {
	case a.status(name) == service.Stopped:
		fmt.Fprintf(a.stdout, "Tunnel %s is stopped; the change applies when you start it.\n", name)
	case noRestart:
		fmt.Fprintf(a.stdout, "Restart the tunnel to apply the change: gst tunnel restart %s\n", name)
	default:
		if err := a.startService(name); err != nil {
			return withHint(fmt.Errorf("restart tunnel %s: %w", name, err), "retry with `gst tunnel restart "+name+"`")
		}
		fmt.Fprintf(a.stdout, "Restarted tunnel %s.\n", name)
	}
	return nil
}
