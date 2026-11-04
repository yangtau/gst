// Package daemon runs the gst daemon: one background process per user
// that is this machine's node in the mesh. It runs the started links of
// the configuration file, accepts the links of other machines, serves the
// proxy and the maps, and answers gst commands on a Unix control socket
// using the gst protocol (internal/wire).
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/mesh"
	"github.com/yangtau/ssh-tunnel/internal/relay"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Config configures a daemon.
type Config struct {
	ConfigPath string // the gst configuration file
	Socket     string // control socket path
	Software   string // reported in Hello and Status, such as "gst 0.3.0"
	Build      string // identifies the executable; see Build

	// Connect opens a connection to the daemon on the machine of the link
	// called name, as gst peer-bridge there does; closing it ends
	// everything under it. Its errors are logged, so they must not quote
	// addresses, users, or paths.
	Connect func(ctx context.Context, name string, l *config.Link) (io.ReadWriteCloser, error)
	// RetryDelay is the first wait after a link fails, which doubles with
	// each failure up to maxRetry; 5 s when zero.
	RetryDelay time.Duration
	// Log receives the daemon's own log; the standard logger when nil.
	Log *log.Logger
}

// Daemon runs links, listeners, and control connections.
type Daemon struct {
	cfg Config

	mesh    *mesh.Node
	control wire.Server                 // the methods of control connections
	config  atomic.Pointer[config.File] // as the last reload read it

	ops      sync.Mutex // serializes reloads and restarts
	mu       sync.Mutex // guards links and listeners
	links    map[string]*supervisor
	proxy    *listener
	maps     map[string]*listener // by listen address
	serveCtx context.Context      // the listeners' context, which ends with Serve
}

// New returns a daemon for cfg.
func New(cfg Config) *Daemon {
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = 5 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	d := &Daemon{cfg: cfg, links: map[string]*supervisor{}, maps: map[string]*listener{}, serveCtx: context.Background()}
	m := mesh.New(cfg.Software, cfg.Log, d.Status)
	d.mesh = m
	d.config.Store(&config.File{})
	none := func(f func() any) func(context.Context, struct{}) (any, error) {
		return func(context.Context, struct{}) (any, error) { return f(), nil }
	}
	wire.Handle(&d.control, wire.MethodStatus, none(func() any { return d.Status() }))
	wire.Handle(&d.control, wire.MethodNodes, none(func() any { return m.Topology() }))
	wire.Handle(&d.control, wire.MethodReload, func(ctx context.Context, _ struct{}) (any, error) {
		if err := d.Reload(ctx); err != nil {
			return nil, err
		}
		return d.Status(), nil
	})
	wire.Handle(&d.control, wire.MethodNodeStatus, func(ctx context.Context, p wire.NodeParams) (any, error) {
		var st wire.Status
		return st, m.Call(ctx, p.Node, wire.MethodPeerStatus, nil, &st)
	})
	wire.Handle(&d.control, wire.MethodPing, func(ctx context.Context, p wire.NodeParams) (any, error) {
		return m.Ping(ctx, p.Node)
	})
	wire.HandleStream(&d.control, wire.MethodConnect, d.connect)
	return d
}

// Serve takes the control socket, starts what the configuration asks
// for, and answers control connections until ctx is done, when it stops
// everything. Only one daemon can serve a socket at a time.
func (d *Daemon) Serve(ctx context.Context) error {
	listener, unlock, err := listen(d.cfg.Socket)
	if err != nil {
		return err
	}
	defer unlock()
	defer d.stopAll()
	meshCtx, stopMesh := context.WithCancel(ctx)
	defer stopMesh()
	go d.mesh.Run(meshCtx)
	d.mu.Lock()
	d.serveCtx = meshCtx
	d.mu.Unlock()

	if err := d.Reload(ctx); err != nil {
		// Keep serving: a later reload, after the configuration is
		// fixed, can still start the links.
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

// serveConn serves a gst command (role control) or, through gst
// peer-bridge, a peer daemon (role peer).
func (d *Daemon) serveConn(ctx context.Context, conn net.Conn) {
	theirs, err := wire.ReadHello(conn)
	if err != nil {
		return
	}
	mine := wire.Hello{Software: d.cfg.Software, Role: wire.RoleControl, Build: d.cfg.Build}
	switch theirs.Role {
	case wire.RolePeer:
		d.mesh.Accept(ctx, conn, theirs)
	case wire.RoleControl:
		if wire.Answer(conn, theirs, mine, nil) == nil {
			d.control.Serve(ctx, conn)
		}
	default:
		wire.Answer(conn, theirs, mine, wire.Errorf(wire.CodeUnsupportedRole, "role %q is not supported", theirs.Role))
	}
}

// connect answers mesh.connect and then carries the bytes of the
// connection until either side ends.
func (d *Daemon) connect(ctx context.Context, conn net.Conn, p wire.ConnectParams, accept func(any) error) error {
	target, err := d.mesh.Dial(ctx, p.Node, p.Target.Network, p.Target.Address)
	if err != nil {
		return err
	}
	if err := accept(nil); err != nil {
		target.Close()
		return err
	}
	relay.Pipe(conn, target)
	return nil
}

// Status reports the daemon and what it runs, sorted.
func (d *Daemon) Status() wire.Status {
	f := d.config.Load()
	st := wire.Status{
		Software: d.cfg.Software, Build: d.cfg.Build, PID: os.Getpid(), Config: d.cfg.ConfigPath,
		Node: d.mesh.Self(), Proxy: wire.Listener{Listen: f.Proxy},
		Links: []wire.LinkState{}, Maps: []wire.MapState{}, Routes: []wire.Route{},
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.proxy != nil {
		st.Proxy = d.proxy.state()
	}
	for _, name := range slices.Sorted(maps.Keys(f.Links)) {
		l := f.Links[name]
		state := wire.LinkState{Name: name, Destination: l.Destination, State: wire.StateDown}
		if s := d.links[name]; s != nil {
			state = s.state()
		}
		st.Links = append(st.Links, state)
	}
	for _, m := range f.Maps {
		state := wire.MapState{Listener: wire.Listener{Listen: m.Listen}, Machine: m.Machine, Target: m.Target}
		if l := d.maps[m.Listen]; l != nil {
			state.Listener = l.state()
		}
		st.Maps = append(st.Maps, state)
	}
	for _, r := range f.Routes {
		st.Routes = append(st.Routes, wire.Route{Match: r.Match, Via: r.Via})
	}
	return st
}

// Reload reads the configuration and applies it: the node's identity,
// the proxy, the maps, and the routes; new links start, and links that
// are gone or changed stop, the changed ones to start again.
func (d *Daemon) Reload(ctx context.Context) error {
	d.ops.Lock()
	defer d.ops.Unlock()
	f, err := config.Load(d.cfg.ConfigPath)
	if err != nil {
		return err
	}
	d.mesh.Configure(f.Node)
	d.config.Store(f)
	d.listen(f)
	var stop []*supervisor
	d.mu.Lock()
	for name, s := range d.links {
		if l := f.Links[name]; l == nil || *l != s.spec {
			stop = append(stop, s)
			delete(d.links, name)
		}
	}
	d.mu.Unlock()
	stopAll(stop)
	d.mu.Lock()
	defer d.mu.Unlock()
	for name, l := range f.Links {
		if d.links[name] == nil {
			s := newSupervisor(d, name, *l)
			d.links[name] = s
			go s.run()
			s.logf("started")
		}
	}
	return nil
}

// stopAll stops every link and listener.
func (d *Daemon) stopAll() {
	d.mu.Lock()
	all := make([]*supervisor, 0, len(d.links))
	for name, s := range d.links {
		all = append(all, s)
		delete(d.links, name)
	}
	listeners := slices.Collect(maps.Values(d.maps))
	if d.proxy != nil {
		listeners = append(listeners, d.proxy)
	}
	clear(d.maps)
	d.proxy = nil
	d.mu.Unlock()
	stopAll(all)
	for _, l := range listeners {
		l.close()
	}
}

func stopAll(list []*supervisor) {
	var wg sync.WaitGroup
	for _, s := range list {
		wg.Go(s.stop)
	}
	wg.Wait()
}
