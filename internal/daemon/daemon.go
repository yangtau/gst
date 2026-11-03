// Package daemon runs the gst daemon: one background process per user that
// runs every started tunnel of the configuration file and answers gst
// commands on a Unix control socket using the gst protocol (internal/wire).
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/logfile"
	"github.com/yangtau/ssh-tunnel/internal/mesh"
	"github.com/yangtau/ssh-tunnel/internal/tunnel"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Config configures a daemon.
type Config struct {
	ConfigPath string // the gst configuration file
	Socket     string // control socket path
	LogDir     string // directory of the tunnels' logs, <name>.log
	Software   string // reported in Hello and Status, such as "gst 0.2.0"
	Build      string // identifies the executable; see Build

	// Resolve turns a configured tunnel into the options to run it with.
	Resolve func(ctx context.Context, t *config.Tunnel) (tunnel.Options, error)
	// Run runs a tunnel until ctx is done or it fails; tunnel.Run when nil.
	Run func(ctx context.Context, opts tunnel.Options) error
	// RetryDelay is the wait after a tunnel fails; 5 s when zero.
	RetryDelay time.Duration
	// Log receives the daemon's own log; the standard logger when nil.
	Log *log.Logger
	// MeshRetry is the wait before a failed peer link retries; 10 s when zero.
	MeshRetry time.Duration
}

// Daemon runs tunnels and answers control connections.
type Daemon struct {
	cfg Config

	mesh *mesh.Node

	ops     sync.Mutex // serializes reloads and restarts
	mu      sync.Mutex // guards tunnels
	tunnels map[string]*supervisor
}

// New returns a daemon for cfg.
func New(cfg Config) *Daemon {
	if cfg.Run == nil {
		cfg.Run = tunnel.Run
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = 5 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	m := mesh.New(cfg.Software, cfg.Log)
	if cfg.MeshRetry > 0 {
		m.SetRetry(cfg.MeshRetry)
	}
	return &Daemon{cfg: cfg, mesh: m, tunnels: map[string]*supervisor{}}
}

// Serve takes the control socket, starts the configured tunnels, and
// answers control connections until ctx is done, when it stops every
// tunnel. Only one daemon can serve a socket at a time.
func (d *Daemon) Serve(ctx context.Context) error {
	listener, unlock, err := listen(d.cfg.Socket)
	if err != nil {
		return err
	}
	defer unlock()
	defer d.stopAll()

	if err := d.Reload(ctx); err != nil {
		// Keep serving: a later reload, after the configuration is
		// fixed, can still start the tunnels.
		d.cfg.Log.Print(err)
	}
	d.cfg.Log.Printf("%s daemon ready", d.cfg.Software)

	var conns sync.WaitGroup
	defer conns.Wait()
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("control socket accept failed: %w", err)
		}
		conns.Go(func() {
			defer conn.Close()
			defer context.AfterFunc(ctx, func() { conn.Close() })()
			d.serveConn(ctx, conn)
		})
	}
}

// listen takes the lock beside socket and listens on it, replacing a
// socket file left behind by a daemon that is gone. The socket and its
// directory are accessible to their owner only.
func listen(socket string) (net.Listener, func(), error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create the socket directory: %w", err)
	}
	lock, err := os.OpenFile(socket+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open the daemon lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, nil, errors.New("another gst daemon is running")
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		lock.Close()
		return nil, nil, fmt.Errorf("remove the old control socket: %w", err)
	}
	old := syscall.Umask(0o177)
	listener, err := net.Listen("unix", socket)
	syscall.Umask(old)
	if err != nil {
		lock.Close()
		return nil, nil, fmt.Errorf("listen on the control socket: %w", err)
	}
	return listener, func() { listener.Close(); lock.Close() }, nil
}

func (d *Daemon) hello() wire.Hello {
	return wire.Hello{Software: d.cfg.Software, Role: wire.RoleControl, Build: d.cfg.Build}
}

// serveConn serves a gst command (role control) or, through gst
// peer-bridge, a peer daemon (role peer).
func (d *Daemon) serveConn(ctx context.Context, conn net.Conn) {
	var role string
	hello := d.hello()
	_, err := wire.Accept(conn, hello, func(h wire.Hello) *wire.Error {
		role = h.Role
		switch h.Role {
		case wire.RoleControl:
			return nil
		case wire.RolePeer:
			return d.mesh.CheckHello(h)
		}
		return wire.Errorf(wire.CodeUnsupportedRole, "role %q is not supported", h.Role)
	}, func(h wire.Hello) wire.Hello {
		if h.Role == wire.RolePeer {
			return d.mesh.Hello()
		}
		return hello
	})
	if err != nil {
		return
	}
	if role == wire.RolePeer {
		d.mesh.Accept(ctx, conn)
		return
	}
	wire.Serve(conn, func(method string, params json.RawMessage) (any, *wire.Error) {
		return d.handle(ctx, method, params)
	})
}

func (d *Daemon) handle(ctx context.Context, method string, params json.RawMessage) (any, *wire.Error) {
	switch method {
	case wire.MethodStatus:
		return d.Status(), nil
	case wire.MethodPeers:
		return d.mesh.Peers(), nil
	case wire.MethodReload:
		if err := d.Reload(ctx); err != nil {
			return nil, wire.Errorf(wire.CodeFailed, "%v", err)
		}
		return d.Status(), nil
	case wire.MethodRestart:
		var p wire.RestartParams
		if err := wire.DecodeParams(params, &p); err != nil {
			return nil, err
		}
		if err := d.Restart(ctx, p.Names); err != nil {
			var werr *wire.Error
			if errors.As(err, &werr) {
				return nil, werr
			}
			return nil, wire.Errorf(wire.CodeFailed, "%v", err)
		}
		return d.Status(), nil
	default:
		return nil, wire.Errorf(wire.CodeUnsupportedMethod, "method %q is not supported", method)
	}
}

// Status reports the daemon and its tunnels, sorted by name.
func (d *Daemon) Status() wire.Status {
	d.mu.Lock()
	states := make([]wire.TunnelState, 0, len(d.tunnels))
	for _, s := range d.tunnels {
		states = append(states, s.state())
	}
	d.mu.Unlock()
	slices.SortFunc(states, func(a, b wire.TunnelState) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return wire.Status{Software: d.cfg.Software, Build: d.cfg.Build, PID: os.Getpid(), Config: d.cfg.ConfigPath, Tunnels: states}
}

// Reload reads the configuration and starts the started tunnels that do
// not run and stops the tunnels that are stopped or gone. Tunnels whose
// configuration changed keep running as they are until Restart.
func (d *Daemon) Reload(ctx context.Context) error {
	d.ops.Lock()
	defer d.ops.Unlock()
	f, err := config.Load(d.cfg.ConfigPath)
	if err != nil {
		return err
	}
	d.mesh.Configure(f.Mesh)
	var stop []*supervisor
	d.mu.Lock()
	for name, s := range d.tunnels {
		if t := f.Tunnels[name]; t == nil || t.Stopped {
			stop = append(stop, s)
			delete(d.tunnels, name)
		}
	}
	d.mu.Unlock()
	stopAll(stop)
	for name, t := range f.Tunnels {
		d.mu.Lock()
		running := d.tunnels[name] != nil
		d.mu.Unlock()
		if !t.Stopped && !running {
			if err := d.start(name, t); err != nil {
				return err
			}
		}
	}
	return nil
}

// Restart restarts the named tunnels with their current configuration.
// Every name must be a started tunnel.
func (d *Daemon) Restart(ctx context.Context, names []string) error {
	d.ops.Lock()
	defer d.ops.Unlock()
	f, err := config.Load(d.cfg.ConfigPath)
	if err != nil {
		return err
	}
	for _, name := range names {
		if t := f.Tunnels[name]; t == nil || t.Stopped {
			return wire.Errorf(wire.CodeNotFound, "tunnel %s is not started", name)
		}
	}
	for _, name := range names {
		d.mu.Lock()
		old := d.tunnels[name]
		delete(d.tunnels, name)
		d.mu.Unlock()
		if old != nil {
			old.stop()
		}
		if err := d.start(name, f.Tunnels[name]); err != nil {
			return err
		}
	}
	return nil
}

// start runs a tunnel until it is stopped, independent of the request
// that started it.
func (d *Daemon) start(name string, t *config.Tunnel) error {
	var logOut *logfile.File
	if d.cfg.LogDir != "" {
		if err := os.MkdirAll(d.cfg.LogDir, 0o700); err != nil {
			return fmt.Errorf("create the log directory: %w", err)
		}
		var err error
		if logOut, err = logfile.Open(filepath.Join(d.cfg.LogDir, name+".log"), nil); err != nil {
			return fmt.Errorf("open the log of tunnel %s: %w", name, err)
		}
	}
	s := newSupervisor(d, name, *t, logOut)
	d.mu.Lock()
	d.tunnels[name] = s
	d.mu.Unlock()
	go s.run()
	d.cfg.Log.Printf("tunnel %s started", name)
	return nil
}

func (d *Daemon) stopAll() {
	d.mu.Lock()
	all := make([]*supervisor, 0, len(d.tunnels))
	for name, s := range d.tunnels {
		all = append(all, s)
		delete(d.tunnels, name)
	}
	d.mu.Unlock()
	stopAll(all)
}

func stopAll(list []*supervisor) {
	var wg sync.WaitGroup
	for _, s := range list {
		wg.Go(s.stop)
	}
	wg.Wait()
}
