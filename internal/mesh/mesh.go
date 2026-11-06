// Package mesh links gst daemons with each other. A link is a connection
// to another machine's daemon, which the daemon opens over SSH by running
// "gst peer-bridge" there. The two daemons shake hands with the gst
// protocol in the peer role, and the connection continues as a yamux
// session that carries one call per stream in both directions: who the
// other node is (node.info), the link-state records that give every node
// a view of the whole mesh (topology.sync), and calls relayed to other
// nodes along routes through it (relay.open). All of that stays on SSH.
// It sets up the virtual network, whose packets take a UDP path where the
// network allows one (direct.go) and the links where not (packets.go).
package mesh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Node is this machine in the mesh: its identity, its links, and what it
// knows of the other nodes.
type Node struct {
	software string
	log      *log.Logger     // the daemon's log
	ttl      time.Duration   // lifetime of this node's record
	update   chan struct{}   // asks Run to publish
	peer     wire.Server     // the methods that peers call
	life     context.Context // ends when Run returns
	stop     context.CancelFunc

	mu       sync.Mutex
	info     wire.NodeInfo // ID is empty until Configure gives the node an identity
	links    map[*link]struct{}
	records  map[string]wire.Record // by node id, this node's included
	changed  chan struct{}          // closed and replaced when records change
	seqFloor uint64                 // the highest Seq seen on this node's own record
	net      *wire.NetInfo          // this node's part in the virtual network; see SetNet

	// UDP paths; see EnableDirect. directCtx is nil while they are off.
	directCtx   context.Context
	directAddrs func() []netip.Addr
	directTries map[string]*directTry         // by node id
	directWaits map[string]directWait         // by node id
	directTrace map[string][]wire.DirectEvent // the latest try with each node

	// Packets of the virtual network; see packets.go.
	onPacket atomic.Pointer[func(node string, packet []byte)]
	packets  sync.Map // node id -> *packetPeer
}

// link is a running link: the peer session with another node.
type link struct {
	name    string // the link's name, for links that this node opened
	peer    wire.NodeInfo
	session session
	opened  bool // this node opened it
}

// New returns a node without an identity, which refuses links until
// Configure gives it one. status reports the daemon to the other nodes.
func New(software string, daemonLog *log.Logger, status func() wire.Status) *Node {
	life, stop := context.WithCancel(context.Background())
	n := &Node{
		life:     life,
		stop:     stop,
		software: software,
		log:      daemonLog,
		ttl:      30 * time.Minute,
		update:   make(chan struct{}, 1),
		links:    map[*link]struct{}{},
		records:  map[string]wire.Record{},
		changed:  make(chan struct{}),

		directTries: map[string]*directTry{},
		directWaits: map[string]directWait{},
		directTrace: map[string][]wire.DirectEvent{},
	}
	wire.Handle(&n.peer, wire.MethodNodeInfo, func(context.Context, struct{}) (any, error) { return n.Self(), nil })
	wire.HandleStream(&n.peer, wire.MethodTopologySync, n.serveSync)
	wire.HandleStream(&n.peer, wire.MethodRelayOpen, n.serveRelay)
	wire.Handle(&n.peer, wire.MethodDirectOffer, n.serveOffer)
	wire.Handle(&n.peer, wire.MethodLinkRemove, n.serveLinkRemove)
	wire.Handle(&n.peer, wire.MethodDirectTrace, n.serveTrace)
	wire.HandleStream(&n.peer, wire.MethodNetPackets, n.servePackets)
	if status != nil {
		wire.Handle(&n.peer, wire.MethodPeerStatus, func(context.Context, struct{}) (any, error) { return status(), nil })
	}
	return n
}

// Configure sets this node's identity. A new identity is a new machine: it
// forgets what the node knew and closes its links and UDP paths, which the
// other ends know by the old identity.
func (n *Node) Configure(c *config.Node) {
	defer n.Update()
	var info wire.NodeInfo
	if c != nil {
		info = wire.NodeInfo{ID: c.ID, Name: c.Name, Software: n.software}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if info.ID != n.info.ID {
		clear(n.records)
		n.seqFloor = 0
		n.net = nil
		n.notify()
		for l := range n.links {
			l.session.Close()
		}
		for _, try := range n.directTries {
			try.cancel()
		}
		n.packets.Range(func(_, value any) bool {
			if c := value.(*packetPeer).direct.Load(); c != nil {
				c.Close()
			}
			return true
		})
	}
	n.info = info
}

// Peer returns the methods that peers call, for the daemon to add to
// before the node runs.
func (n *Node) Peer() *wire.Server { return &n.peer }

// Self returns this node's identity; its ID is empty until Configure gives it one.
func (n *Node) Self() wire.NodeInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.info
}

// hello is this node's Hello for peer connections.
func (n *Node) hello() wire.Hello {
	return wire.Hello{Software: n.software, Role: wire.RolePeer, Features: []string{wire.FeatureYamux}}
}

// RemoteError is a failure that the other machine reported. Its text may
// quote that machine, so logs leave it out.
type RemoteError struct{ msg string }

func (e *RemoteError) Error() string { return e.msg }

// Accept serves a link that another node opened on conn, whose Hello was
// theirs, until the link ends or ctx is done.
func (n *Node) Accept(ctx context.Context, conn io.ReadWriteCloser, theirs wire.Hello) {
	defer conn.Close()
	var refusal *wire.Error
	switch {
	case n.Self().ID == "":
		refusal = wire.Errorf(wire.CodeFailed, "gst on this machine has no node identity yet")
	case !slices.Contains(theirs.Features, wire.FeatureYamux):
		refusal = wire.Errorf(wire.CodeUnsupportedRole, "the peer does not support yamux")
	}
	if wire.Answer(conn, theirs, n.hello(), refusal) != nil {
		return
	}
	yamuxed, err := yamux.Server(conn, yamuxConfig())
	if err != nil {
		return
	}
	if err := n.run(ctx, &link{session: session{yamuxed}}, nil); err != nil && ctx.Err() == nil {
		// The reason may quote the peer, so the log leaves it out.
		n.log.Print("incoming link ended")
	}
}

// handshakeTimeout bounds the start of the remote gst and its Hello.
const handshakeTimeout = 30 * time.Second

// Link runs the link called name over conn, which leads to the other
// node's daemon, until the link ends or ctx is done, and closes conn.
// onUp is called with the other node once the link runs. Link returns why
// the link ended, or nil when ctx is done. When conn has a Failure method,
// as an sshconn.Session does, what it returns explains a remote gst that
// did not answer.
func (n *Node) Link(ctx context.Context, name string, conn io.ReadWriteCloser, onUp func(peer wire.NodeInfo)) error {
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { conn.Close() })()

	// A remote gst that starts but never answers would leave the link
	// connecting forever.
	timeout := time.AfterFunc(handshakeTimeout, func() { conn.Close() })
	_, err := wire.Dial(conn, n.hello())
	if !timeout.Stop() && err == nil {
		err = errors.New("the peer handshake timed out")
	}
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		var refused *wire.Error
		if errors.As(err, &refused) {
			return &RemoteError{"the remote gst refused the link: " + refused.Message}
		}
		if failed, ok := conn.(interface{ Failure() string }); ok && failed.Failure() != "" {
			return &RemoteError{"the remote gst failed: " + failed.Failure()}
		}
		return errors.New("the remote gst did not answer; is gst 0.4 or later installed there?")
	}
	yamuxed, err := yamux.Client(conn, yamuxConfig())
	if err != nil {
		return err
	}
	return n.run(ctx, &link{name: name, opened: true, session: session{yamuxed}}, onUp)
}

// run serves the peer's calls, learns who the peer is, and keeps the link
// l until its session ends, which it reports as an error unless ctx is
// done.
func (n *Node) run(ctx context.Context, l *link, onUp func(wire.NodeInfo)) error {
	s := l.session
	defer s.Close()
	go n.serve(ctx, s)

	peer, err := n.nodeInfo(ctx, s)
	if err != nil {
		return err
	}
	if peer.ID == n.Self().ID {
		// The opening side reports this and closes the link; the
		// accepting side waits for that, so the report is not lost.
		if l.opened {
			return errors.New("the other machine is this machine")
		}
		<-s.done()
		return nil
	}
	l.peer = peer
	n.mu.Lock()
	n.links[l] = struct{}{}
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		delete(n.links, l)
		n.mu.Unlock()
		n.Update()
	}()
	n.Update()
	if onUp != nil {
		onUp(peer)
	}
	go n.sync(ctx, s)
	select {
	case <-ctx.Done():
		return nil
	case <-s.done():
		return errors.New("the link closed")
	}
}

func (n *Node) nodeInfo(ctx context.Context, s session) (wire.NodeInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	var info wire.NodeInfo
	conn, err := call(ctx, s, wire.MethodNodeInfo, nil, &info)
	if err != nil {
		var refused *wire.Error
		if errors.As(err, &refused) {
			return info, &RemoteError{"the peer did not say who it is: " + refused.Message}
		}
		return info, fmt.Errorf("ask the peer who it is: %w", err)
	}
	conn.Close()
	if !config.ValidNodeID(info.ID) {
		return info, errors.New("the peer sent no valid node id")
	}
	return info, nil
}

// serve answers the calls that the peer opens streams for, until the
// session ends. The context of each call ends with the session.
func (n *Node) serve(ctx context.Context, s session) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for {
		conn, err := s.accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			defer context.AfterFunc(ctx, func() { conn.Close() })()
			n.peer.Serve(ctx, conn)
		}()
	}
}

// sessionTo returns the session of a running link with node id.
func (n *Node) sessionTo(id string) (session, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for l := range n.links {
		if l.peer.ID == id && !l.session.IsClosed() {
			return l.session, true
		}
	}
	return session{}, false
}
