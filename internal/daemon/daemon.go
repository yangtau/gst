// Package daemon runs the gst daemon: one background process per user
// that is this machine's node in the mesh. It runs the links of the
// configuration file, accepts the links of other nodes, runs the
// virtual network, and answers gst commands on a Unix control socket
// using the gst protocol (internal/wire). Where a process cannot serve a
// socket, as on a phone, Run runs the same daemon without one, and Call
// answers the same methods in the process.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/mesh"
	"github.com/yangtau/ssh-tunnel/internal/tun"
	"github.com/yangtau/ssh-tunnel/internal/vnet"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Config configures a daemon.
type Config struct {
	ConfigPath string // the gst configuration file
	Socket     string // control socket path, for Serve
	// StateDir holds the table of the network and the keys of the virtual
	// network; the directory of Socket when empty.
	StateDir string
	Software string // reported in Hello and Status, such as "gst 0.5.0+a2ffc87"
	Build    string // identifies the executable; see Build

	// Connect opens a connection to the daemon on the node at the other
	// end of l, as gst peer-bridge there does; closing it ends everything
	// under it. Its errors are logged, so they must not quote addresses,
	// users, or paths.
	Connect func(ctx context.Context, l *config.Link) (io.ReadWriteCloser, error)
	// Addresses returns the local addresses that UDP paths use; they are
	// off when it is nil.
	Addresses func() []netip.Addr
	// OpenNet sets up the interface of the virtual network, which cannot
	// run when it is nil; NetDNS is the UDP address to answer for .gst on,
	// and UsedPrefixes what is in use on this machine, by default as
	// vnet.LocalPrefixes finds it.
	OpenNet func(tun.Spec) (vnet.Interface, error)
	NetDNS  string
	NetEcho int // the UDP port that answers gst ping
	// NetInside answers on NetDNS and NetEcho at the packets of the
	// interface rather than on sockets; see vnet.Config.Inside.
	NetInside    bool
	UsedPrefixes func(skip string) vnet.UsedPrefixes
	// RetryDelay is the first wait after a link fails, which doubles with
	// each failure up to maxRetry; 5 s when zero.
	RetryDelay time.Duration
	// Log receives the daemon's own log; the standard logger when nil.
	Log *log.Logger
}

// Daemon runs links, the virtual network, and control connections.
type Daemon struct {
	cfg Config

	mesh    *mesh.Node
	net     *vnet.Manager
	control wire.Server                 // the methods of control connections
	config  atomic.Pointer[config.File] // as the last reload read it

	ops     sync.Mutex             // serializes reloads and restarts
	stopped bool                   // Serve ended, and no link starts again; guarded by ops
	mu      sync.Mutex             // guards links
	links   map[string]*supervisor // by destination
}

// New returns a daemon for cfg.
func New(cfg Config) *Daemon {
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = 5 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	if cfg.StateDir == "" {
		cfg.StateDir = filepath.Dir(cfg.Socket)
	}
	d := &Daemon{cfg: cfg, links: map[string]*supervisor{}}
	m := mesh.New(mesh.Config{
		Software:       cfg.Software,
		Log:            cfg.Log,
		StatePath:      filepath.Join(cfg.StateDir, "state.json"),
		Status:         d.Status,
		SaveGeneration: d.saveGeneration,
	})
	d.mesh = m
	d.net = vnet.New(vnet.Config{
		Mesh:         m,
		Log:          cfg.Log,
		StatePath:    filepath.Join(cfg.StateDir, "net.json"),
		Open:         cfg.OpenNet,
		UsedPrefixes: cfg.UsedPrefixes,
		DNS:          cfg.NetDNS,
		Echo:         cfg.NetEcho,
		Inside:       cfg.NetInside,
	})
	d.config.Store(&config.File{})
	none := func(f func() any) func(context.Context, struct{}) (any, error) {
		return func(context.Context, struct{}) (any, error) { return f(), nil }
	}
	wire.Handle(&d.control, wire.MethodStatus, none(func() any { return d.Status() }))
	wire.Handle(&d.control, wire.MethodTopology, none(func() any { return m.Topology() }))
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
	wire.Handle(&d.control, wire.MethodNetPing, func(ctx context.Context, p wire.NetPingParams) (any, error) {
		return d.netPing(ctx, p.Node, p.Size)
	})
	wire.Handle(&d.control, wire.MethodDirect, func(ctx context.Context, p wire.NodeParams) (any, error) {
		r, err := m.DirectReport(ctx, p.Node)
		r.Stats.InboundDrops = d.net.Dropped()
		return r, err
	})
	wire.Handle(&d.control, wire.MethodRemove, func(ctx context.Context, p wire.NodeParams) (any, error) {
		return m.Remove(p.Node)
	})
	wire.Handle(&d.control, wire.MethodSubnet, func(ctx context.Context, p wire.SubnetParams) (any, error) {
		subnet, err := config.ParseSubnet(p.Subnet)
		if err != nil {
			return nil, wire.Errorf(wire.CodeInvalidParams, "%v", err)
		}
		return struct{}{}, m.SetSubnet(subnet.String(), true)
	})
	mesh.Handle(m, wire.MethodNetPrefixes, func(context.Context, struct{}) (any, error) { return d.net.UsedPrefixes(), nil })
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
	defer d.start(ctx)()

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

// Run starts what the configuration asks for and keeps it until ctx is
// done, when it stops everything. It serves no socket: Call answers the
// control methods, and no other node can link to this one.
func (d *Daemon) Run(ctx context.Context) {
	stop := d.start(ctx)
	<-ctx.Done()
	stop()
}

// Call answers a call of a control method in this process, as a control
// connection would.
func (d *Daemon) Call(ctx context.Context, req wire.Request) wire.Response {
	return d.control.Call(ctx, req)
}

// start starts the mesh, the links of the configuration, and the virtual
// network. The function it returns stops them all and waits for that.
func (d *Daemon) start(ctx context.Context) (stop func()) {
	meshCtx, stopMesh := context.WithCancel(ctx)
	go d.mesh.Run(meshCtx)
	if d.cfg.Addresses != nil {
		d.mesh.EnableDirect(meshCtx, d.cfg.Addresses)
	}

	if err := d.Reload(ctx); err != nil {
		// Keep going: a later reload, after the configuration is
		// fixed, can still start the links.
		d.cfg.Log.Print(err)
	}
	go d.dropRemovedLinks(meshCtx)
	go d.retryWhenMoved(meshCtx)
	netDone := make(chan struct{})
	go func() {
		defer close(netDone)
		d.net.Run(meshCtx)
	}()
	d.cfg.Log.Printf("%s daemon ready", d.cfg.Software)
	return func() {
		stopMesh()
		<-netDone
		d.stopAll()
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

// Status reports the daemon and what it runs, sorted.
func (d *Daemon) Status() wire.Status {
	f := d.config.Load()
	st := wire.Status{
		Software: d.cfg.Software, Build: d.cfg.Build, PID: os.Getpid(), Config: d.cfg.ConfigPath,
		Node: d.mesh.Self(), Links: []wire.LinkState{}, Net: d.net.Status(), Exposes: d.net.Exposes(),
		Removed: d.mesh.Removed(),
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range f.Links {
		state := wire.LinkState{Destination: l.Destination, State: wire.StateDown}
		if s := d.links[l.Destination]; s != nil {
			state = s.state()
		}
		state.SSHArgs = l.SSHArgs
		st.Links = append(st.Links, state)
	}
	return st
}

// netPing sends a packet to the node that ref names and back, over the
// virtual network, and reports the way it went.
func (d *Daemon) netPing(ctx context.Context, ref string, size int) (wire.NetPing, error) {
	if size < 0 || size > vnet.MaxPingSize {
		return wire.NetPing{}, wire.Errorf(wire.CodeInvalidParams, "the size must be at most %d", vnet.MaxPingSize)
	}
	topo := d.mesh.Topology()
	dest, err := mesh.FindNode(topo, ref)
	if err != nil {
		return wire.NetPing{}, err
	}
	if !dest.Online() {
		return wire.NetPing{}, wire.Errorf(wire.CodeUnreachable, "%s is offline", dest.Name)
	}
	if st := d.net.Status(); st == nil || st.Interface == "" {
		return wire.NetPing{}, wire.Errorf(wire.CodeFailed, "this machine is not on the gst network; run `gst up`")
	}
	if dest.Net == nil {
		return wire.NetPing{}, wire.Errorf(wire.CodeFailed, "%s is not on the gst network; run `gst up` there", dest.Name)
	}
	rtt, err := d.net.Ping(ctx, dest.Net.Address, size)
	if err != nil {
		return wire.NetPing{}, wire.Errorf(wire.CodeFailed, "%v", err)
	}
	p := wire.NetPing{
		Node:    dest.Info(),
		Address: dest.Net.Address,
		Via:     []wire.NodeInfo{},
		TimeMS:  float64(rtt.Microseconds()) / 1000,
	}
	if dest.ID != topo.Self {
		// The path now is the one the reply took.
		if p.Way = d.mesh.PacketWay(dest.ID); p.Way == wire.WayRelay {
			p.Via = mesh.Via(topo, dest)
		}
		p.Endpoint = d.mesh.PacketEndpoint(dest.ID)
	}
	return p, nil
}

// Reload reads the configuration and applies it: the node's identity
// and the exposed ports; new links start, and links that are gone or
// changed stop, the changed ones to start again, and so do the links
// added again.
func (d *Daemon) Reload(ctx context.Context) error {
	d.ops.Lock()
	defer d.ops.Unlock()
	if d.stopped {
		return nil
	}
	f, err := config.Load(d.cfg.ConfigPath)
	if err != nil {
		return err
	}
	d.config.Store(f)
	d.mesh.Configure(f.Node)
	d.net.Configure(ctx, f.Exposes...)
	var stop []*supervisor
	d.mu.Lock()
	for destination, s := range d.links {
		// A link added again restarts, so that it admits its machine.
		if l := f.Link(destination); l == nil || !slices.Equal(l.SSHArgs, s.spec.SSHArgs) || l.Admit && !s.spec.Admit {
			stop = append(stop, s)
			delete(d.links, destination)
		}
	}
	d.mu.Unlock()
	stopAll(stop)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range f.Links {
		if d.links[l.Destination] == nil {
			s := newSupervisor(d, l)
			d.links[l.Destination] = s
			go s.run()
			s.logf("started")
		}
	}
	return nil
}

// admitted reports whether the link to destination is one that a person
// added and that has not run yet.
func (d *Daemon) admitted(destination string) bool {
	l := d.config.Load().Link(destination)
	return l != nil && l.Admit
}

// linkUp keeps in the configuration which node the link to
// destination reached, and that it ran. A link that a person added
// replaces the others to the same node.
func (d *Daemon) linkUp(destination string, peer wire.NodeInfo) {
	known := func(l *config.Link) bool { return l == nil || l.Target == peer.ID && !l.Admit }
	if known(d.config.Load().Link(destination)) {
		return
	}
	changed, dropped := false, false
	err := config.Update(d.cfg.ConfigPath, func(f *config.File) error {
		l := f.Link(destination)
		if known(l) {
			return nil
		}
		changed = true
		other := func(o config.Link) bool { return o.Destination != destination && o.Target == peer.ID }
		switch {
		case l.Admit:
			// A person added it: it replaces the link that reached the
			// node before.
			f.Links = slices.DeleteFunc(f.Links, other)
			l = f.Link(destination)
		case slices.ContainsFunc(f.Links, other):
			// One link each way: the one that reached the node first
			// stays.
			f.Links = slices.DeleteFunc(f.Links, func(o config.Link) bool { return o.Destination == destination })
			dropped = true
			return nil
		}
		l.Target, l.Admit = peer.ID, false
		return nil
	})
	if dropped {
		d.cfg.Log.Printf("link %s: another link reaches the same machine; dropped this one", config.LinkID(destination))
	}
	if err != nil {
		d.cfg.Log.Printf("link %s: keep its machine: %v", config.LinkID(destination), err)
		return
	}
	if changed {
		d.reloadLater()
	}
}

// saveGeneration keeps the generation of this node in the configuration.
func (d *Daemon) saveGeneration(gen int64) error {
	self := d.mesh.Self().ID
	return config.Update(d.cfg.ConfigPath, func(f *config.File) error {
		if f.Node != nil && f.Node.ID == self {
			f.Node.Generation = max(f.Node.Generation, gen)
		}
		return nil
	})
}

// dropRemovedLinks deletes from the configuration the links to nodes
// removed from the network, and all links when it is this node, until
// ctx is done. Links that a person added since stay: they bring the
// nodes back.
func (d *Daemon) dropRemovedLinks(ctx context.Context) {
	for {
		changed := d.mesh.Changed()
		removed := d.mesh.Removed() != nil
		gone := func(l config.Link) bool {
			return !l.Admit && (removed || l.Target != "" && d.mesh.IsRemoved(l.Target))
		}
		if slices.ContainsFunc(d.config.Load().Links, gone) {
			err := config.Update(d.cfg.ConfigPath, func(f *config.File) error {
				f.Links = slices.DeleteFunc(f.Links, gone)
				return nil
			})
			if err != nil {
				d.cfg.Log.Printf("drop the links to removed machines: %v", err)
			} else {
				d.cfg.Log.Print("dropped the links to removed machines")
				d.reloadLater()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

// retryWhenMoved makes the links that wait to try again try at once
// whenever this machine moves to another network, until ctx is done: what
// kept them from connecting may be gone, and a machine that moves often,
// as a phone does, would otherwise wait out the longest delay each time.
func (d *Daemon) retryWhenMoved(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.mesh.Moved():
		}
		d.mu.Lock()
		for _, s := range d.links {
			s.retry()
		}
		d.mu.Unlock()
	}
}

// reloadLater reloads the configuration in the background, after the
// daemon changed it.
func (d *Daemon) reloadLater() {
	go func() {
		if err := d.Reload(context.Background()); err != nil {
			d.cfg.Log.Print(err)
		}
	}()
}

// stopAll stops every link for good: a reload in the background, such
// as after a link ran, starts none again.
func (d *Daemon) stopAll() {
	d.ops.Lock()
	d.stopped = true
	d.ops.Unlock()
	d.mu.Lock()
	all := make([]*supervisor, 0, len(d.links))
	for destination, s := range d.links {
		all = append(all, s)
		delete(d.links, destination)
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
