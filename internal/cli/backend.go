package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// Status returns what the daemon reports, and false when no daemon
	// answers.
	Status(ctx context.Context) (wire.Status, bool)
	// Nodes returns the mesh as the daemon knows it.
	Nodes(ctx context.Context) (wire.Topology, error)
	// NodeStatus returns the status of the daemon on a machine of the
	// mesh, asked through the mesh.
	NodeStatus(ctx context.Context, node string) (wire.Status, error)
	// NetPing sends a packet to a machine and back over the virtual network.
	NetPing(ctx context.Context, node string, size int) (wire.NetPing, error)
	// Ping asks a machine of the mesh to answer through the links.
	Ping(ctx context.Context, node string) (wire.Ping, error)
	// Direct reports the UDP path to a machine of the mesh and how the
	// latest try at one went.
	Direct(ctx context.Context, node string) (wire.DirectReport, error)
}

// daemonBackend is the real backend.
type daemonBackend struct {
	manager service.Manager
	socket  string
	build   func() (string, error)
}

func newDaemonBackend(notes io.Writer, executable func() (string, error)) (backend, error) {
	m, err := service.New(notes)
	if err != nil {
		return nil, err
	}
	socket, err := service.Socket()
	if err != nil {
		return nil, err
	}
	return &daemonBackend{manager: m, socket: socket, build: func() (string, error) {
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
		return withHint(fmt.Errorf("gst could not apply the change: %w", err), b.manager.LogHint())
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
		c, err := daemon.DialWait(ctx, b.socket, software, build)
		switch {
		case err == nil && (build == "" || c.Daemon.Build == build):
			return c, nil
		case err == nil:
			c.Close()
		case build == "" || !otherProtocol(err):
			return nil, withHint(fmt.Errorf("%w: %w", errNoDaemon, err), b.manager.LogHint())
		}
		select {
		case <-ctx.Done():
			return nil, withHint(errors.New("gst still runs another version after a restart"), b.manager.LogHint())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// queryTimeout bounds a question to a running daemon.
const queryTimeout = 5 * time.Second

// query runs fn on a connection to a daemon that is already running,
// within queryTimeout.
func (b *daemonBackend) query(ctx context.Context, fn func(ctx context.Context, c *daemon.Client) error) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	c, err := daemon.Dial(ctx, b.socket, software, "")
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

// meshQuery runs fn on a connection to a running daemon for a question
// through the mesh, which bounds the relay itself; the query timeout
// would cut it short.
func (b *daemonBackend) meshQuery(ctx context.Context, fn func(ctx context.Context, c *daemon.Client) error) error {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	c, err := daemon.Dial(ctx, b.socket, software, "")
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(ctx, c)
}

func (b *daemonBackend) NodeStatus(ctx context.Context, node string) (st wire.Status, err error) {
	err = b.meshQuery(ctx, func(ctx context.Context, c *daemon.Client) error {
		st, err = c.NodeStatus(ctx, node)
		return err
	})
	return st, err
}

func (b *daemonBackend) NetPing(ctx context.Context, node string, size int) (p wire.NetPing, err error) {
	err = b.meshQuery(ctx, func(ctx context.Context, c *daemon.Client) error {
		p, err = c.NetPing(ctx, node, size)
		return err
	})
	return p, err
}

func (b *daemonBackend) Ping(ctx context.Context, node string) (p wire.Ping, err error) {
	err = b.meshQuery(ctx, func(ctx context.Context, c *daemon.Client) error {
		p, err = c.Ping(ctx, node)
		return err
	})
	return p, err
}

func (b *daemonBackend) Direct(ctx context.Context, node string) (r wire.DirectReport, err error) {
	err = b.meshQuery(ctx, func(ctx context.Context, c *daemon.Client) error {
		r, err = c.Direct(ctx, node)
		return err
	})
	return r, err
}
