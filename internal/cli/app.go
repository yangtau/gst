// Package cli implements the gst command line.
package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/service"
	"github.com/yangtau/ssh-tunnel/internal/sshconfig"
	"github.com/yangtau/ssh-tunnel/internal/sshconn"
	"github.com/yangtau/ssh-tunnel/internal/tun"
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
		stdin:        os.Stdin,
		stdout:       os.Stdout,
		stderr:       os.Stderr,
		newBackend:   func() (backend, error) { return newDaemonBackend(os.Stderr, os.Executable) },
		resolve:      sshconfig.Resolve,
		executable:   os.Executable,
		newHelper:    func() (helperService, error) { return service.NewHelper() },
		removeHelper: func() error { return tun.Remove(tun.Socket) },
		runRoot:      runTerminal,
		geteuid:      os.Geteuid,
		interactive: func() bool {
			return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
		},
	}
	return a.execute(context.Background(), args)
}

type app struct {
	stdin          io.Reader // nil reads nothing
	stdout, stderr io.Writer
	newBackend     func() (backend, error)
	resolve        func(ctx context.Context, destination string) (sshconfig.Host, error)
	executable     func() (string, error)
	newHelper      func() (helperService, error)
	removeHelper   func() error              // asks the helper to remove itself
	runRoot        func(argv []string) error // runs a command on this terminal
	geteuid        func() int
	interactive    func() bool // whether a person can answer

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
	if cmd.HasAvailableSubCommands() && len(placeholders(cmd)) == 0 {
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

// options resolves l's destination with ssh.
func (a *app) options(ctx context.Context, l *config.Link) (sshconn.Options, error) {
	host, err := a.resolve(ctx, l.Destination)
	if err != nil {
		return sshconn.Options{}, err
	}
	interval := cmp.Or(host.ServerAliveInterval, defaultKeepaliveInterval)
	return sshconn.Options{
		Server:            host.Address(),
		User:              host.User,
		IdentityFiles:     host.IdentityFiles,
		KnownHostsFiles:   host.KnownHostsFiles,
		ConnectTimeout:    cmp.Or(host.ConnectTimeout, defaultConnectTimeout),
		KeepaliveInterval: interval,
		KeepaliveTimeout:  interval * time.Duration(cmp.Or(host.ServerAliveCountMax, defaultKeepaliveCount)),
	}, nil
}

// preflight checks offline that link l can connect: its destination
// resolves, an identity is usable, and known_hosts records the server.
func (a *app) preflight(ctx context.Context, l *config.Link) error {
	opts, err := a.options(ctx, l)
	if err != nil {
		return withHint(err, "check the destination and your ssh configuration with `ssh -G "+l.Destination+"`")
	}
	if err := sshconn.Check(opts); err != nil {
		var problem *sshconn.CheckError
		if errors.As(err, &problem) {
			return withHint(err, problem.Hint)
		}
		return err
	}
	return nil
}

// runTerminal runs argv with this process's terminal, so that sudo can ask
// for a password itself.
func runTerminal(argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// remoteGst returns the command that runs gst on l's server.
func remoteGst(l *config.Link) string {
	return sshconn.ShellQuote(cmp.Or(l.RemoteGst, "gst"))
}

// connectLink connects to the daemon on the machine of link l over SSH,
// for the daemon. Its errors omit addresses, users, and paths.
func (a *app) connectLink(ctx context.Context, l *config.Link) (io.ReadWriteCloser, error) {
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
