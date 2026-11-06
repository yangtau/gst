package mesh

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

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
	linkUp(t, a, b, "b")
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
	if edges := a.Topology().Edges; len(edges) != 1 || edges[0].Link != "b" {
		t.Fatalf("edges = %+v", edges)
	}

	streams := a.packetPeer(nid("b")).streams.Load()
	a.SendPacket(nid("b"), wg("over udp"))
	waitFor(t, "the packet arrives over UDP", func() bool { return boxes[b].has(nid("a"), string(wg("over udp"))) })
	if got := a.packetPeer(nid("b")).streams.Load(); got > streams {
		t.Fatalf("%d streams, before %d", got, streams)
	}

	a.packetPeer(nid("b")).direct.Load().Close()
	waitFor(t, "the path ends on b", func() bool { return !hasDirect(b, nid("a")) })
	waitFor(t, "the path ends on a", func() bool { return !hasDirect(a, nid("b")) })
	a.SendPacket(nid("b"), wg("back on ssh"))
	waitFor(t, "the packet arrives along the link", func() bool { return boxes[b].has(nid("a"), string(wg("back on ssh"))) })
	if hasDirect(a, nid("b")) {
		t.Fatal("a path came back at once")
	}
}

// Two nodes that have no link between them get a path too, and the
// machine between them sees no sign of it.
func TestDirectPathAcrossTheMesh(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	boxes := enableDirect(t, a, c)
	linkUp(t, a, b, "b")
	linkUp(t, c, b, "b")
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
// UDP path carries on, and they keep each other's records past their end.
func TestDirectPathOutlivesSSH(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	boxes := enableDirect(t, a, c)
	ab, cb := linkUp(t, a, b, "b"), linkUp(t, c, b, "b")
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

	a.mu.Lock()
	r := a.records[nid("c")]
	r.Expires = time.Now().Add(-time.Second)
	a.records[nid("c")] = r
	a.mu.Unlock()
	a.prune()
	if n, ok := findTopo(a.Topology(), nid("c")); !ok || !n.Direct {
		t.Fatalf("a lost c: %+v", n)
	}
}

// A node that removes its last link to the mesh leaves it, paths included.
func TestRemovingALinkEndsDirect(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	enableDirect(t, a, c)
	linkUp(t, a, b, "b")
	cb := linkUp(t, c, b, "b")
	goDirect(t, a, c)

	cb.cut()
	cb.wait(t)
	c.EndStrandedDirect()
	waitFor(t, "the path ends", func() bool { return !hasDirect(a, nid("c")) && !hasDirect(c, nid("a")) })
	waitFor(t, "a loses c", func() bool { n, _ := findTopo(a.Topology(), nid("c")); return !n.Reachable })
}

// The node at the other end of a removed link leaves as well, even if it
// had a path to another node.
func TestRemovingALinkEndsDirectAtTheOtherEnd(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	enableDirect(t, a, c)
	linkUp(t, a, b, "b")
	linkUp(t, b, c, "c")
	goDirect(t, a, c)

	b.Unlink(t.Context(), "c")
	waitFor(t, "the path ends", func() bool { return !hasDirect(a, nid("c")) && !hasDirect(c, nid("a")) })
}

// A node that SSH links still lead to stays in the mesh, so its paths do
// too.
func TestRemovingALinkKeepsDirectInTheMesh(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	enableDirect(t, a, c)
	linkUp(t, a, b, "b")
	linkUp(t, b, c, "c")
	linkUp(t, a, c, "c")
	waitFor(t, "c sees the link from a", func() bool {
		for _, e := range c.Topology().Edges {
			if e.From == nid("a") && e.To == nid("c") {
				return true
			}
		}
		return false
	})
	goDirect(t, a, c)

	b.Unlink(t.Context(), "c")
	if !hasDirect(a, nid("c")) || !hasDirect(c, nid("a")) {
		t.Fatal("the path ended with SSH links left")
	}
}

// When both nodes offer at the same time, one path results.
func TestDirectOffersCross(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	boxes := enableDirect(t, a, b)
	linkUp(t, a, b, "b")
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
	linkUp(t, a, b, "b")
	waitFor(t, "a sees b", func() bool { n, _ := findTopo(a.Topology(), nid("b")); return n.Reachable })
	a.SendPacket(nid("b"), wg("hello"))
	waitFor(t, "a holds off", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return time.Now().Before(a.directWaits[nid("b")].next)
	})
	waitFor(t, "the packet arrives", func() bool { return atB.has(nid("a"), string(wg("hello"))) })
	if hasDirect(a, nid("b")) {
		t.Fatal("a path without the other side")
	}
}

// A machine off the virtual network neither asks for a path nor takes one.
func TestDirectNeedsTheNetwork(t *testing.T) {
	a := newNode(t, "a")
	a.EnableDirect(t.Context(), loopback)
	if _, err := a.serveOffer(t.Context(), wire.DirectOffer{Node: nid("b")}); err == nil {
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
	if _, err := a.serveOffer(t.Context(), wire.DirectOffer{Node: nid("b")}); err == nil {
		t.Error("answered with UDP paths off")
	}
	enableDirect(t, a)
	for _, node := range []string{"", "nonsense", nid("a")} {
		if _, err := a.serveOffer(t.Context(), wire.DirectOffer{Node: node}); err == nil {
			t.Errorf("answered an offer from %q", node)
		}
	}
}

// A node reports the addresses of a path, and what both ends did to set
// it up.
func TestDirectReport(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	enableDirect(t, a, c)
	linkUp(t, a, b, "b")
	linkUp(t, c, b, "b")
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
	for _, want := range []string{"offering", "the answer offers", "the path works", "the other side confirmed"} {
		if !slices.ContainsFunc(opener, hasText(want)) {
			t.Fatalf("the offering side's trace lacks %q: %v", want, opener)
		}
	}
	for _, want := range []string{"offer over SSH", "answering with", "probe from", "the other side picked"} {
		if !slices.ContainsFunc(other, hasText(want)) {
			t.Fatalf("the answering side's trace lacks %q: %v", want, other)
		}
	}

	if p, err := a.Ping(t.Context(), "c"); err != nil || !p.Direct {
		t.Fatalf("ping: %+v, %v", p, err)
	}
	if p, err := a.Ping(t.Context(), "b"); err != nil || p.Direct {
		t.Fatalf("ping over SSH: %+v, %v", p, err)
	}
	if _, err := a.DirectReport(t.Context(), "a"); err == nil {
		t.Fatal("a report on this machine")
	}
}

func hasText(text string) func(wire.DirectEvent) bool {
	return func(e wire.DirectEvent) bool { return strings.Contains(e.Text, text) }
}
