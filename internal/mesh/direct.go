package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/direct"
	"github.com/yangtau/ssh-tunnel/internal/mesh/state"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

const (
	directTimeout = 15 * time.Second // to find a UDP path
	directRetry   = 5 * time.Second  // first wait after a try fails or a path falls silent
	directMaxWait = 5 * time.Minute
	directIdle    = 45 * time.Second // a path that this node sends nothing on for this long ends; see runDirect
	maxTrace      = 200              // latest events kept of a try
)

// A UDP path (internal/direct) carries the WireGuard packets between two
// nodes of the network, which may have no SSH link between them.
// Traffic to a node starts a try at one, signaling with routed calls, and
// takes the links until the path is up, and again after it ends.
//
// A path is no link: it carries only the packets of its two nodes, no
// calls, no table, and nobody else's packets, and the table says nothing
// of it. It is a shortcut for packets between nodes of the network, so it outlives
// the links, as when the machine between its two nodes sleeps, and ends
// when the other node is removed from the network.

// EnableDirect lets this node find and accept UDP paths until ctx is
// done. addrs returns the local addresses to offer.
func (n *Node) EnableDirect(ctx context.Context, addrs func() []netip.Addr) {
	udp, err := direct.Listen(direct.Port, addrs, n.inSubnet)
	if err != nil {
		n.log.Print("UDP paths are off: no socket")
		return
	}
	context.AfterFunc(ctx, udp.Close)
	n.mu.Lock()
	defer n.mu.Unlock()
	n.directCtx, n.udp = ctx, udp
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-udp.Changed():
				n.addressesChanged(ctx)
			}
		}
	}()
}

// addressesChanged starts over after this machine moved to another network:
// what failed may work now, and what was measured no longer holds. Each
// running path measures again, and the other side learns the addresses.
func (n *Node) addressesChanged(ctx context.Context) {
	n.mu.Lock()
	clear(n.directWaits)
	n.mu.Unlock()
	n.packets.Range(func(key, value any) bool {
		id := key.(string)
		path := value.(*packetPeer).direct.Load()
		if path == nil {
			return true
		}
		n.trace(id, "this machine's addresses changed")
		path.Remeasure()
		offer := path.Offer()
		go func() {
			ctx, cancel := context.WithTimeout(ctx, directTimeout)
			defer cancel()
			if err := n.Call(ctx, id, wire.MethodDirectUpdate, offer, nil); err != nil {
				n.trace(id, "the update failed: %v", err)
			}
		}()
		return true
	})
}

// serveUpdate answers direct.update: the other machine moved.
func (n *Node) serveUpdate(_ context.Context, from string, offer wire.DirectOffer) (any, error) {
	if path := n.packetPeer(from).direct.Load(); path != nil {
		if err := path.Refresh(offer); err != nil {
			return nil, wire.Errorf(wire.CodeInvalidParams, "%v", err)
		}
		n.trace(from, "update over SSH: %s", strings.Join(offer.Addresses, ", "))
	}
	return struct{}{}, nil
}

// inSubnet reports whether addr is in the virtual network. Such an
// address is no way to a node: packets to it would take the very path
// that they make up.
func (n *Node) inSubnet(addr netip.Addr) bool {
	subnet := n.subnetPrefix.Load()
	return subnet != nil && subnet.Contains(addr)
}

// closeDirect ends the UDP paths, and the tries at one, to the nodes
// that gone reports. n.mu is held.
func (n *Node) closeDirect(gone func(id string) bool) {
	for id, try := range n.directTries {
		if gone(id) {
			try.cancel()
		}
	}
	n.packets.Range(func(id, value any) bool {
		if c := value.(*packetPeer).direct.Load(); c != nil && gone(id.(string)) {
			c.Close()
		}
		return true
	})
}

// trace records what the try with node id did. The trace holds addresses,
// so it goes only to the user who asks for it, never to the log.
func (n *Node) trace(id, format string, args ...any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	events := n.directTrace[id]
	if len(events) >= maxTrace {
		events = slices.Delete(events, 0, len(events)-maxTrace+1)
	}
	n.directTrace[id] = append(events, wire.DirectEvent{Time: time.Now(), Text: fmt.Sprintf(format, args...)})
}

func (n *Node) serveTrace(_ context.Context, from string, _ struct{}) (any, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return wire.DirectTrace{Events: slices.Clone(n.directTrace[from])}, nil
}

// DirectReport reports the UDP path to the node that ref names, and
// what this node and that node did in their latest try at one.
func (n *Node) DirectReport(ctx context.Context, ref string) (wire.DirectReport, error) {
	dest, err := FindNode(n.Topology(), ref)
	if err != nil {
		return wire.DirectReport{}, err
	}
	if dest.ID == n.Self().ID {
		return wire.DirectReport{}, wire.Errorf(wire.CodeInvalidParams, "%s is this machine", dest.Name)
	}
	r := wire.DirectReport{Node: dest.Info()}
	if err := n.Call(ctx, dest.ID, wire.MethodDirectTrace, nil, &r.Peer); err != nil {
		r.PeerError = err.Error()
	}
	peer := n.packetPeer(dest.ID)
	if c := peer.direct.Load(); c != nil {
		r.Path = &wire.DirectPath{Local: c.Local(), Remote: c.Remote()}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.udp != nil {
		r.Stats = n.udp.Stats()
	}
	r.Stats.WriteErrors, r.Stats.QueueDrops = peer.writeErrors.Load(), peer.queueDrops.Load()
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
	case time.Now().Before(n.directWaits[id].next), n.removed != nil, n.isRemoved(id):
		return
	}
	ctx, try := n.startDirect(id, true)
	go n.offerDirect(ctx, id, try)
}

// offerDirect is the side of a try that starts it: it offers a path and
// finds one to the node that answers.
func (n *Node) offerDirect(ctx context.Context, id string, try *directTry) {
	defer n.endDirect(id, try)
	path, err := n.connectDirect(ctx, id)
	if err != nil {
		n.directFailed(ctx, id, err.Error())
		return
	}
	n.runDirect(id, path, try)
}

// connectDirect offers a path to node id and finds one. Its errors are
// logged, so they leave out what the other node reports.
func (n *Node) connectDirect(ctx context.Context, id string) (*direct.Path, error) {
	path, err := n.udp.Start(func(text string) { n.trace(id, "%s", text) })
	if err != nil {
		return nil, err
	}
	offer := path.Offer()
	n.trace(id, "offering over SSH: %s", strings.Join(offer.Addresses, ", "))
	var answer wire.DirectOffer
	if err := n.Call(ctx, id, wire.MethodDirectOffer, offer, &answer); err != nil {
		n.trace(id, "the offer failed: %v", err)
		path.Close()
		return nil, errors.New("the other machine did not take the offer")
	}
	n.trace(id, "the answer offers: %s", strings.Join(answer.Addresses, ", "))
	if err := path.Connect(ctx, answer); err != nil {
		path.Close()
		return nil, errors.New("no path")
	}
	return path, nil
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
func (n *Node) serveOffer(_ context.Context, from string, offer wire.DirectOffer) (any, error) {
	if n.onPacket.Load() == nil {
		return nil, wire.Errorf(wire.CodeFailed, "%s is not on the gst network", n.Self().Name)
	}
	old := n.packetPeer(from).direct.Load()
	n.mu.Lock()
	defer n.mu.Unlock()
	switch {
	case n.directCtx == nil:
		return nil, wire.Errorf(wire.CodeFailed, "UDP paths are off on %s", n.info.Name)
	case from == n.info.ID:
		return nil, wire.Errorf(wire.CodeInvalidParams, "a UDP path needs another machine")
	case n.removed != nil || n.isRemoved(from):
		return nil, wire.Errorf(wire.CodeRemoved, "%s is not in this network", n.nodeName(from))
	}
	if try := n.directTries[from]; try != nil && try.offering && n.info.ID < from {
		return nil, wire.Errorf(wire.CodeFailed, "%s is offering a path itself", n.info.Name)
	}
	path, err := n.udp.Join(offer, func(text string) { n.trace(from, "%s", text) })
	if err != nil {
		return nil, wire.Errorf(wire.CodeInvalidParams, "%v", err)
	}
	// The other node has no path, or it would not offer one. If this one
	// thinks there is, that path is gone.
	if old != nil {
		old.Close()
	}
	answer := path.Offer()
	answer.Token = ""
	ctx, try := n.startDirect(from, false)
	go func() {
		defer n.endDirect(from, try)
		n.trace(from, "offer over SSH: %s", strings.Join(offer.Addresses, ", "))
		n.trace(from, "answering with: %s", strings.Join(answer.Addresses, ", "))
		if err := path.Connect(ctx, offer); err != nil {
			path.Close()
			n.directFailed(ctx, from, "no path")
			return
		}
		n.runDirect(from, path, try)
	}()
	return answer, nil
}

// runDirect makes packets to node id take path until it ends. The try
// ends once the path runs, so that there is always one or the other.
//
// A path that this node sends nothing on for directIdle ends, so that
// idle paths cost no probes; the next packet takes the links and finds a
// path again. Sending is the sign of traffic both ways: WireGuard answers
// what it receives within 10 seconds, with a keepalive if it has nothing
// else to send. A path that the links do not lead around stays, as it is
// then the only way to the node.
func (n *Node) runDirect(id string, path *direct.Path, try *directTry) {
	n.mu.Lock()
	delete(n.directWaits, id)
	ctx := n.directCtx
	n.mu.Unlock()
	n.attachPackets(id, path)
	defer n.Update() // after the path is gone: the watchers see it go
	defer n.detachPackets(id, path)
	defer path.Close()
	n.endDirect(id, try)
	n.log.Printf("UDP path to %s: up", n.nodeName(id))
	n.Update()
	idle := time.NewTicker(n.directIdle / 5)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-path.Done():
			// Only a failure waits: the other side closed the path on
			// purpose, as it was idle there or the node left.
			why := "closed by the other node"
			if path.FellSilent() {
				why = "fell silent"
				n.backOff(id)
			}
			n.log.Printf("UDP path to %s: %s", n.nodeName(id), why)
			n.trace(id, "%s", why)
			return
		case <-idle.C:
			if path.Idle() >= n.directIdle && n.router.Reaches(id) {
				n.log.Printf("UDP path to %s: idle, closed", n.nodeName(id))
				n.trace(id, "nothing sent for %s: closed until traffic comes", n.directIdle)
				return
			}
		}
	}
}

// nodeName returns the name of node id, for the log. It reads only the
// table, so n.mu may be held or not.
func (n *Node) nodeName(id string) string {
	if e, ok := n.table.Get(state.NodeKey(id)); ok {
		var v wire.NodeValue
		if json.Unmarshal(e.Value, &v) == nil && v.Name != "" {
			return v.Name
		}
	}
	return id[:min(8, len(id))]
}
