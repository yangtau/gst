package mesh

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/direct"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

const (
	directTimeout = 15 * time.Second // to set up a direct link
	directRetry   = time.Minute      // first wait after a direct link fails or ends
	directMaxWait = 30 * time.Minute
	removeTimeout = 3 * time.Second // to tell a peer that its link is being removed
)

// A direct link is a link over UDP (internal/direct) between two nodes,
// which may have no SSH link between them. Nodes set one up when a
// connection first goes to a node, signaling through the mesh, so the
// connection that started it keeps its way, and the ones after it take
// the direct link. It runs like any other link, and when it fails,
// connections go over the other links again.
//
// Direct links are shortcuts: only the mesh can start one, and SSH links
// decide which machines belong to it. Losing SSH links does not change
// that, so a direct link outlives them, as when the machine between its
// two nodes sleeps. Removing an SSH link does: both of its ends then end
// the direct links to nodes that SSH links no longer lead to.

// EnableDirect lets this node open and accept direct links until ctx is
// done. addrs returns the local addresses to use.
func (n *Node) EnableDirect(ctx context.Context, addrs func() []netip.Addr) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.directCtx, n.directAddrs = ctx, addrs
}

// Unlink tells the node at the other end of the link called name, if the
// link runs, that the link is being removed, before it stops. A node that
// does not hear it keeps its direct links until they end by themselves.
func (n *Node) Unlink(ctx context.Context, name string) {
	n.mu.Lock()
	var s session
	for l := range n.links {
		if l.opened && !l.direct && l.name == name {
			s = l.session
		}
	}
	self := n.info.ID
	n.mu.Unlock()
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, removeTimeout)
	defer cancel()
	if conn, err := call(ctx, s, wire.MethodLinkRemove, wire.LinkRemove{Node: self, Link: name}, nil); err == nil {
		conn.Close()
	}
}

func (n *Node) serveLinkRemove(_ context.Context, p wire.LinkRemove) (any, error) {
	self := n.Self().ID
	n.endStrandedDirect(func(e wire.TopoEdge) bool { return e.From == p.Node && e.To == self && e.Link == p.Link })
	return nil, nil
}

// EndStrandedDirect ends the direct links to nodes that SSH links do not
// lead to, after this node removed a link.
func (n *Node) EndStrandedDirect() {
	n.endStrandedDirect(func(wire.TopoEdge) bool { return false })
}

// endStrandedDirect is EndStrandedDirect without the links that gone
// reports, which are about to stop.
func (n *Node) endStrandedDirect(gone func(wire.TopoEdge) bool) {
	var overSSH []wire.TopoEdge
	topo := n.Topology()
	for _, e := range topo.Edges {
		if !e.Direct && !gone(e) {
			overSSH = append(overSSH, e)
		}
	}
	reached := reach(overSSH, topo.Self)
	n.mu.Lock()
	defer n.mu.Unlock()
	for l := range n.links {
		if _, ok := reached[l.peer.ID]; l.direct && !ok {
			l.session.Close()
		}
	}
}

// directTry is a direct link being set up with a node.
type directTry struct {
	offering bool
	cancel   context.CancelFunc
}

// directWait is when to try a node again after a failure.
type directWait struct {
	next time.Time
	wait time.Duration
}

// startDirect begins a try with node id, which replaces any other. n.mu
// is held. The caller ends it with endDirect.
func (n *Node) startDirect(id string, offering bool) (context.Context, *directTry) {
	ctx, cancel := context.WithTimeout(n.directCtx, directTimeout)
	if old := n.directTries[id]; old != nil {
		old.cancel()
	}
	try := &directTry{offering: offering, cancel: cancel}
	n.directTries[id] = try
	return ctx, try
}

func (n *Node) endDirect(id string, try *directTry) {
	try.cancel()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.directTries[id] == try {
		delete(n.directTries, id)
	}
}

// backOff makes the node wait before trying node id again, longer after
// each failure in a row.
func (n *Node) backOff(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	w := n.directWaits[id]
	w.wait = min(max(2*w.wait, directRetry), directMaxWait)
	w.next = time.Now().Add(w.wait)
	n.directWaits[id] = w
}

// directLink returns the running direct link with node id; n.mu is held.
func (n *Node) directLink(id string) *link {
	for l := range n.links {
		if l.direct && l.peer.ID == id && !closed(l.session) {
			return l
		}
	}
	return nil
}

// tryDirect sets up a direct link with node id in the background, unless
// there is one, or it failed recently.
func (n *Node) tryDirect(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	switch {
	case n.directCtx == nil, id == n.info.ID, n.directTries[id] != nil, n.directLink(id) != nil:
		return
	case time.Now().Before(n.directWaits[id].next):
		return
	}
	ctx, try := n.startDirect(id, true)
	go n.offerDirect(ctx, id, try)
}

// offerDirect is the side of a direct link that starts it: it offers the
// link and connects to the node that answers.
func (n *Node) offerDirect(ctx context.Context, id string, try *directTry) {
	defer n.endDirect(id, try)
	conn, err := n.connectDirect(ctx, id)
	if err != nil {
		n.directFailed(ctx, id, err.Error())
		return
	}
	n.runDirect(id, conn, true, try)
}

// connectDirect offers a direct link to node id and connects to it. Its
// errors are logged, so they leave out what the other machine reports.
func (n *Node) connectDirect(ctx context.Context, id string) (*direct.Conn, error) {
	attempt, err := direct.New(n.directAddrs())
	if err != nil {
		return nil, errors.New("no local address")
	}
	defer attempt.Close()
	offer := attempt.Offer()
	offer.Node = n.Self().ID
	var answer wire.DirectOffer
	if err := n.Call(ctx, id, wire.MethodDirectOffer, offer, &answer); err != nil {
		return nil, errors.New("the other machine did not take the offer")
	}
	conn, err := attempt.Dial(ctx, answer)
	if err != nil {
		return nil, errors.New("no path")
	}
	return conn, nil
}

// directFailed logs why a try failed, unless a newer try replaced it,
// and waits before the next.
func (n *Node) directFailed(ctx context.Context, id, reason string) {
	if ctx.Err() == context.Canceled {
		return
	}
	n.log.Printf("direct link with %s: %s", n.nodeName(id), reason)
	n.backOff(id)
}

// serveOffer answers direct.offer: it sets up its end and connects when
// the other node does. When both nodes offer at once, the offer of the
// node with the lower id goes on.
func (n *Node) serveOffer(_ context.Context, offer wire.DirectOffer) (any, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	switch {
	case n.directCtx == nil:
		return nil, wire.Errorf(wire.CodeFailed, "direct links are off on %s", n.info.Name)
	case !config.ValidNodeID(offer.Node) || offer.Node == n.info.ID:
		return nil, wire.Errorf(wire.CodeInvalidParams, "invalid node in the offer")
	}
	if try := n.directTries[offer.Node]; try != nil && try.offering && n.info.ID < offer.Node {
		return nil, wire.Errorf(wire.CodeFailed, "%s is offering a direct link itself", n.info.Name)
	}
	attempt, err := direct.New(n.directAddrs())
	if err != nil {
		return nil, wire.Errorf(wire.CodeFailed, "%s has no local address for a direct link", n.info.Name)
	}
	// The other node has no direct link, or it would not offer one. If
	// this one thinks there is, that link is gone.
	if old := n.directLink(offer.Node); old != nil {
		old.session.Close()
	}
	answer := attempt.Offer()
	ctx, try := n.startDirect(offer.Node, false)
	go func() {
		defer n.endDirect(offer.Node, try)
		conn, err := attempt.Accept(ctx, offer)
		attempt.Close()
		if err != nil {
			n.directFailed(ctx, offer.Node, "no connection arrived")
			return
		}
		n.runDirect(offer.Node, conn, false, try)
	}()
	return answer, nil
}

// runDirect runs a direct link until it ends. The try ends once the link
// runs, so that there is always one or the other. The node that opened
// the link publishes it, as with links over SSH.
func (n *Node) runDirect(id string, conn *direct.Conn, opened bool, try *directTry) {
	n.mu.Lock()
	delete(n.directWaits, id)
	ctx := n.directCtx
	n.mu.Unlock()
	err := n.run(ctx, &link{session: conn, direct: true, opened: opened}, func(wire.NodeInfo) {
		n.endDirect(id, try)
		n.log.Printf("direct link with %s: up", n.nodeName(id))
	})
	if err != nil && ctx.Err() == nil {
		n.log.Printf("direct link with %s: ended", n.nodeName(id))
		n.backOff(id)
	}
}

// nodeName returns the name of node id, for the log.
func (n *Node) nodeName(id string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if r, ok := n.records[id]; ok && r.Node.Name != "" {
		return r.Node.Name
	}
	return id[:min(8, len(id))]
}
