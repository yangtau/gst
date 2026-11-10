//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Machines a and c have no link between them, only links to b. Packets
// from a to c start through b and set up a UDP path, which takes over. It
// carries on when b goes away, and ends when c leaves the network.
func TestUDPPath(t *testing.T) {
	t.Parallel()
	a, b, c := startNode(t), startNode(t), startNode(t)
	a.direct, c.direct = true, true
	_, _, _, ab, cb := link3(t, a, b, c)
	for _, n := range []*node{a, b, c} {
		n.installHelper()
	}
	a.waitNet(2)
	address := c.waitNet(2).Address

	a.waitPing(address)
	a.waitPath("c", wire.WayDirect, 30*time.Second)
	c.waitPath("a", wire.WayDirect, 30*time.Second)
	eventually(t, 10*time.Second, "gst status does not show the path", func() bool {
		return strings.Contains(a.mustGst("status"), " direct")
	})
	// The path carries packets only, so the links still run through b.
	cNode, topo := a.waitReachable("c")
	if got := path(topo, cNode); got != "a,b,c" {
		t.Fatalf("a reaches c by %s, want a,b,c", got)
	}
	out := a.mustGst("ping", "c", "-c", "1", "-v")
	for _, want := range []string{"): direct ", "SSH links: c answered via b in ", "UDP path to c: up, ", "UDP sockets: port ", "Packets lost: ", "the path works", "offering over SSH"} {
		if !strings.Contains(out, want) {
			t.Fatalf("gst ping c -v lacks %q: %q", want, out)
		}
	}

	b.mustGst("down")
	a.waitLink(ab.destination(), "down")
	c.waitLink(cb.destination(), "down")
	a.waitPing(address)
	if out := a.mustGst("status"); !strings.Contains(out, " direct") {
		t.Fatalf("gst status without b:\n%s", out)
	}
	b.mustGst("up")
	a.waitLink(ab.destination(), "running")
	c.waitLink(cb.destination(), "running")

	// Unlinking on c changes nothing for the path: c stays in the network.
	c.mustGst("unlink", cb.destination())
	a.waitUnreachable("c")
	a.waitPing(address)
	c.mustGst("add", cb.destination())
	a.waitReachable("c")

	c.mustGst("remove", "c")
	eventually(t, 30*time.Second, "the path outlived the machine's leaving", func() bool {
		return !strings.Contains(a.mustGst("status"), " direct")
	})
}
