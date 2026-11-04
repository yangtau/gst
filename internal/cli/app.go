// Package cli implements the gst command line.
package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/sshconfig"
	"github.com/yangtau/ssh-tunnel/internal/sshconn"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1 // the command could not do its work
	exitUsage   = 2 // invalid command line or configuration
)

// Settings used when neither the link nor the ssh configuration sets them.
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

func lookup(f *config.File, name string) (*config.Link, error) {
	l := f.Links[name]
	if l == nil {
		return nil, namesHint(f, fmt.Errorf("no link named %s", name))
	}
	return l, nil
}

// namesHint adds f's link names to err.
func namesHint(f *config.File, err error) error {
	if len(f.Links) == 0 {
		return withHint(err, "there are no links yet; add one with `gst link add <name>`")
	}
	return withHint(err, "links: "+strings.Join(sortedNames(f), ", "))
}

func sortedNames(f *config.File) []string {
	return slices.Sorted(maps.Keys(f.Links))
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

// Link statuses, as gst link list and show print them.
const (
	statusStopped    = "stopped"
	statusConnecting = wire.StateConnecting
	statusRunning    = wire.StateRunning
	statusDown       = wire.StateDown
)

// status returns what the daemon reports, or nil when no daemon answers.
func (a *app) status(ctx context.Context) *wire.Status {
	b, err := a.backend()
	if err != nil {
		return nil
	}
	if st, ok := b.Status(ctx); ok {
		return &st
	}
	return nil
}

// linkStatus returns the status of link name, configured as l, and its
// state as the daemon reports it, which is zero for a link that the daemon
// does not run. A started link that the daemon does not run is down.
func linkStatus(name string, l *config.Link, st *wire.Status) (string, wire.LinkState) {
	if l.Stopped {
		return statusStopped, wire.LinkState{}
	}
	if st != nil {
		for _, s := range st.Links {
			if s.Name == name {
				return s.State, s
			}
		}
	}
	return statusDown, wire.LinkState{}
}

// options resolves l's destination with ssh and applies l's overrides.
func (a *app) options(ctx context.Context, l *config.Link) (sshconn.Options, error) {
	host, err := a.resolve(ctx, l.Destination)
	if err != nil {
		return sshconn.Options{}, err
	}
	opts := sshconn.Options{
		Server:          host.Address(),
		User:            host.User,
		IdentityFiles:   host.IdentityFiles,
		KnownHostsFiles: host.KnownHostsFiles,
	}
	if l.IdentityFile != "" {
		opts.IdentityFiles = []string{l.IdentityFile}
	}
	if l.KnownHostsFile != "" {
		opts.KnownHostsFiles = []string{l.KnownHostsFile}
	}
	maxDuration := time.Duration(config.MaxSeconds) * time.Second
	opts.ConnectTimeout = min(firstPositive(seconds(l.ConnectTimeoutSeconds), host.ConnectTimeout, defaultConnectTimeout), maxDuration)
	opts.KeepaliveInterval = min(firstPositive(seconds(l.KeepaliveIntervalSeconds), host.ServerAliveInterval, defaultKeepaliveInterval), maxDuration)
	count := min(firstPositive(l.KeepaliveCountMax, int64(host.ServerAliveCountMax), defaultKeepaliveCount), config.MaxKeepaliveCount)
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

// preflight checks offline that link l can connect: its destination
// resolves, an identity is usable, and known_hosts records the server.
func (a *app) preflight(ctx context.Context, l *config.Link) error {
	opts, err := a.options(ctx, l)
	if err != nil {
		return withHint(err, "check the destination and your ssh configuration with `ssh -G "+l.Destination+"`")
	}
	for _, step := range sshconn.Diagnose(ctx, opts, false, "") {
		if !step.OK {
			return withHint(fmt.Errorf("%s: %s", step.Name, step.Detail), step.Hint)
		}
	}
	return nil
}

// remoteGst returns the command that runs gst on l's server.
func remoteGst(l *config.Link) string {
	return sshconn.ShellQuote(cmp.Or(l.RemoteGst, "gst"))
}

// connectLink connects to the daemon on the machine of link l over SSH,
// for the daemon. Its errors omit addresses, users, and paths.
func (a *app) connectLink(ctx context.Context, _ string, l *config.Link) (io.ReadWriteCloser, error) {
	opts, err := a.options(ctx, l)
	if err != nil {
		// Resolution errors quote the ssh configuration.
		return nil, errors.New("ssh config resolution failed")
	}
	conn, err := sshconn.Dial(ctx, opts)
	if err != nil {
		return nil, err
	}
	// A server can stall opening the session; the mesh handshake bounds
	// only what follows.
	stall := time.AfterFunc(defaultConnectTimeout, func() { conn.Close() })
	session, err := conn.Start(remoteGst(l) + " peer-bridge")
	if !stall.Stop() && err == nil {
		session.Close()
		err = errors.New("the remote command did not start in time")
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &linkConn{session, conn}, nil
}

// linkConn is gst peer-bridge on a link's server; closing it closes the
// SSH connection too.
type linkConn struct {
	*sshconn.Session
	conn *sshconn.Conn
}

func (c *linkConn) Close() error {
	c.Session.Close()
	return c.conn.Close()
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

// sync makes the daemon apply the configuration, starting it unless gst
// is down on this machine.
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
		if f.Down {
			fmt.Fprintln(a.stderr, "note: gst is down on this machine; changes apply after `gst up`")
			return b.Sync(ctx, nil)
		}
		command, err := a.daemonCommand()
		if err != nil {
			return err
		}
		return b.Sync(ctx, command)
	})
}

// restart restarts started links with their current configuration.
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

// applyChange restarts a started link so that a configuration change
// takes effect, and tells the user what happened.
func (a *app) applyChange(ctx context.Context, name string, noRestart bool) error {
	f, err := a.load()
	if err != nil {
		return err
	}
	switch l := f.Links[name]; {
	case l == nil:
		return nil
	case l.Stopped:
		fmt.Fprintf(a.stdout, "Link %s is stopped; the change applies when you start it.\n", name)
	case noRestart:
		fmt.Fprintf(a.stdout, "Restart the link to apply the change: gst link restart %s\n", name)
	default:
		if err := a.restart(ctx, []string{name}); err != nil {
			return withHint(fmt.Errorf("restart link %s: %w", name, err), "retry with `gst link restart "+name+"`")
		}
		fmt.Fprintf(a.stdout, "Restarted link %s.\n", name)
	}
	return nil
}
