package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
// takes the links until the path is up, and again after it ends. The two
// nodes confirm the path to each other before either sends on it, and the
// one that knows of it first is ready for the other's packets, so a try
// hands on what its path lets through from the start (receiveOn).
//
// A path is no link: it carries only the packets of its two nodes, no
// calls, no table, and nobody else's packets, and the table says nothing
// of it. It is a shortcut for packets between nodes of the network, so it outlives
// the links, as when the machine between its two nodes sleeps, and ends
// when the other node is removed from the network.

// EnableDirect lets this node find and accept UDP paths on network until
// ctx is done.
func (n *Node) EnableDirect(ctx context.Context, network direct.Network) {
	udp, err := direct.Listen(direct.Port, network, n.inSubnet)
	if err != nil {
		n.log.Print("UDP paths are off: no socket")
		return
	}
	n.mu.Lock()
	n.directCtx, n.udp = ctx, udp
	n.offerSeen()
	n.mu.Unlock()
	go n.watchNetwork(ctx, network, udp)
}

// watchNetwork follows this machine from network to network until ctx is
// done, with udp, the transport of the UDP paths, at first.
func (n *Node) watchNetwork(ctx context.Context, network direct.Network, udp *direct.Transport) {
	for {
		select {
		case <-ctx.Done():
			udp.Close()
			return
		case <-udp.Changed():
			n.addressesChanged(ctx)
		case <-udp.Moved():
			if udp = n.startOver(ctx, network, udp); udp == nil {
				return
			}
		}
	}
}

// transport returns the transport of the UDP paths; nil while they are
// off.
func (n *Node) transport() *direct.Transport {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.udp
}

// offerSeen has the offers of UDP paths name the addresses that the peers
// of the running links see this machine at: the ends of their SSH
// connections, which behind NAT show its public address when no node has
// yet reached it over UDP. They hold while their links run. n.mu is held.
func (n *Node) offerSeen() {
	if n.udp == nil {
		return
	}
	var seen []netip.Addr
	for l := range n.links {
		if l.Seen.IsValid() {
			seen = append(seen, l.Seen)
		}
	}
	slices.SortFunc(seen, netip.Addr.Compare)
	n.udp.SetSeen(slices.Compact(seen))
}

// Moved returns a channel that is closed the next time this machine moves
// to another network, once the links and UDP paths started over (see
// startOver), for the daemon to connect its links again at once. Nothing
// closes it while UDP paths are off, as their transport finds out.
func (n *Node) Moved() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.moved
}

// startOver ends the links and the UDP paths after this machine moved to
// another network, as a restart of gst would, but keeps the virtual
// network and what runs on it. What was opened on the old network may be
// stuck there: a connection that leaves from an address now gone hears
// nothing until the alive frames give up on it, and on iOS a socket keeps
// sending to where the old network was. What was learned of the old
// network, what failed and what was measured, holds no longer either.
//
// The links end with a bye, which the other side gets if it still can;
// otherwise it ends its side when a new link replaces it. The daemon then
// connects its links again at once (Moved). The paths end with their
// transport, old, and a new one with new sockets takes its place, which
// startOver returns; nil when there is no socket, and UDP paths are off.
// The next packets find paths again.
func (n *Node) startOver(ctx context.Context, network direct.Network, old *direct.Transport) *direct.Transport {
	n.log.Print("this machine moved to another network; starting the links and UDP paths over")
	n.mu.Lock()
	links := slices.Collect(maps.Keys(n.links))
	clear(n.directWaits)
	for _, try := range n.directTries {
		try.cancel()
	}
	n.mu.Unlock()
	bye := &wire.Bye{Error: wire.Errorf(wire.CodeFailed, "%s moved to another network", n.Self().Name)}
	for _, l := range links {
		l.Close(bye)
	}
	n.mu.Lock()
	close(n.moved) // after the links closed, which the daemon's restart would close as stopped
	n.moved = make(chan struct{})
	n.mu.Unlock()
	old.Close()
	udp, err := direct.Listen(direct.Port, network, n.inSubnet)
	n.mu.Lock()
	defer n.mu.Unlock()
	if err != nil || ctx.Err() != nil {
		if udp != nil {
			udp.Close()
		}
		n.directCtx, n.udp = nil, nil
		if err != nil {
			n.log.Print("UDP paths are off: no socket")
		}
		return nil
	}
	n.udp = udp
	n.offerSeen()
	if through := udp.Stats().Interfaces; len(through) > 0 {
		n.log.Printf("UDP paths send through %s", strings.Join(through, ", "))
	}
	return udp
}

// addressesChanged takes in new addresses of this machine on the same
// network, as when a temporary IPv6 address comes: what failed may work
// now, and what was measured may not hold. Each running path measures
// again, and the other side learns the addresses.
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
	return n.traceOf(from), nil
}

// traceOf returns the latest tries with node id. n.mu is held.
func (n *Node) traceOf(id string) wire.DirectTrace {
	return wire.DirectTrace{Events: slices.Clone(n.directTrace[id]), Previous: slices.Clone(n.directPrev[id])}
}

// DirectReport reports the UDP path to the node that ref names, and
// what this node and that node did in their latest two tries at one.
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
	r.Local = n.traceOf(dest.ID)
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
	if events := n.directTrace[id]; len(events) > 0 {
		n.directPrev[id] = events // Its end says why there is a try again.
	}
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
	udp := n.transport()
	if udp == nil {
		return nil, errors.New("UDP paths are off")
	}
	path, err := udp.Start(func(text string) { n.trace(id, "%s", text) })
	if err != nil {
		return nil, err
	}
	n.receiveOn(id, path)
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
	case n.directCtx == nil || n.udp == nil:
		return nil, wire.Errorf(wire.CodeFailed, "UDP paths are off on %s", n.info.Name)
	case from == n.info.ID:
		return nil, wire.Errorf(wire.CodeInvalidParams, "a UDP path needs another machine")
	case n.removed != nil || n.isRemoved(from):
		return nil, wire.Errorf(wire.CodeRemoved, "%s is not in this network", n.nodeName(from))
	}
	if try := n.directTries[from]; try != nil && try.offering && n.info.ID < from {
		return nil, wire.Errorf(wire.CodeFailed, "%s is offering a path itself", n.info.Name)
	}
	// The other node offers a path when it has none. A path that it still
	// answers on says otherwise: the offer crossed this node's, and the
	// other node took this one's since and gave up its own, whose path
	// would never come up. Taking it would end the path that runs.
	if old != nil && old.Answering() {
		return nil, wire.Errorf(wire.CodeFailed, "a UDP path to %s runs", n.info.Name)
	}
	path, err := n.udp.Join(offer, func(text string) { n.trace(from, "%s", text) })
	if err != nil {
		return nil, wire.Errorf(wire.CodeInvalidParams, "%v", err)
	}
	// A path that the other node no longer answers on is gone.
	if old != nil {
		old.Close()
	}
	n.receiveOn(from, path)
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
	ctx, moved := n.directCtx, n.moved
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
		case <-moved:
			n.log.Printf("UDP path to %s: closed, as this machine moved", n.nodeName(id))
			n.trace(id, "closed, as this machine moved")
			return
		case <-path.Done():
			// Only a failure waits: the other side closed the path on
			// purpose, as it was idle there or the node left.
			why := "closed by the other node"
			select {
			case <-moved: // the transport closed it after the move
				why = "closed, as this machine moved"
			default:
				if path.FellSilent() {
					why = "fell silent"
					n.backOff(id)
				}
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
