package mesh

import (
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/direct"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// noDirect makes a and b find no UDP path between them, as two machines
// behind NATs that do not let them: the try of each failed, and the next
// one waits.
func noDirect(a, b *Node) {
	for n, id := range map[*Node]string{a: b.Self().ID, b: a.Self().ID} {
		n.mu.Lock()
		n.directWaits[id] = directWait{next: time.Now().Add(time.Hour), wait: time.Hour}
		n.mu.Unlock()
	}
}

// relayed sets up a and b, which find no UDP path between them, and r,
// which relays and which both link to, and has a and b agree on r. Paths
// end after idle without traffic.
func relayed(t *testing.T, idle time.Duration) (a, b, r *Node, boxes map[*Node]*inbox, links []*pipeLink) {
	t.Helper()
	a, b, r = newNode(t, "a"), newNode(t, "b"), newNode(t, "r")
	a.directIdle, b.directIdle, r.directIdle = idle, idle, idle
	r.Configure(&config.Node{ID: nid("r"), Name: "r", Relay: true})
	boxes = enableDirect(t, a, b, r)
	links = []*pipeLink{linkUp(t, a, r), linkUp(t, b, r)}
	waitFor(t, "a knows that r relays", func() bool { n, _ := findTopo(a.Topology(), nid("r")); return n.Relay && reaches(a, "b") })
	waitFor(t, "b knows that r relays", func() bool { n, _ := findTopo(b.Topology(), nid("r")); return n.Relay && reaches(b, "a") })
	noDirect(a, b)
	a.SendPacket(nid("b"), wg("first"))
	waitFor(t, "the first packet arrives along the links", func() bool { return boxes[b].has(nid("a"), string(wg("first"))) })
	waitFor(t, "a and b send through r", func() bool {
		return a.PacketWay(nid("b")) == wire.WayUDPRelay && b.PacketWay(nid("a")) == wire.WayUDPRelay
	})
	return a, b, r, boxes, links
}

// Two nodes with no UDP path between them send through a relay that both
// have one to, over UDP: the links may be gone.
func TestRelay(t *testing.T) {
	a, b, r, boxes, links := relayed(t, directIdle)
	if node, _ := findTopo(a.Topology(), nid("b")); node.Via != nid("r") || node.Direct {
		t.Fatalf("the topology shows %+v", node)
	}
	if hasDirect(a, nid("b")) || hasDirect(b, nid("a")) {
		t.Fatal("a UDP path between the two")
	}
	for _, l := range links {
		l.cut()
		l.wait(t)
	}
	waitFor(t, "no link leads from a to b", func() bool { return !reaches(a, "b") && !reaches(b, "a") })
	a.SendPacket(nid("b"), wg("through r"))
	waitFor(t, "the packet arrives through r", func() bool { return boxes[b].has(nid("a"), string(wg("through r"))) })
	b.SendPacket(nid("a"), wg("back through r"))
	waitFor(t, "the packet comes back through r", func() bool { return boxes[a].has(nid("b"), string(wg("back through r"))) })
	if boxes[r].has(nid("a"), string(wg("through r"))) {
		t.Fatal("the relay took the packet for itself")
	}
}

// A relay that lost its UDP path to a node hands the packets to the links,
// as the node that sent them.
func TestRelayFallsBackToTheLinks(t *testing.T) {
	a, b, r, boxes, _ := relayed(t, directIdle)
	noDirect(r, b)
	r.packetPeer(nid("b")).direct.Load().Close()
	waitFor(t, "the path from r to b ends", func() bool { return !hasDirect(r, nid("b")) && !hasDirect(b, nid("r")) })
	a.SendPacket(nid("b"), wg("along the links"))
	waitFor(t, "the packet arrives", func() bool { return boxes[b].has(nid("a"), string(wg("along the links"))) })
	// b has no path to the relay: its packets take the links at once.
	b.SendPacket(nid("a"), wg("back"))
	waitFor(t, "the packet comes back", func() bool { return boxes[a].has(nid("b"), string(wg("back"))) })
	if got := b.PacketWay(nid("a")); got != wire.WayRelay {
		t.Fatalf("b sends %q", got)
	}
}

// Traffic keeps the relay: each node its path to it, and the relay both of
// its paths, which carry nothing of its own. A node that hears nothing
// back through the relay gives it up.
func TestRelayStaysWhileUsed(t *testing.T) {
	idle := 800 * time.Millisecond
	a, b, r, boxes, _ := relayed(t, idle)
	// No new try at a relay: the test is of the one that runs.
	a.packetPeer(nid("b")).asked.Store(time.Now().Add(time.Hour).UnixNano())
	b.packetPeer(nid("a")).asked.Store(time.Now().Add(time.Hour).UnixNano())
	for end := time.Now().Add(3 * idle); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		a.SendPacket(nid("b"), wg("busy"))
		b.SendPacket(nid("a"), wg("busy"))
	}
	if a.PacketWay(nid("b")) != wire.WayUDPRelay || b.PacketWay(nid("a")) != wire.WayUDPRelay {
		t.Fatal("the relay ended under traffic")
	}
	if !hasDirect(r, nid("a")) || !hasDirect(r, nid("b")) {
		t.Fatal("the relay closed a path that it sends the packets of others on")
	}

	for end := time.Now().Add(2 * idle); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		a.SendPacket(nid("b"), wg("one way"))
	}
	if got := a.PacketWay(nid("b")); got != wire.WayRelay {
		t.Fatalf("a sends %q with nothing coming back", got)
	}
	a.SendPacket(nid("b"), wg("along the links"))
	waitFor(t, "the packet takes the links", func() bool { return boxes[b].has(nid("a"), string(wg("along the links"))) })
}

// A node that stops relaying carries nothing more, and the nodes that sent
// through it take the links.
func TestRelayStops(t *testing.T) {
	a, b, r, boxes, _ := relayed(t, directIdle)
	r.Configure(&config.Node{ID: nid("r"), Name: "r"})
	waitFor(t, "a and b give r up", func() bool {
		return a.PacketWay(nid("b")) == wire.WayRelay && b.PacketWay(nid("a")) == wire.WayRelay
	})
	a.SendPacket(nid("b"), wg("along the links"))
	waitFor(t, "the packet arrives", func() bool { return boxes[b].has(nid("a"), string(wg("along the links"))) })
	// A packet that still asks r to relay goes nowhere.
	a.packetPeer(nid("r")).direct.Load().WriteRelay(direct.RelayTo, a.packetPeer(nid("b")).key, wg("refused"))
	time.Sleep(100 * time.Millisecond)
	if boxes[b].has(nid("a"), string(wg("refused"))) {
		t.Fatal("a node that does not relay carried a packet")
	}
}

// A node that knows nothing of relays is never sent through one.
func TestRelayNeedsBothNodes(t *testing.T) {
	a, b, r := newNode(t, "a"), newNode(t, "b"), newNode(t, "r")
	r.Configure(&config.Node{ID: nid("r"), Name: "r", Relay: true})
	enableDirect(t, a, b, r)
	linkUp(t, a, r)
	linkUp(t, b, r)
	waitFor(t, "a knows that r relays", func() bool { n, _ := findTopo(a.Topology(), nid("r")); return n.Relay && reaches(a, "b") })
	noDirect(a, b)
	b.HandlePackets(nil) // as a node off the virtual network refuses
	a.SendPacket(nid("b"), wg("first"))
	waitFor(t, "the try at a relay fails", func() bool { return a.packetPeer(nid("b")).relayFails.Load() > 0 })
	if got := a.PacketWay(nid("b")); got != wire.WayRelay {
		t.Fatalf("a sends %q", got)
	}
}
