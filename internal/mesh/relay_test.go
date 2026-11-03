package mesh

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// echoServer answers each connection with "reply:" and what it received
// until the client half-closed.
func echoServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				data, _ := io.ReadAll(c)
				c.Write(append([]byte("reply:"), data...))
			}()
		}
	}()
	return l.Addr().String()
}

func roundtrip(t *testing.T, c net.Conn, payload string) string {
	t.Helper()
	defer c.Close()
	if _, err := c.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	// TCP connections half-close; yamux streams half-close on Close and
	// can still read.
	if hc, ok := c.(interface{ CloseWrite() error }); ok {
		hc.CloseWrite()
	} else {
		c.Close()
	}
	data, err := io.ReadAll(c)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return string(data)
}

// mini reaches devcn through m3, which links to both.
func TestRelayThroughPeer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m3, mini, devcn := newNode("m3"), newNode("mini"), newNode("devcn")
	for _, n := range []*Node{m3, mini, devcn} {
		go n.Run(ctx)
		n.Update()
	}
	linkTunnel(ctx, m3, mini, "mini", "")
	linkTunnel(ctx, m3, devcn, "devcn", "")
	waitFor(t, "mini does not see devcn", func() bool {
		path, err := Route(mini.Topology(), devcn.self().ID)
		return err == nil && len(path) == 3
	})

	addr := echoServer(t)
	for _, target := range []string{"devcn", "m3", "mini", devcn.self().ID} {
		c, err := mini.Dial(ctx, target, "tcp", addr)
		if err != nil {
			t.Fatalf("dial %s: %v", target, err)
		}
		if got := roundtrip(t, c, "hi "+target); got != "reply:hi "+target {
			t.Fatalf("through %s: %q", target, got)
		}
	}

	// Failures name the machine that failed.
	if _, err := mini.Dial(ctx, "devcn", "tcp", "127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "devcn cannot connect") {
		t.Fatalf("refused target: %v", err)
	}
	for _, tt := range []struct{ node, network, address, want string }{
		{"nowhere", "tcp", addr, "no machine"},
		{"devcn", "udp", addr, "invalid target udp"},
		{"devcn", "tcp", "no-port", "invalid target tcp"},
	} {
		if _, err := mini.Dial(ctx, tt.node, tt.network, tt.address); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("dial %s %s %s: %v, want %q", tt.node, tt.network, tt.address, err, tt.want)
		}
	}

	// A relay path must start at the node that receives it.
	if _, err := devcn.open(ctx, []string{m3.self().ID}, wire.Target{Network: "tcp", Address: addr}); err == nil {
		t.Fatal("devcn followed a path of m3")
	}
}

func TestRoute(t *testing.T) {
	topo := wire.Topology{Self: "mini", Nodes: []wire.TopoNode{
		{ID: "mini", Name: "mini"}, {ID: "m3", Name: "m3"}, {ID: "devcn", Name: "devcn"},
		{ID: "ssh:db", Name: "db"}, {ID: "far", Name: "far"}, {ID: "w1", Name: "web"}, {ID: "w2", Name: "web"},
	}, Edges: []wire.TopoEdge{
		{From: "m3", To: "mini", State: wire.StateRunning, Linked: true},
		// m3 reaches devcn, but without a peer link: devcn can be the end of
		// a path but cannot relay to far.
		{From: "m3", To: "devcn", State: wire.StateRunning},
		{From: "devcn", To: "far", State: wire.StateRunning, Linked: true},
		{From: "m3", To: "ssh:db", State: wire.StateRunning},
		{From: "mini", To: "w1", State: wire.StateDown},
	}}
	for _, tt := range []struct{ to, want string }{
		{"mini", "mini"},
		{"m3", "mini m3"},
		{"devcn", "mini m3 devcn"},
		{"ssh:db", "mini m3 ssh:db"},
		{"far", "error"},
		{"w1", "error"},
	} {
		path, err := Route(topo, tt.to)
		got := strings.Join(path, " ")
		if err != nil {
			got = "error"
		}
		if got != tt.want {
			t.Errorf("route to %s: %s (%v), want %s", tt.to, got, err, tt.want)
		}
	}

	for ref, want := range map[string]string{"db": "ssh:db", "m3": "m3", "ssh:db": "ssh:db"} {
		if n, err := FindNode(topo, ref); err != nil || n.ID != want {
			t.Errorf("find %s: %+v %v", ref, n, err)
		}
	}
	if _, err := FindNode(topo, "web"); err == nil || !strings.Contains(err.Error(), "several machines") {
		t.Errorf("ambiguous name: %v", err)
	}
}

// When the peer link is lost, both ends of a relayed connection close,
// even if neither side ever ends its direction.
func TestRelayEndsWithLink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m3, devcn := newNode("m3"), newNode("devcn")
	for _, n := range []*Node{m3, devcn} {
		go n.Run(ctx)
		n.Update()
	}
	linkTunnel(ctx, m3, devcn, "devcn", "")
	waitFor(t, "m3 does not see devcn", func() bool {
		path, err := Route(m3.Topology(), devcn.self().ID)
		return err == nil && len(path) == 2
	})

	// The target never writes and never closes.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	targetClosed := make(chan struct{})
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(io.Discard, c)
		close(targetClosed)
	}()

	c, err := m3.Dial(ctx, "devcn", "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	read := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 1))
		read <- err
	}()
	devcn.Configure(nil) // closes the link
	select {
	case err := <-read:
		if err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("read after the link was lost: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the connection outlived its peer link")
	}
	select {
	case <-targetClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("the target connection outlived the peer link")
	}
}
