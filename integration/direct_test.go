//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

// Machines a and c have no link between them, only links to b. A
// connection from a to c goes through b and sets up a direct link, which
// the next connections take. It carries on when b goes away, and ends
// when c removes its link to the mesh.
func TestDirectLink(t *testing.T) {
	t.Parallel()
	a, b, c := startNode(t), startNode(t), startNode(t)
	a.direct, c.direct = true, true
	link3(t, a, b, c)
	c.startEcho(7000)

	if out := a.mustSh(`printf hello | gst connect c 7000`); out != "reply:hello" {
		t.Fatalf("gst connect c 7000: %q", out)
	}
	eventually(t, 30*time.Second, "a did not go direct to c", func() bool {
		return strings.Contains(a.mustGst("status"), "online; direct")
	})
	cNode, topo := a.waitReachable("c")
	if got := path(topo, cNode); got != "a,c" {
		t.Fatalf("a reaches c by %s, want a,c", got)
	}
	if out := a.mustSh(`printf hello | gst connect c 7000`); out != "reply:hello" {
		t.Fatalf("gst connect c 7000 over the direct link: %q", out)
	}
	out := a.mustGst("ping", "c", "-c", "1", "-v")
	for _, want := range []string{"over a direct link", "Direct link with c: up, ", "the path works", "QUIC", "offer through the mesh"} {
		if !strings.Contains(out, want) {
			t.Fatalf("gst ping c -v lacks %q: %q", want, out)
		}
	}
	eventually(t, 30*time.Second, "b did not see the direct link", func() bool {
		all := b.mustGst("status", "--all")
		return (strings.Contains(all, "a->c (direct) ") || strings.Contains(all, "c->a (direct) ")) && strings.Contains(all, " -> ")
	})

	b.mustGst("down")
	a.waitLink("tob", "down")
	c.waitLink("tob", "down")
	if out := a.mustSh(`printf hello | gst connect c 7000`); out != "reply:hello" {
		t.Fatalf("gst connect c 7000 without b: %q", out)
	}
	b.mustGst("up")
	a.waitLink("tob", "running")
	c.waitLink("tob", "running")

	c.mustGst("remove", "tob")
	eventually(t, 30*time.Second, "the direct link outlived the removed link", func() bool {
		return strings.Contains(a.mustGst("status"), "offline") || !strings.Contains(a.mustGst("status"), " c ")
	})
}
