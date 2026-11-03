// Package mesh links gst daemons with each other. A tunnel with mesh on
// runs "gst peer-bridge" on its server over the tunnel's SSH connection;
// the bridge connects to the server's daemon, the two daemons shake hands
// with the gst protocol in the peer role, and the connection continues as
// a yamux session that carries one call per stream in both directions.
package mesh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Node is this machine's place in the mesh: its identity, its tunnels with
// mesh on, its peer links, and what it knows of the other nodes.
type Node struct {
	software     string
	log          *log.Logger // the daemon's log
	retry        time.Duration
	ttl          time.Duration // lifetime of this node's record
	hostKeyFiles string        // glob of the sshd host public keys
	update       chan struct{} // asks Run to publish
	peer         wire.Server   // the methods that peers call

	mu        sync.Mutex
	info      wire.NodeInfo // ID is empty while the mesh is disabled
	tunnels   map[*Tunnel]struct{}
	links     map[*link]struct{}
	records   map[string]wire.Record // by node id, this node's included
	changed   chan struct{}          // closed and replaced when records change
	seqFloor  uint64                 // the highest Seq seen on this node's own record
	republish bool                   // publish even if the record did not change
}

// New returns a node, disabled until Configure enables it.
func New(software string, daemonLog *log.Logger) *Node {
	n := &Node{
		software:     software,
		log:          daemonLog,
		retry:        10 * time.Second,
		ttl:          30 * time.Minute,
		hostKeyFiles: "/etc/ssh/ssh_host_*_key.pub",
		update:       make(chan struct{}, 1),
		tunnels:      map[*Tunnel]struct{}{},
		links:        map[*link]struct{}{},
		records:      map[string]wire.Record{},
		changed:      make(chan struct{}),
	}
	wire.Handle(&n.peer, wire.MethodNodeInfo, func(context.Context, struct{}) (any, error) { return n.self(), nil })
	wire.HandleStream(&n.peer, wire.MethodTopologySync, n.serveSync)
	wire.HandleStream(&n.peer, wire.MethodRelayOpen, n.serveRelay)
	return n
}

// SetRetry sets the wait before an outgoing link that failed retries.
func (n *Node) SetRetry(d time.Duration) { n.retry = d }

// SetHostKeyFiles sets the glob of this machine's sshd host public keys,
// whose fingerprints identify it as a tunnel's server.
func (n *Node) SetHostKeyFiles(glob string) { n.hostKeyFiles = glob }

// Configure applies the mesh settings; nil disables the mesh and closes
// every link.
func (n *Node) Configure(m *config.Mesh) {
	defer n.Update()
	var hostKeys []string
	if m != nil {
		hostKeys = readHostKeys(n.hostKeyFiles)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if m == nil || m.ID != n.info.ID {
		clear(n.records)
		n.seqFloor = 0
		n.notify()
	}
	if m == nil {
		n.info = wire.NodeInfo{}
		for l := range n.links {
			l.closeSession()
		}
		return
	}
	n.info = wire.NodeInfo{ID: m.ID, Name: m.Name, Software: n.software, HostKeys: hostKeys}
}

func (n *Node) self() wire.NodeInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.info
}

// Enabled reports whether this machine takes part in the mesh.
func (n *Node) Enabled() bool { return n.self().ID != "" }

var errDisabled = wire.Errorf(wire.CodeFailed, "gst mesh is not enabled on this machine; run `gst mesh enable`")

// Peers reports this node and its links.
func (n *Node) Peers() wire.Peers {
	n.mu.Lock()
	p := wire.Peers{Enabled: n.info.ID != "", Node: n.info, Links: []wire.PeerLink{}}
	links := make([]*link, 0, len(n.links))
	for l := range n.links {
		links = append(links, l)
	}
	n.mu.Unlock()
	for _, l := range links {
		p.Links = append(p.Links, l.state())
	}
	slices.SortFunc(p.Links, func(a, b wire.PeerLink) int {
		return strings.Compare(a.Direction+a.Tunnel+a.Peer.Name+a.Peer.ID, b.Direction+b.Tunnel+b.Peer.Name+b.Peer.ID)
	})
	return p
}

// hello is this node's Hello for peer connections.
func (n *Node) hello() wire.Hello {
	return wire.Hello{Software: n.software, Role: wire.RolePeer, Features: []string{wire.FeatureYamux}}
}

// Accept serves a peer link on conn, whose other end sent theirs to open
// it, until the link ends or ctx is done. It answers theirs, refusing
// the link while the mesh is disabled.
func (n *Node) Accept(ctx context.Context, conn io.ReadWriteCloser, theirs wire.Hello) {
	var refusal *wire.Error
	switch {
	case !n.Enabled():
		refusal = wire.Errorf(wire.CodeUnsupportedRole, "gst mesh is not enabled on this machine; run `gst mesh enable` there")
	case !slices.Contains(theirs.Features, wire.FeatureYamux):
		refusal = wire.Errorf(wire.CodeUnsupportedRole, "the peer does not support yamux")
	}
	if wire.Answer(conn, theirs, n.hello(), refusal) != nil {
		conn.Close()
		return
	}
	session, err := yamux.Server(conn, yamuxConfig())
	if err != nil {
		conn.Close()
		return
	}
	l := n.add(wire.LinkIn, "")
	defer n.remove(l)
	// The reason may quote the peer, so the log leaves it out.
	if err := n.run(ctx, l, session, nil); err != nil && ctx.Err() == nil && n.Enabled() {
		n.log.Print("incoming peer link ended")
	}
}

// handshakeTimeout bounds the start of the remote gst and its Hello.
const handshakeTimeout = 30 * time.Second

var errNoAnswer = errors.New("the remote gst did not answer; is gst 0.2 or later installed there? See --remote-gst")

// linkOver runs the connecting side of a peer link over conn, which leads
// to the peer's daemon, and closes conn when done.
func (n *Node) linkOver(ctx context.Context, l *link, conn io.ReadWriteCloser, onUp func()) error {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	// A remote gst that starts but never answers would hold the link in
	// connecting forever.
	timeout := time.AfterFunc(handshakeTimeout, func() { conn.Close() })
	hello, err := wire.Dial(conn, n.hello())
	if !timeout.Stop() && err == nil {
		err = errors.New("handshake timed out")
	}
	if err != nil {
		var refused *wire.Error
		if errors.As(err, &refused) {
			return fmt.Errorf("the remote gst refused the link: %s", refused.Message)
		}
		return errNoAnswer
	}
	if !slices.Contains(hello.Features, wire.FeatureYamux) {
		return errors.New("the remote gst does not support yamux")
	}
	mux, err := yamux.Client(conn, yamuxConfig())
	if err != nil {
		return err
	}
	return n.run(ctx, l, mux, onUp)
}

// run serves the peer's calls, learns who the peer is, and waits until the
// session ends, which it reports as an error unless ctx is done.
func (n *Node) run(ctx context.Context, l *link, session *yamux.Session, onUp func()) error {
	defer session.Close()
	l.setSession(session)
	go n.serve(ctx, session)

	peer, err := n.nodeInfo(ctx, session)
	if err != nil {
		return err
	}
	if peer.ID == n.self().ID {
		// The connecting side reports this and closes the link; the
		// accepting side waits for that, so the report is not lost.
		if l.state().Direction == wire.LinkOut {
			return errors.New("the server is this machine")
		}
		<-session.CloseChan()
		return nil
	}
	l.set(wire.LinkUp, peer, "")
	n.Update()
	if onUp != nil {
		onUp()
	}
	go n.sync(ctx, session)
	select {
	case <-ctx.Done():
		return nil
	case <-session.CloseChan():
		return errors.New("the peer link closed")
	}
}

func (n *Node) nodeInfo(ctx context.Context, session *yamux.Session) (wire.NodeInfo, error) {
	stream, err := session.OpenStream()
	if err != nil {
		return wire.NodeInfo{}, err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	stream.SetDeadline(time.Now().Add(30 * time.Second))
	var info wire.NodeInfo
	if err := wire.Call(stream, wire.MethodNodeInfo, nil, &info); err != nil {
		return info, fmt.Errorf("ask the peer who it is: %w", err)
	}
	if info.ID == "" {
		return info, errors.New("the peer sent no node id")
	}
	return info, nil
}

// serve answers the calls that the peer opens streams for, until the
// session ends. The context of each call ends with the session.
func (n *Node) serve(ctx context.Context, session *yamux.Session) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		go func() {
			conn := streamConn{stream, session}
			defer conn.Close()
			defer context.AfterFunc(ctx, func() { conn.Close() })()
			n.peer.Serve(ctx, conn)
		}()
	}
}

func (n *Node) add(direction, tunnel string) *link {
	l := &link{info: wire.PeerLink{Direction: direction, Tunnel: tunnel, State: wire.LinkConnecting, Since: time.Now()}}
	n.mu.Lock()
	n.links[l] = struct{}{}
	n.mu.Unlock()
	return l
}

func (n *Node) remove(l *link) {
	n.mu.Lock()
	delete(n.links, l)
	n.mu.Unlock()
}

func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.ConnectionWriteTimeout = 30 * time.Second
	return cfg
}

// link is one peer link and its state.
type link struct {
	mu       sync.Mutex
	info     wire.PeerLink
	lastPeer string // the id of the last peer, kept while the link is down
	session  *yamux.Session
}

func (l *link) set(state string, peer wire.NodeInfo, errText string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.info.State != state {
		l.info.State, l.info.Since = state, time.Now()
	}
	l.info.Peer, l.info.Error = peer, errText
	if peer.ID != "" {
		l.lastPeer = peer.ID
	}
}

func (l *link) setSession(s *yamux.Session) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.session = s
}

func (l *link) closeSession() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.session != nil {
		l.session.Close()
	}
}

// sessionTo returns the session if the link is up with node id.
func (l *link) sessionTo(id string) *yamux.Session {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.info.State == wire.LinkUp && l.info.Peer.ID == id && l.session != nil && !l.session.IsClosed() {
		return l.session
	}
	return nil
}

// peer returns the id of the link's peer while the link is up or, when
// it is not, of its last peer, and whether the link is up.
func (l *link) peer() (id string, up bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.info.State == wire.LinkUp {
		return l.info.Peer.ID, true
	}
	return l.lastPeer, false
}

func (l *link) state() wire.PeerLink {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.info
}
