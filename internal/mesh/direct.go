package mesh

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/direct"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

const (
	directTimeout = 15 * time.Second // to find a UDP path
	directRetry   = 5 * time.Second  // first wait after a try fails or a path ends
	directMaxWait = 5 * time.Minute
	removeTimeout = 3 * time.Second // to tell a peer that its link is being removed
	maxTrace      = 200             // events kept of a try
)

// A UDP path (internal/direct) carries the WireGuard packets between two
// nodes, which may have no SSH link between them. Traffic to a node starts
// a try at one, signaling over the mesh, and takes the links until the
// path is up, and again after it ends.
//
// A path is no link: no calls run on it, and the records say nothing of
// it. It is a shortcut for packets, and only the mesh can start one, as
// SSH links decide which machines belong to it. Losing SSH links does not
// change that, so a path outlives them, as when the machine between its
// two nodes sleeps. Removing an SSH link does: both of its ends then end
// the paths to nodes that SSH links no longer lead to.

// EnableDirect lets this node find and accept UDP paths until ctx is
// done. addrs returns the local addresses to use.
func (n *Node) EnableDirect(ctx context.Context, addrs func() []netip.Addr) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.directCtx, n.directAddrs = ctx, addrs
}

// Unlink tells the node at the other end of the link called name, if the
// link runs, that the link is being removed, before it stops. A node that
// does not hear it keeps its UDP paths until they end by themselves.
func (n *Node) Unlink(ctx context.Context, name string) {
	n.mu.Lock()
	var s session
	found := false
	for l := range n.links {
		if l.opened && l.name == name {
			s, found = l.session, true
		}
	}
	self := n.info.ID
	n.mu.Unlock()
	if !found {
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

// EndStrandedDirect ends the UDP paths to nodes that SSH links do not lead
// to, after this node removed a link.
func (n *Node) EndStrandedDirect() {
	n.endStrandedDirect(func(wire.TopoEdge) bool { return false })
}

// endStrandedDirect is EndStrandedDirect without the links that gone
// reports, which are about to stop.
func (n *Node) endStrandedDirect(gone func(wire.TopoEdge) bool) {
	topo := n.Topology()
	reached := reach(slices.DeleteFunc(slices.Clone(topo.Edges), gone), topo.Self)
	n.packets.Range(func(id, value any) bool {
		if _, ok := reached[id.(string)]; !ok {
			if c := value.(*packetPeer).direct.Load(); c != nil {
				c.Close()
			}
		}
		return true
	})
}

// trace records what the try with node id did. The trace holds addresses,
// so it goes only to the user who asks for it, never to the log.
func (n *Node) trace(id, format string, args ...any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.directTrace[id]) < maxTrace {
		n.directTrace[id] = append(n.directTrace[id], wire.DirectEvent{Time: time.Now(), Text: fmt.Sprintf(format, args...)})
	}
}

func (n *Node) serveTrace(_ context.Context, p wire.DirectTraceParams) (any, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return wire.DirectTrace{Events: slices.Clone(n.directTrace[p.Node])}, nil
}

// DirectReport reports the UDP path to the machine that ref names, and
// what this node and that machine did in their latest try at one.
func (n *Node) DirectReport(ctx context.Context, ref string) (wire.DirectReport, error) {
	dest, err := FindNode(n.Topology(), ref)
	if err != nil {
		return wire.DirectReport{}, err
	}
	if dest.ID == n.Self().ID {
		return wire.DirectReport{}, wire.Errorf(wire.CodeInvalidParams, "%s is this machine", dest.Name)
	}
	r := wire.DirectReport{Node: wire.NodeInfo{ID: dest.ID, Name: dest.Name, Software: dest.Software}}
	if err := n.Call(ctx, dest.ID, wire.MethodDirectTrace, wire.DirectTraceParams{Node: n.Self().ID}, &r.Peer); err != nil {
		r.PeerError = err.Error()
	}
	if c := n.packetPeer(dest.ID).direct.Load(); c != nil {
		r.Path = &wire.DirectPath{Local: c.Local(), Remote: c.Remote()}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	r.Off = n.directCtx == nil
	r.Trying = n.directTries[dest.ID] != nil
	if next := n.directWaits[dest.ID].next; next.After(time.Now()) {
		r.Next = next
	}
	r.Local.Events = slices.Clone(n.directTrace[dest.ID])
	return r, nil
}

// directTry is a try at a UDP path to a node.
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
	n.directTrace[id] = nil
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

// tryDirect looks for a UDP path to node id in the background, unless
// there is one, or a try failed recently.
func (n *Node) tryDirect(id string) {
	path := n.packetPeer(id).direct.Load()
	n.mu.Lock()
	defer n.mu.Unlock()
	switch {
	case n.directCtx == nil, id == n.info.ID, n.directTries[id] != nil, path != nil, n.onPacket.Load() == nil:
		return
	case time.Now().Before(n.directWaits[id].next):
		return
	}
	ctx, try := n.startDirect(id, true)
	go n.offerDirect(ctx, id, try)
}

// offerDirect is the side of a try that starts it: it offers a path and
// finds one to the node that answers.
func (n *Node) offerDirect(ctx context.Context, id string, try *directTry) {
	defer n.endDirect(id, try)
	conn, err := n.connectDirect(ctx, id)
	if err != nil {
		n.directFailed(ctx, id, err.Error())
		return
	}
	n.runDirect(id, conn, try)
}

// connectDirect offers a path to node id and finds one. Its errors are
// logged, so they leave out what the other machine reports.
func (n *Node) connectDirect(ctx context.Context, id string) (*direct.Conn, error) {
	attempt, err := direct.New(n.directAddrs())
	if err != nil {
		return nil, errors.New("no local address")
	}
	defer attempt.Close()
	attempt.Trace = func(text string) { n.trace(id, "%s", text) }
	offer := attempt.Offer()
	offer.Node = n.Self().ID
	n.trace(id, "offering over SSH: %s", strings.Join(offer.Addresses, ", "))
	var answer wire.DirectOffer
	if err := n.Call(ctx, id, wire.MethodDirectOffer, offer, &answer); err != nil {
		n.trace(id, "the offer failed: %v", err)
		return nil, errors.New("the other machine did not take the offer")
	}
	n.trace(id, "the answer offers: %s", strings.Join(answer.Addresses, ", "))
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
	n.log.Printf("UDP path to %s: %s", n.nodeName(id), reason)
	n.trace(id, "failed: %s", reason)
	n.backOff(id)
}

// serveOffer answers direct.offer: it helps the other node find a path.
// When both nodes offer at once, the offer of the node with the lower id
// goes on.
func (n *Node) serveOffer(_ context.Context, offer wire.DirectOffer) (any, error) {
	if n.onPacket.Load() == nil {
		return nil, wire.Errorf(wire.CodeFailed, "%s is not on the virtual network", n.Self().Name)
	}
	if !config.ValidNodeID(offer.Node) {
		return nil, wire.Errorf(wire.CodeInvalidParams, "invalid node in the offer")
	}
	old := n.packetPeer(offer.Node).direct.Load()
	n.mu.Lock()
	defer n.mu.Unlock()
	switch {
	case n.directCtx == nil:
		return nil, wire.Errorf(wire.CodeFailed, "UDP paths are off on %s", n.info.Name)
	case offer.Node == n.info.ID:
		return nil, wire.Errorf(wire.CodeInvalidParams, "invalid node in the offer")
	}
	if try := n.directTries[offer.Node]; try != nil && try.offering && n.info.ID < offer.Node {
		return nil, wire.Errorf(wire.CodeFailed, "%s is offering a path itself", n.info.Name)
	}
	attempt, err := direct.New(n.directAddrs())
	if err != nil {
		return nil, wire.Errorf(wire.CodeFailed, "%s has no local address for a path", n.info.Name)
	}
	// The other node has no path, or it would not offer one. If this one
	// thinks there is, that path is gone.
	if old != nil {
		old.Close()
	}
	answer := attempt.Offer()
	ctx, try := n.startDirect(offer.Node, false)
	go func() {
		defer n.endDirect(offer.Node, try)
		attempt.Trace = func(text string) { n.trace(offer.Node, "%s", text) }
		n.trace(offer.Node, "offer over SSH: %s", strings.Join(offer.Addresses, ", "))
		n.trace(offer.Node, "answering with: %s", strings.Join(answer.Addresses, ", "))
		conn, err := attempt.Accept(ctx, offer)
		attempt.Close()
		if err != nil {
			n.directFailed(ctx, offer.Node, "no path")
			return
		}
		n.runDirect(offer.Node, conn, try)
	}()
	return answer, nil
}

// runDirect makes packets to node id take conn until it ends. The try
// ends once the path runs, so that there is always one or the other.
func (n *Node) runDirect(id string, conn *direct.Conn, try *directTry) {
	n.mu.Lock()
	delete(n.directWaits, id)
	ctx := n.directCtx
	n.mu.Unlock()
	n.attachPackets(id, conn)
	defer n.detachPackets(id, conn)
	defer conn.Close()
	n.endDirect(id, try)
	n.log.Printf("UDP path to %s: up", n.nodeName(id))
	n.trace(id, "up: %s -> %s", conn.Local(), conn.Remote())
	n.Update()
	select {
	case <-ctx.Done():
	case <-conn.Done():
		n.log.Printf("UDP path to %s: ended", n.nodeName(id))
		n.trace(id, "ended")
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
