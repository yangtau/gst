package mesh

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func loopback() []netip.Addr { return []netip.Addr{netip.MustParseAddr("127.0.0.1")} }

// enableDirect lets nodes use UDP paths on the loopback interface, and
// puts them on the virtual network; it returns what each receives.
func enableDirect(t *testing.T, nodes ...*Node) map[*Node]*inbox {
	t.Helper()
	boxes := map[*Node]*inbox{}
	for _, n := range nodes {
		boxes[n] = &inbox{}
		n.HandlePackets(boxes[n].handle)
		n.EnableDirect(t.Context(), loopback)
	}
	return boxes
}

func hasDirect(n *Node, id string) bool {
	return n.packetPeer(id).direct.Load() != nil
}

// goDirect sets up a UDP path between a and c, which reach each other.
func goDirect(t *testing.T, a, c *Node) {
	t.Helper()
	waitFor(t, "a reaches c", func() bool { n, _ := findTopo(a.Topology(), nid("c")); return n.Reachable })
	a.tryDirect(nid("c"))
	waitFor(t, "the path runs", func() bool { return hasDirect(a, nid("c")) && hasDirect(c, nid("a")) })
}

// wg makes a packet that looks like WireGuard's, as UDP paths only carry
// those.
func wg(text string) []byte { return append([]byte{4}, text...) }

// Traffic between two machines sets up a UDP path: packets go along the
// links first, then over UDP, and along the links again when it ends.
func TestDirectPath(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	boxes := enableDirect(t, a, b)
	linkUp(t, a, b)
	waitFor(t, "a sees b", func() bool { n, _ := findTopo(a.Topology(), nid("b")); return n.Reachable })

	a.SendPacket(nid("b"), wg("first"))
	waitFor(t, "the first packet arrives", func() bool { return boxes[b].has(nid("a"), string(wg("first"))) })
	waitFor(t, "the path runs on a", func() bool { return hasDirect(a, nid("b")) })
	waitFor(t, "the path runs on b", func() bool { return hasDirect(b, nid("a")) })
	if got := a.PacketPath(nid("b")); got != wire.PathDirect {
		t.Fatalf("path = %q", got)
	}
	node, _ := findTopo(a.Topology(), nid("b"))
	if !node.Direct {
		t.Fatal("the topology does not show the path")
	}
	// A path is no link: only the SSH link counts.
	if edges := a.Topology().Edges; len(edges) != 1 {
		t.Fatalf("edges = %+v", edges)
	}

	a.SendPacket(nid("b"), wg("over udp"))
	waitFor(t, "the packet arrives over UDP", func() bool { return boxes[b].has(nid("a"), string(wg("over udp"))) })

	a.packetPeer(nid("b")).direct.Load().Close()
	waitFor(t, "the path ends on b", func() bool { return !hasDirect(b, nid("a")) })
	waitFor(t, "the path ends on a", func() bool { return !hasDirect(a, nid("b")) })
	a.SendPacket(nid("b"), wg("back on ssh"))
	waitFor(t, "the packet arrives along the link", func() bool { return boxes[b].has(nid("a"), string(wg("back on ssh"))) })
	// A path closed on purpose is no failure.
	if waiting(a, nid("b")) || waiting(b, nid("a")) {
		t.Fatal("waiting after a path was closed")
	}
}

// waiting reports whether n holds off trying node id again.
func waiting(n *Node, id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return time.Now().Before(n.directWaits[id].next)
}

// A path that carries nothing ends, and traffic finds one again at once.
func TestIdleDirectPathEnds(t *testing.T) {
	a, c := newNode(t, "a"), newNode(t, "c")
	a.directIdle, c.directIdle = 300*time.Millisecond, 300*time.Millisecond
	boxes := enableDirect(t, a, c)
	linkUp(t, a, c)
	goDirect(t, a, c)
	waitFor(t, "the path ends at both ends", func() bool { return !hasDirect(a, nid("c")) && !hasDirect(c, nid("a")) })

	a.SendPacket(nid("c"), wg("again"))
	waitFor(t, "the packet arrives", func() bool { return boxes[c].has(nid("a"), string(wg("again"))) })
	waitFor(t, "traffic finds a path again", func() bool { return hasDirect(a, nid("c")) && hasDirect(c, nid("a")) })
}

// An idle path that is the only way to a node stays.
func TestIdleDirectPathWithoutLinksStays(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	a.directIdle, c.directIdle = 300*time.Millisecond, 300*time.Millisecond
	enableDirect(t, a, c)
	ab, cb := linkUp(t, a, b), linkUp(t, c, b)
	goDirect(t, a, c)
	// Traffic keeps the path until the links are gone.
	busy := make(chan struct{})
	go func() {
		for {
			select {
			case <-busy:
				return
			case <-time.After(50 * time.Millisecond):
				a.SendPacket(nid("c"), wg("busy"))
				c.SendPacket(nid("a"), wg("busy"))
			}
		}
	}()
	ab.cut()
	cb.cut()
	ab.wait(t)
	cb.wait(t)
	waitFor(t, "a loses b", func() bool { n, _ := findTopo(a.Topology(), nid("b")); return !n.Reachable })
	waitFor(t, "c loses b", func() bool { n, _ := findTopo(c.Topology(), nid("b")); return !n.Reachable })
	close(busy)
	time.Sleep(3 * a.directIdle)
	if !hasDirect(a, nid("c")) || !hasDirect(c, nid("a")) {
		t.Fatal("the only way to the node ended")
	}
}

// Two nodes that have no link between them get a path too, and the
// machine between them sees no sign of it.
func TestDirectPathAcrossTheMesh(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	boxes := enableDirect(t, a, c)
	linkUp(t, a, b)
	linkUp(t, c, b)
	goDirect(t, a, c)
	a.SendPacket(nid("c"), wg("hello"))
	waitFor(t, "the packet arrives", func() bool { return boxes[c].has(nid("a"), string(wg("hello"))) })
	if n, _ := findTopo(a.Topology(), nid("c")); !n.Direct || len(n.Path) != 3 {
		t.Fatalf("a sees c as %+v", n)
	}
	if n, _ := findTopo(b.Topology(), nid("a")); n.Direct {
		t.Fatal("b has a path")
	}
}

// When the machine between two nodes goes away, as when it sleeps, their
// UDP path carries on: they are members still.
func TestDirectPathOutlivesSSH(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	boxes := enableDirect(t, a, c)
	ab, cb := linkUp(t, a, b), linkUp(t, c, b)
	goDirect(t, a, c)

	ab.cut()
	cb.cut()
	ab.wait(t)
	cb.wait(t)
	waitFor(t, "a loses b", func() bool { n, _ := findTopo(a.Topology(), nid("b")); return !n.Reachable })
	if !hasDirect(a, nid("c")) || !hasDirect(c, nid("a")) {
		t.Fatal("the path ended with the SSH links")
	}
	a.SendPacket(nid("c"), wg("hello"))
	waitFor(t, "the packet arrives", func() bool { return boxes[c].has(nid("a"), string(wg("hello"))) })
	if n, _ := findTopo(a.Topology(), nid("c")); !n.Online() || n.Reachable {
		t.Fatalf("a sees c as %+v", n)
	}
}

// A new identity is a new machine: the other end knows its UDP paths by
// the old one, so they end at once.
func TestANewIdentityEndsDirect(t *testing.T) {
	a, c := newNode(t, "a"), newNode(t, "c")
	enableDirect(t, a, c)
	linkUp(t, a, c)
	goDirect(t, a, c)
	a.SetNet(&wire.NodeNet{Address: "10.0.0.1"})

	a.Configure(&config.Node{ID: nid("n"), Name: "a"})
	waitFor(t, "the path ends at both ends", func() bool { return !hasDirect(a, nid("c")) && !hasDirect(c, nid("a")) })
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.net != nil {
		t.Fatalf("the new identity took the old one's place in the network: %+v", a.net)
	}
}

// Removing a machine ends the UDP paths to it.
func TestRemovingANodeEndsDirect(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	enableDirect(t, a, c)
	linkUp(t, a, b)
	linkUp(t, c, b)
	goDirect(t, a, c)

	if _, err := b.Remove("c"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the path ends", func() bool { return !hasDirect(a, nid("c")) && !hasDirect(c, nid("a")) })
	a.SendPacket(nid("c"), wg("hello"))
	a.mu.Lock()
	tries := len(a.directTries)
	a.mu.Unlock()
	if tries != 0 {
		t.Fatal("a tries a path to a removed machine")
	}
	if _, err := a.serveOffer(t.Context(), nid("c"), wire.DirectOffer{}); err == nil {
		t.Fatal("a answered a removed machine")
	}
}

// When both nodes offer at the same time, one path results.
func TestDirectOffersCross(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	boxes := enableDirect(t, a, b)
	linkUp(t, a, b)
	waitFor(t, "they see each other", func() bool {
		x, _ := findTopo(a.Topology(), nid("b"))
		y, _ := findTopo(b.Topology(), nid("a"))
		return x.Reachable && y.Reachable
	})
	go a.tryDirect(nid("b"))
	go b.tryDirect(nid("a"))
	waitFor(t, "the path runs", func() bool { return hasDirect(a, nid("b")) && hasDirect(b, nid("a")) })
	time.Sleep(200 * time.Millisecond)
	a.SendPacket(nid("b"), wg("hello"))
	waitFor(t, "the packet arrives", func() bool { return boxes[b].has(nid("a"), string(wg("hello"))) })
}

// A node with UDP paths off refuses, and the other waits before it tries
// again. Packets take the links all the same.
func TestDirectOffOnTheOtherSide(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	enableDirect(t, a)
	var atB inbox
	b.HandlePackets(atB.handle)
	linkUp(t, a, b)
	waitFor(t, "a sees b", func() bool { n, _ := findTopo(a.Topology(), nid("b")); return n.Reachable })
	a.SendPacket(nid("b"), wg("hello"))
	waitFor(t, "a holds off", func() bool { return waiting(a, nid("b")) })
	waitFor(t, "the packet arrives", func() bool { return atB.has(nid("a"), string(wg("hello"))) })
	if hasDirect(a, nid("b")) {
		t.Fatal("a path without the other side")
	}
}

// A machine off the virtual network neither asks for a path nor takes one.
func TestDirectNeedsTheNetwork(t *testing.T) {
	a := newNode(t, "a")
	a.EnableDirect(t.Context(), loopback)
	if _, err := a.serveOffer(t.Context(), nid("b"), wire.DirectOffer{}); err == nil {
		t.Error("answered off the network")
	}
	a.tryDirect(nid("b"))
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.directTries) != 0 {
		t.Error("tried a path off the network")
	}
}

func TestDirectOfferInvalid(t *testing.T) {
	a := newNode(t, "a")
	a.HandlePackets(func(string, []byte) {})
	if _, err := a.serveOffer(t.Context(), nid("b"), wire.DirectOffer{}); err == nil {
		t.Error("answered with UDP paths off")
	}
	enableDirect(t, a)
	if _, err := a.serveOffer(t.Context(), nid("a"), wire.DirectOffer{}); err == nil {
		t.Error("answered an offer from itself")
	}
}

// Addresses of the virtual network are no way to a machine.
func TestAddressesOfTheNetworkAreSkipped(t *testing.T) {
	a := newNode(t, "a")
	addr := netip.MustParseAddr("10.99.0.7")
	if a.onNetwork(addr) {
		t.Fatal("without a subnet, no address is in it")
	}
	a.SetNetwork("10.99.0.0/16", true)
	waitFor(t, "the subnet decides", func() bool {
		return a.onNetwork(addr) && !a.onNetwork(netip.MustParseAddr("192.0.2.1"))
	})
}

// A node reports the addresses of a path, and what both ends did to set
// it up.
func TestDirectReport(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	enableDirect(t, a, c)
	linkUp(t, a, b)
	linkUp(t, c, b)
	goDirect(t, a, c)

	var r wire.DirectReport
	waitFor(t, "the report of the path", func() bool {
		var err error
		r, err = a.DirectReport(t.Context(), "c")
		return err == nil && r.Path != nil && !r.Trying && len(r.Peer.Events) > 0 && slices.ContainsFunc(r.Peer.Events, hasText("up: ")) && slices.ContainsFunc(r.Local.Events, hasText("up: "))
	})
	if r.PeerError != "" || r.Path.Local == "" || r.Path.Remote == "" {
		t.Fatalf("report %+v", r)
	}
	opener, other := r.Local.Events, r.Peer.Events
	if !slices.ContainsFunc(opener, hasText("offering")) {
		opener, other = other, opener // c offered.
	}
	for _, want := range []string{"offering", "the answer offers", "the path works", "up: using "} {
		if !slices.ContainsFunc(opener, hasText(want)) {
			t.Fatalf("the offering side's trace lacks %q: %v", want, opener)
		}
	}
	for _, want := range []string{"offer over SSH", "answering with", "probe from", "up: using "} {
		if !slices.ContainsFunc(other, hasText(want)) {
			t.Fatalf("the answering side's trace lacks %q: %v", want, other)
		}
	}

	if _, err := a.DirectReport(t.Context(), "a"); err == nil {
		t.Fatal("a report on this machine")
	}
}

func hasText(text string) func(wire.DirectEvent) bool {
	return func(e wire.DirectEvent) bool { return strings.Contains(e.Text, text) }
}

// A packet that UDP cannot send takes the links, as one too long for UDP.
func TestPacketsThatUDPCannotSend(t *testing.T) {
	a, c := newNode(t, "a"), newNode(t, "c")
	boxes := enableDirect(t, a, c)
	linkUp(t, a, c)
	goDirect(t, a, c)
	big := wg(strings.Repeat("x", 65520)) // more than a UDP packet holds, not than a stream does
	a.SendPacket(nid("c"), big)
	waitFor(t, "the packet arrives", func() bool { return boxes[c].has(nid("a"), string(big)) })
	if got := a.packetPeer(nid("c")).writeErrors.Load(); got != 1 {
		t.Fatalf("%d write errors", got)
	}
	r, err := a.DirectReport(t.Context(), "c")
	if err != nil || r.Stats.WriteErrors != 1 || r.Stats.Port == 0 {
		t.Fatalf("report: %+v, %v", r.Stats, err)
	}
}

// Moving to another network ends the wait after failures, and a running
// path measures again, on both sides.
func TestNetworkChange(t *testing.T) {
	a, c := newNode(t, "a"), newNode(t, "c")
	enableDirect(t, a, c)
	linkUp(t, a, c)
	goDirect(t, a, c)
	a.backOff(nid("gone"))

	a.networkChanged(t.Context())
	a.mu.Lock()
	waits := len(a.directWaits)
	a.mu.Unlock()
	if waits != 0 {
		t.Fatalf("%d waits left", waits)
	}
	waitFor(t, "c hears of it", func() bool {
		r, err := c.DirectReport(t.Context(), "a")
		return err == nil && slices.ContainsFunc(r.Local.Events, func(e wire.DirectEvent) bool {
			return strings.HasPrefix(e.Text, "update over SSH")
		})
	})
	if !hasDirect(a, nid("c")) || !hasDirect(c, nid("a")) {
		t.Fatal("the path ended")
	}
}

func TestDirectUpdateInvalid(t *testing.T) {
	a, c := newNode(t, "a"), newNode(t, "c")
	enableDirect(t, a, c)
	linkUp(t, a, c)
	if _, err := a.serveUpdate(t.Context(), nid("c"), wire.DirectOffer{}); err != nil {
		t.Errorf("an update without a path: %v", err)
	}
	goDirect(t, a, c)
	if _, err := a.serveUpdate(t.Context(), nid("c"), wire.DirectOffer{Token: "00"}); err == nil {
		t.Error("took the update of another path")
	}
}

// The trace keeps the latest events.
func TestTraceKeepsTheLatest(t *testing.T) {
	a := newNode(t, "a")
	for i := range maxTrace + 10 {
		a.trace("x", "event %d", i)
	}
	events := a.directTrace["x"]
	if len(events) != maxTrace || events[0].Text != "event 10" || events[maxTrace-1].Text != "event 209" {
		t.Fatalf("%d events, from %q", len(events), events[0].Text)
	}
}
