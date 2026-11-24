package mesh

import (
	"cmp"
	"context"
	"encoding/hex"
	"slices"
	"sync"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/direct"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

const (
	relayRetry = 30 * time.Second // first wait after a try at a relay fails
	relayWait  = 8 * time.Second  // for the UDP paths to the relays, on each side of a try
	maxRelays  = 8                // relays that a try looks at
)

// Two nodes that find no UDP path between them, as behind NATs that give
// every destination another port, may each have one to a third node. When
// that node relays (gst relay), it carries their packets between its two
// paths, over UDP all the way, which beats the SSH links: those are TCP,
// where a lost packet holds up the ones behind it, and lead only where the
// links do.
//
// The two nodes agree on the relay with one routed call, relay.offer, so a
// node that knows nothing of relays is never sent through one. The caller
// names the relays it has a UDP path to, with the round trip of each; the
// callee finds its own paths to them and picks the relay with the shortest
// way through it. Both then send through that relay, which matters: each
// keeps its path to the relay by sending on it, and the relay its two
// paths by handing on what comes (see runDirect).
//
// The relay itself is told nothing. It hands a packet to its UDP path to
// the node named, and to the links when it has none, so that nothing is
// lost while a path is gone. A node gives a relay up when its path to it
// ends, or when nothing came back through it for as long as an idle path
// lasts: WireGuard answers what it receives within 10 seconds, so silence
// that long says that the other node left it. The next packets take the
// links and ask again.
//
// The relay also serves as the node that tells the others their public
// addresses: the answers on a path to it name the source it saw, which the
// next offers of a UDP path carry, so the two nodes may yet find a path of
// their own. They keep trying, and a path between them takes the packets
// as soon as it is up.

// relayPacket sends packet to the node of p through its relay, and reports
// whether it went. It gives up a relay that it has no UDP path to, or that
// nothing came back through.
func (n *Node) relayPacket(p *packetPeer, packet []byte) bool {
	relay := p.relay.Load()
	if relay == nil {
		return false
	}
	path := n.packetPeer(*relay).direct.Load()
	if path == nil || time.Since(time.Unix(0, p.relayHeard.Load())) > n.directIdle {
		if p.relay.CompareAndSwap(relay, nil) {
			n.Update()
		}
		return false
	}
	return path.WriteRelay(direct.RelayTo, p.key, packet) == nil
}

// carry handles a packet that a relay carries, which came over the UDP
// path to node from, whose id in packets is key. One that names where it
// goes asks this node to relay it: to its UDP path to that node, or, while
// it has none, along the links, as from sent it. One that names where it
// came from is for this node, and from is the relay.
func (n *Node) carry(from string, key [direct.NodeLen]byte, kind direct.Relay, node [direct.NodeLen]byte, packet []byte) {
	other := hex.EncodeToString(node[:])
	peer, _ := n.packets.Load(other)
	switch kind {
	case direct.RelayTo:
		if !n.relaying.Load() {
			return
		}
		if peer != nil {
			if path := peer.(*packetPeer).direct.Load(); path != nil && path.WriteRelay(direct.RelayFrom, key, packet) == nil {
				return
			}
		}
		n.router.Forward(from, other, packet)
	case direct.RelayFrom:
		if peer != nil {
			if p := peer.(*packetPeer); p.relayIs(from) {
				p.relayHeard.Store(time.Now().UnixNano())
			}
		}
		n.deliver(other, packet)
	}
}

func (p *packetPeer) relayIs(id string) bool {
	relay := p.relay.Load()
	return relay != nil && *relay == id
}

// PacketRelay returns the id of the relay that packets to node id go
// through now; empty when they go another way.
func (n *Node) PacketRelay(id string) string {
	p := n.packetPeer(id)
	if relay := p.relay.Load(); relay != nil && p.direct.Load() == nil && n.packetPeer(*relay).direct.Load() != nil {
		return *relay
	}
	return ""
}

// useRelay makes packets to node id go through relay while there is no UDP
// path to it.
func (n *Node) useRelay(id, relay string) {
	p := n.packetPeer(id)
	p.relayHeard.Store(time.Now().UnixNano())
	p.relayFails.Store(0)
	p.relay.Store(&relay)
	n.log.Printf("UDP relay to %s: through %s", n.nodeName(id), n.nodeName(relay))
	n.Update()
}

// dropRelays gives up the relays that keep reports no longer to be one.
// n.mu is held.
func (n *Node) dropRelays(keep func(relay string) bool) {
	n.packets.Range(func(_, value any) bool {
		p := value.(*packetPeer)
		if relay := p.relay.Load(); relay != nil && !keep(*relay) {
			p.relay.CompareAndSwap(relay, nil)
		}
		return true
	})
}

// relayNodes returns the nodes of the network that relay, besides this one
// and but. n.mu is held.
func (n *Node) relayNodes(but string) []string {
	var list []string
	for id, e := range nodeEntries(n.table.Entries()) {
		if e.value.Relay && id != n.info.ID && id != but && !n.isRemoved(id) {
			list = append(list, id)
		}
	}
	slices.Sort(list)
	return list[:min(len(list), maxRelays)]
}

// tryRelay looks for a relay to node id in the background, once a try at a
// UDP path to it failed, unless a try at a relay failed recently.
func (n *Node) tryRelay(id string) {
	p := n.packetPeer(id)
	n.mu.Lock()
	defer n.mu.Unlock()
	fails := p.relayFails.Load()
	switch {
	case n.directCtx == nil, id == n.info.ID, n.onPacket.Load() == nil, n.removed != nil:
		return
	case p.relay.Load() != nil, p.direct.Load() != nil, n.relayTries[id], n.directWaits[id].wait == 0:
		return
	case fails > 0 && time.Since(time.Unix(0, p.relayAsked.Load())) < min(relayRetry<<min(fails-1, 8), directMaxWait):
		return
	case !n.router.Reaches(id), n.isRemoved(id):
		return // The offer goes along the links.
	}
	relays := n.relayNodes(id)
	if len(relays) == 0 {
		return
	}
	p.relayAsked.Store(time.Now().UnixNano())
	n.relayTries[id] = true
	ctx, cancel := context.WithTimeout(n.directCtx, 3*relayWait)
	go func() {
		defer cancel()
		why := n.offerRelay(ctx, id, relays)
		n.mu.Lock()
		delete(n.relayTries, id)
		n.mu.Unlock()
		// The other node's offer may have set one meanwhile.
		if why != "" && p.relay.Load() == nil && ctx.Err() != context.Canceled {
			p.relayFails.Add(1)
			n.log.Printf("UDP relay to %s: %s", n.nodeName(id), why)
		}
	}()
}

// offerRelay is the side of a try at a relay that starts it. It returns
// why it found none, for the log, which leaves out what the other node
// reports.
func (n *Node) offerRelay(ctx context.Context, id string, relays []string) (why string) {
	legs := n.relayLegs(ctx, relays)
	if len(legs) == 0 {
		return "this machine has a UDP path to none of the relays"
	}
	var choice wire.RelayChoice
	if err := n.router.Call(ctx, id, wire.MethodRelayOffer, wire.RelayOffer{Relays: legs}, &choice); err != nil {
		n.trace(id, "the offer of relays failed: %v", err)
		return "the other machine took none of the relays"
	}
	if !slices.ContainsFunc(legs, func(l wire.RelayLeg) bool { return l.Node == choice.Node }) {
		return "the other machine picked a relay that was not offered"
	}
	n.useRelay(id, choice.Node)
	return ""
}

// serveRelay answers relay.offer: of the relays that both nodes have a UDP
// path to, it picks the one with the shortest way through it. When both
// nodes offer at once, the offer of the node with the lower id goes on.
func (n *Node) serveRelay(ctx context.Context, from string, offer wire.RelayOffer) (any, error) {
	n.mu.Lock()
	var err error
	switch {
	case n.directCtx == nil || n.onPacket.Load() == nil:
		err = wire.Errorf(wire.CodeFailed, "UDP paths are off on %s", n.info.Name)
	case from == n.info.ID:
		err = wire.Errorf(wire.CodeInvalidParams, "a relay needs another machine")
	case n.removed != nil || n.isRemoved(from):
		err = wire.Errorf(wire.CodeRemoved, "%s is not in this network", n.nodeName(from))
	case n.relayTries[from] && n.info.ID < from:
		err = wire.Errorf(wire.CodeFailed, "%s is offering relays itself", n.info.Name)
	}
	relays := n.relayNodes(from)
	name := n.info.Name
	n.mu.Unlock()
	if err != nil {
		return nil, err
	}
	theirs := map[string]float64{}
	for _, leg := range offer.Relays {
		theirs[leg.Node] = leg.RTTMS
	}
	relays = slices.DeleteFunc(relays, func(id string) bool { _, ok := theirs[id]; return !ok })
	var best *wire.RelayLeg
	for _, leg := range n.relayLegs(ctx, relays) {
		if leg.RTTMS += theirs[leg.Node]; best == nil || leg.RTTMS < best.RTTMS {
			best = &leg
		}
	}
	if best == nil {
		return nil, wire.Errorf(wire.CodeFailed, "%s has a UDP path to none of the relays offered", name)
	}
	n.useRelay(from, best.Node)
	return wire.RelayChoice{Node: best.Node}, nil
}

// relayLegs finds the UDP paths to relays, and returns those that it has
// within relayWait, each with its round trip.
func (n *Node) relayLegs(ctx context.Context, relays []string) []wire.RelayLeg {
	ctx, cancel := context.WithTimeout(ctx, relayWait)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	legs := []wire.RelayLeg{}
	for _, id := range relays {
		wg.Go(func() {
			if rtt, ok := n.measure(ctx, id); ok {
				mu.Lock()
				defer mu.Unlock()
				legs = append(legs, wire.RelayLeg{Node: id, RTTMS: float64(rtt.Microseconds()) / 1000})
			}
		})
	}
	wg.Wait()
	slices.SortFunc(legs, func(a, b wire.RelayLeg) int { return cmp.Compare(a.Node, b.Node) })
	return legs
}

// measure returns the round trip of the UDP path to node id, which it
// asks for when there is none; false when a try found none, or nothing
// measured the path before ctx is done.
func (n *Node) measure(ctx context.Context, id string) (time.Duration, bool) {
	p := n.packetPeer(id)
	for {
		if path := p.direct.Load(); path != nil {
			if rtt, ok := path.RTT(); ok {
				return rtt, true
			}
		} else if n.tryDirect(id); !n.trying(id) && p.direct.Load() == nil {
			return 0, false // A try found none, and the next one waits.
		}
		select {
		case <-ctx.Done():
			return 0, false
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// trying reports whether a try at a UDP path to node id runs.
func (n *Node) trying(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.directTries[id] != nil
}
