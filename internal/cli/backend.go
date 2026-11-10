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
	// Call calls method, one of wire's control methods, of the daemon,
	// which must run already, and decodes its result into result. It
	// bounds the call: longer for the methods that ask other machines.
	Call(ctx context.Context, method string, params, result any) error
}

// call calls method of the daemon, and returns its result.
func call[R any](ctx context.Context, b backend, method string, params any) (R, error) {
	var r R
	err := b.Call(ctx, method, params, &r)
	return r, err
}

// status returns what the daemon reports, and false when no daemon
// answers.
func status(ctx context.Context, b backend) (wire.Status, bool) {
	st, err := call[wire.Status](ctx, b, wire.MethodStatus, nil)
	return st, err == nil
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
	if err := c.Call(ctx, wire.MethodReload, nil, nil); err != nil {
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

// Bounds of a call to a running daemon: a question of this machine, and
// one that goes along the links to another machine, which bounds the
// relay itself.
const (
	queryTimeout = 5 * time.Second
	meshTimeout  = 40 * time.Second
)

// meshMethods are the control methods that ask other machines.
var meshMethods = map[string]bool{
	wire.MethodNodeStatus: true,
	wire.MethodNetPing:    true,
	wire.MethodPing:       true,
	wire.MethodDirect:     true,
}

func (b *daemonBackend) Call(ctx context.Context, method string, params, result any) error {
	timeout := queryTimeout
	if meshMethods[method] {
		timeout = meshTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, err := daemon.Dial(ctx, b.socket, software, "")
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Call(ctx, method, params, result)
}
