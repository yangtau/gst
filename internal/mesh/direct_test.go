package mesh

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func loopback() []netip.Addr { return []netip.Addr{netip.MustParseAddr("127.0.0.1")} }

// enableDirect lets nodes use direct links on the loopback interface.
func enableDirect(t *testing.T, nodes ...*Node) {
	t.Helper()
	for _, n := range nodes {
		n.EnableDirect(t.Context(), loopback)
	}
}

func hasDirect(n *Node, id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.directLink(id) != nil
}

// echoServer returns the address of a TCP server that echoes until it
// sees the end of the input.
func echoServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	return l.Addr().String()
}

// echoes checks that a connection through the mesh reaches the server.
func echoes(t *testing.T, conn net.Conn) {
	t.Helper()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "hello" {
		t.Fatalf("read %q, %v", got, err)
	}
}

func dial(t *testing.T, n *Node, ref, address string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := n.Dial(ctx, ref, "tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// A connection between two machines sets up a direct link. It keeps its
// way, the next ones take the direct link, and everything keeps working
// when the direct link goes.
func TestDirectLink(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	enableDirect(t, a, b)
	linkUp(t, a, b, "b")
	waitFor(t, "a sees b", func() bool { n, _ := findTopo(a.Topology(), nid("b")); return n.Reachable })
	address := echoServer(t)

	first := dial(t, a, "b", address)
	echoes(t, first)
	waitFor(t, "the direct link runs on a", func() bool { return hasDirect(a, nid("b")) })
	waitFor(t, "the direct link runs on b", func() bool { return hasDirect(b, nid("a")) })
	echoes(t, first)

	second := dial(t, a, "b", address)
	echoes(t, second)
	if _, ok := second.(interface{ CloseWrite() error }); !ok {
		t.Fatal("no CloseWrite on the connection")
	}
	a.mu.Lock()
	var l *link
	for x := range a.links {
		if x.direct {
			l = x
		}
	}
	a.mu.Unlock()
	if a.sessionTo(nid("b")) != l.session {
		t.Fatal("the direct link is not preferred")
	}

	node, _ := findTopo(a.Topology(), nid("b"))
	if !node.Direct {
		t.Fatal("the topology does not show the direct link")
	}
	waitFor(t, "b sees a's direct edge", func() bool {
		for _, e := range b.Topology().Edges {
			if e.From == nid("a") && e.To == nid("b") && e.Direct {
				return true
			}
		}
		return false
	})

	// The connection over SSH stays when the direct link fails.
	l.session.Close()
	waitFor(t, "the direct link ends on b", func() bool { return !hasDirect(b, nid("a")) })
	echoes(t, first)
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatal("the connection over the direct link survived")
	}
	// New connections go over SSH, and the failure holds off a new try.
	third := dial(t, a, "b", address)
	echoes(t, third)
	if hasDirect(a, nid("b")) {
		t.Fatal("a direct link came back at once")
	}
}

// Two nodes that have no link between them go direct too, and are a link
// apart from then on.
func TestDirectLinkAcrossTheMesh(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	enableDirect(t, a, b, c)
	linkUp(t, a, b, "b")
	linkUp(t, c, b, "b")
	waitFor(t, "a reaches c", func() bool { n, _ := findTopo(a.Topology(), nid("c")); return n.Reachable })
	address := echoServer(t)
	echoes(t, dial(t, a, "c", address))
	waitFor(t, "a goes straight to c", func() bool {
		n, _ := findTopo(a.Topology(), nid("c"))
		return len(n.Path) == 2 && n.Direct
	})
	waitFor(t, "c goes straight to a", func() bool {
		n, _ := findTopo(c.Topology(), nid("a"))
		return len(n.Path) == 2 && n.Direct
	})
	echoes(t, dial(t, a, "c", address))

	// The node in between sees the link as well.
	waitFor(t, "b sees the link", func() bool {
		for _, e := range b.Topology().Edges {
			if e.Direct {
				return true
			}
		}
		return false
	})
}

// brokenSession is a direct link that has just broken without anyone
// noticing: it opens streams, and calls on them fail as the session ends.
type brokenSession struct {
	done chan struct{}
}

func (s brokenSession) Open(context.Context) (net.Conn, error) {
	ours, theirs := net.Pipe()
	go func() {
		io.Copy(io.Discard, theirs)
	}()
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(s.done)
		theirs.Close()
	}()
	return ours, nil
}

func (s brokenSession) Accept(ctx context.Context) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (s brokenSession) Close() error          { return nil }
func (s brokenSession) Done() <-chan struct{} { return s.done }

// A connection that picked a direct link just as it broke takes the SSH
// link instead of failing.
func TestRelayLeavesBrokenDirectLink(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	linkUp(t, a, b, "b")
	waitFor(t, "a sees b", func() bool { n, _ := findTopo(a.Topology(), nid("b")); return n.Reachable })
	a.mu.Lock()
	a.links[&link{peer: wire.NodeInfo{ID: nid("b")}, direct: true, session: brokenSession{make(chan struct{})}}] = struct{}{}
	a.mu.Unlock()
	echoes(t, dial(t, a, "b", echoServer(t)))
}

// goDirect sets up a direct link between a and c, which reach each other.
func goDirect(t *testing.T, a, c *Node) {
	t.Helper()
	waitFor(t, "a reaches c", func() bool { n, _ := findTopo(a.Topology(), nid("c")); return n.Reachable })
	a.tryDirect(nid("c"))
	waitFor(t, "the direct link runs", func() bool { return hasDirect(a, nid("c")) && hasDirect(c, nid("a")) })
}

// When the machine between two nodes goes away, as when it sleeps, their
// direct link carries on, and the nodes stay in the mesh.
func TestDirectLinkOutlivesSSH(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	enableDirect(t, a, c)
	ab, cb := linkUp(t, a, b, "b"), linkUp(t, c, b, "b")
	goDirect(t, a, c)

	ab.cut()
	cb.cut()
	ab.wait(t)
	cb.wait(t)
	waitFor(t, "a loses b", func() bool { n, _ := findTopo(a.Topology(), nid("b")); return !n.Reachable })
	// c's record, which lists the link, may still be on its way.
	waitFor(t, "a reaches c", func() bool { n, _ := findTopo(a.Topology(), nid("c")); return n.Reachable })
	if !hasDirect(a, nid("c")) || !hasDirect(c, nid("a")) {
		t.Fatal("the direct link ended with the SSH links")
	}
	echoes(t, dial(t, a, "c", echoServer(t)))
}

// A node that removes its last link to the mesh leaves it, direct links
// included.
func TestRemovingALinkEndsDirect(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	enableDirect(t, a, c)
	linkUp(t, a, b, "b")
	cb := linkUp(t, c, b, "b")
	goDirect(t, a, c)

	cb.cut()
	cb.wait(t)
	c.EndStrandedDirect()
	waitFor(t, "the direct link ends", func() bool { return !hasDirect(a, nid("c")) && !hasDirect(c, nid("a")) })
	waitFor(t, "a loses c", func() bool { n, _ := findTopo(a.Topology(), nid("c")); return !n.Reachable })
}

// The node at the other end of a removed link leaves as well, even if it
// had a direct link to another node.
func TestRemovingALinkEndsDirectAtTheOtherEnd(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	enableDirect(t, a, c)
	linkUp(t, a, b, "b")
	linkUp(t, b, c, "c")
	goDirect(t, a, c)

	b.Unlink(t.Context(), "c")
	waitFor(t, "the direct link ends", func() bool { return !hasDirect(a, nid("c")) && !hasDirect(c, nid("a")) })
}

// A node that SSH links still lead to stays in the mesh, so its direct
// links do too.
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
		t.Fatal("the direct link ended with SSH links left")
	}
}

// When both nodes offer at the same time, one link results.
func TestDirectOffersCross(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	enableDirect(t, a, b)
	linkUp(t, a, b, "b")
	waitFor(t, "they see each other", func() bool {
		x, _ := findTopo(a.Topology(), nid("b"))
		y, _ := findTopo(b.Topology(), nid("a"))
		return x.Reachable && y.Reachable
	})
	go a.tryDirect(nid("b"))
	go b.tryDirect(nid("a"))
	waitFor(t, "the link runs", func() bool { return hasDirect(a, nid("b")) && hasDirect(b, nid("a")) })
	time.Sleep(200 * time.Millisecond)
	a.mu.Lock()
	defer a.mu.Unlock()
	count := 0
	for l := range a.links {
		if l.direct {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d direct links", count)
	}
}

// A node with direct links off refuses, and the other waits before it
// tries again.
func TestDirectOffOnTheOtherSide(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	enableDirect(t, a)
	linkUp(t, a, b, "b")
	waitFor(t, "a sees b", func() bool { n, _ := findTopo(a.Topology(), nid("b")); return n.Reachable })
	echoes(t, dial(t, a, "b", echoServer(t)))
	waitFor(t, "a holds off", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return time.Now().Before(a.directWaits[nid("b")].next)
	})
	if hasDirect(a, nid("b")) {
		t.Fatal("a direct link without the other side")
	}
}

func TestDirectOfferInvalid(t *testing.T) {
	a := newNode(t, "a")
	if _, err := a.serveOffer(t.Context(), wire.DirectOffer{Node: nid("b")}); err == nil {
		t.Error("answered with direct links off")
	}
	enableDirect(t, a)
	for _, node := range []string{"", "nonsense", nid("a")} {
		if _, err := a.serveOffer(t.Context(), wire.DirectOffer{Node: node}); err == nil {
			t.Errorf("answered an offer from %q", node)
		}
	}
}
