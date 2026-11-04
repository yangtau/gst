package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/daemon"
	"github.com/yangtau/ssh-tunnel/internal/service"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// backend is the gst daemon on this machine. The real one controls the
// daemon's service; tests use a fake.
type backend interface {
	// Sync makes the background match the configuration file. With a
	// command, it installs the daemon's service to run command, restarts
	// a daemon that runs another gst, and tells the daemon to reload the
	// configuration; with none, it removes the service.
	Sync(ctx context.Context, command []string) error
	// Restart restarts started links in a daemon that Sync brought up.
	Restart(ctx context.Context, names []string) error
	// Status returns what the daemon reports, and false when no daemon
	// answers.
	Status(ctx context.Context) (wire.Status, bool)
	// Nodes returns the mesh as the daemon knows it.
	Nodes(ctx context.Context) (wire.Topology, error)
	// NodeStatus returns the status of the daemon on a machine of the
	// mesh, asked through the mesh.
	NodeStatus(ctx context.Context, node string) (wire.Status, error)
	// Connect connects to target on a machine of the mesh through the
	// daemon.
	Connect(ctx context.Context, node string, target wire.Target) (io.ReadWriteCloser, error)
	// Logs writes the last lines of a link's log to w and then, if
	// follow is set, new lines until ctx is done.
	Logs(ctx context.Context, name string, lines int, follow bool, w io.Writer) error
	RemoveLogs(name string) error
}

const software = "gst " + version

// daemonBackend is the real backend.
type daemonBackend struct {
	manager service.Manager
	paths   service.Paths
	build   func() (string, error)
}

func newDaemonBackend(notes io.Writer, executable func() (string, error)) (backend, error) {
	m, err := service.New(notes)
	if err != nil {
		return nil, err
	}
	paths, err := service.DefaultPaths()
	if err != nil {
		return nil, err
	}
	return &daemonBackend{manager: m, paths: paths, build: func() (string, error) {
		exe, err := executable()
		if err != nil {
			return "", err
		}
		return daemon.Build(exe)
	}}, nil
}

func (b *daemonBackend) Sync(ctx context.Context, command []string) error {
	if command == nil {
		return b.manager.Uninstall()
	}
	build, err := b.build()
	if err != nil {
		return err
	}
	if err := b.manager.Install(command, false); err != nil {
		return err
	}
	c, err := b.connect(ctx, "")
	if otherProtocol(err) {
		c, err = nil, nil
	}
	if err != nil {
		return err
	}
	if c == nil || c.Daemon.Build != build {
		// The daemon runs another gst, as after an upgrade.
		if c != nil {
			c.Close()
		}
		if err := b.manager.Install(command, true); err != nil {
			return err
		}
		if c, err = b.connect(ctx, build); err != nil {
			return err
		}
	}
	defer c.Close()
	if _, err := c.Reload(ctx); err != nil {
		return withHint(fmt.Errorf("the gst daemon could not reload: %w", err), b.manager.LogHint())
	}
	return nil
}

// otherProtocol reports whether err is the handshake failure with a
// daemon of another gst protocol, as after an upgrade.
func otherProtocol(err error) bool {
	var refused *wire.Error
	return errors.As(err, &refused) && refused.Code == wire.CodeUnsupportedProto
}

// connect connects to the daemon, waiting for its service to start it.
// With build set, it also waits until a daemon of that build answers, as
// one that is still shutting down may answer for a moment.
func (b *daemonBackend) connect(ctx context.Context, build string) (*daemon.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		c, err := daemon.DialWait(ctx, b.paths.Socket, software, build)
		switch {
		case err == nil && (build == "" || c.Daemon.Build == build):
			return c, nil
		case err == nil:
			c.Close()
		case build == "" || !otherProtocol(err):
			return nil, withHint(fmt.Errorf("the gst daemon does not answer: %w", err), b.manager.LogHint())
		}
		select {
		case <-ctx.Done():
			return nil, withHint(errors.New("the gst daemon still runs another gst after a restart"), b.manager.LogHint())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (b *daemonBackend) Restart(ctx context.Context, names []string) error {
	c, err := b.connect(ctx, "")
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Restart(ctx, names)
	return err
}

// queryTimeout bounds a question to a running daemon.
const queryTimeout = 5 * time.Second

// query runs fn on a connection to a daemon that is already running,
// within queryTimeout.
func (b *daemonBackend) query(ctx context.Context, fn func(ctx context.Context, c *daemon.Client) error) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	c, err := daemon.Dial(ctx, b.paths.Socket, software, "")
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(ctx, c)
}

func (b *daemonBackend) Status(ctx context.Context) (st wire.Status, ok bool) {
	err := b.query(ctx, func(ctx context.Context, c *daemon.Client) (err error) {
		st, err = c.Status(ctx)
		return err
	})
	return st, err == nil
}

func (b *daemonBackend) Nodes(ctx context.Context) (topo wire.Topology, err error) {
	err = b.query(ctx, func(ctx context.Context, c *daemon.Client) error {
		topo, err = c.Nodes(ctx)
		return err
	})
	return topo, err
}

func (b *daemonBackend) NodeStatus(ctx context.Context, node string) (st wire.Status, err error) {
	// The mesh bounds the relay; the query timeout would cut it short.
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	c, err := daemon.Dial(ctx, b.paths.Socket, software, "")
	if err != nil {
		return st, err
	}
	defer c.Close()
	return c.NodeStatus(ctx, node)
}

// Connect connects through the mesh. The connection is set up within
// connectTimeout, which leaves the mesh its own relay timeout.
func (b *daemonBackend) Connect(ctx context.Context, node string, target wire.Target) (io.ReadWriteCloser, error) {
	const connectTimeout = 40 * time.Second
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	c, err := daemon.Dial(ctx, b.paths.Socket, software, "")
	if err != nil {
		return nil, fmt.Errorf("the gst daemon does not answer: %w", err)
	}
	conn, err := c.Connect(ctx, node, target)
	if err != nil {
		c.Close()
		return nil, err
	}
	return conn, nil
}

func (b *daemonBackend) logPath(name string) string {
	return filepath.Join(b.paths.LogDir, name+".log")
}

func (b *daemonBackend) Logs(ctx context.Context, name string, lines int, follow bool, w io.Writer) error {
	path := b.logPath(name)
	if _, err := os.Stat(path); err != nil && !follow {
		_, err := fmt.Fprintf(w, "No log for link %s yet.\n", name)
		return err
	}
	args := []string{"-n", strconv.Itoa(lines)}
	if follow {
		args = append(args, "-F")
	}
	cmd := exec.CommandContext(ctx, "tail", append(args, path)...)
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("tail: %w", err)
	}
	return nil
}

func (b *daemonBackend) RemoveLogs(name string) error {
	for _, path := range []string{b.logPath(name), b.logPath(name) + ".1"} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
