package mesh

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// echo serves an echo server on network and address in the test and returns
// its address.
func echo(t *testing.T, network, address string) string {
	t.Helper()
	ln, err := net.Listen(network, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// lanAddress returns a non-loopback IPv4 address of this machine, or skips.
func lanAddress(t *testing.T) string {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 address")
	return ""
}

func roundTrip(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != msg {
		t.Fatalf("echo = %q, %v; want %q", buf, err, msg)
	}
}

// chain returns nodes a, b, and c where a opened a link to b and b one to
// c, once every node knows the whole chain, with the links.
func chain(t *testing.T) (a, b, c *Node, ab, bc *pipeLink) {
	t.Helper()
	a, b, c = newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	ab = linkUp(t, a, b, "ab")
	bc = linkUp(t, b, c, "bc")
	waitFor(t, "a reaches c", func() bool { return reaches(a, "c", "a", "b", "c") })
	waitFor(t, "c reaches a", func() bool { return reaches(c, "a", "c", "b", "a") })
	return
}

func TestDialAlongPath(t *testing.T) {
	a, _, c, _, _ := chain(t)
	tcp := echo(t, "tcp", "127.0.0.1:0")
	sock := filepath.Join(shortDir(t), "e.sock")
	echo(t, "unix", sock)

	for _, tt := range []struct {
		name         string
		from         *Node
		ref          string
		network, dst string
	}{
		{"two hops by name", a, "c", "tcp", tcp},
		{"by link name and .gst", a, "BC.gst", "tcp", tcp}, // a link name that b published
		{"by id prefix", a, nid("c")[:6], "tcp", tcp},
		{"one hop", a, "b", "tcp", tcp},
		{"unix socket", a, "c", "unix", sock},
		{"the other way, against the links' directions", c, "a", "tcp", tcp},
		{"to itself", a, "a", "tcp", tcp},
		{"localhost", a, "c", "tcp", "localhost:" + tcp[strings.LastIndex(tcp, ":")+1:]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := tt.from.Dial(t.Context(), tt.ref, tt.network, tt.dst)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			roundTrip(t, conn, "hello "+tt.name)
			roundTrip(t, conn, "again")
		})
	}
}

func TestDialHalfClose(t *testing.T) {
	a, _, _, _, _ := chain(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		data, _ := io.ReadAll(c) // until the client ends its side
		got <- string(data)
		c.Write([]byte("bye"))
	}()
	conn, err := a.Dial(t.Context(), "c", "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("request"))
	conn.(interface{ CloseWrite() error }).CloseWrite()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	reply, err := io.ReadAll(conn)
	if err != nil || string(reply) != "bye" || <-got != "request" {
		t.Fatalf("reply = %q, %v", reply, err)
	}
}

func TestDialErrors(t *testing.T) {
	a, _, _, _, _ := chain(t)
	// A node that no link leads to.
	a.merge([]wire.Record{rec("far")})
	waitFor(t, "far is known", func() bool { _, ok := findTopo(a.Topology(), nid("far")); return ok })
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := closed.Addr().String()
	closed.Close()

	for _, tt := range []struct {
		name         string
		ref, network string
		address      string
		code         string
		msg          string
	}{
		{"unknown machine", "nowhere", "tcp", "localhost:1", wire.CodeNotFound, "no machine"},
		{"no route", "far", "tcp", "localhost:1", wire.CodeNotFound, "no route"},
		{"invalid target", "c", "tcp", "no-port", wire.CodeInvalidParams, "invalid target"},
		{"invalid network", "c", "udp", "localhost:1", wire.CodeInvalidParams, "invalid target"},
		{"relative socket", "c", "unix", "rel.sock", wire.CodeInvalidParams, "invalid target"},
		{"refused at the last hop", "c", "tcp", refused, wire.CodeFailed, "cannot connect"},
		{"refused on this node", "a", "tcp", refused, wire.CodeFailed, "cannot connect"},
		{"not an exit", "c", "tcp", "192.0.2.1:80", wire.CodeForbidden, "not an exit"},
		{"not an exit, a hostname", "c", "tcp", "example.com:80", wire.CodeForbidden, "not an exit"},
		{"this node is not an exit either", "a", "tcp", "192.0.2.1:80", wire.CodeForbidden, "not an exit"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := a.Dial(t.Context(), tt.ref, tt.network, tt.address)
			if err == nil {
				conn.Close()
				t.Fatal("Dial succeeded")
			}
			wantCode(t, err, tt.code)
			if !strings.Contains(err.Error(), tt.msg) {
				t.Fatalf("err = %v, want mention of %q", err, tt.msg)
			}
		})
	}
}

func TestExitAllowsAddressesOffTheMachine(t *testing.T) {
	a, b, c, _, _ := chain(t)
	lan := lanAddress(t)
	address := echo(t, "tcp", lan+":0")

	// Only the node that connects must be an exit; b merely passes the
	// connection on.
	if _, err := a.Dial(t.Context(), "c", "tcp", address); err == nil {
		t.Fatal("c connected off its machine without being an exit")
	} else {
		wantCode(t, err, wire.CodeForbidden)
	}
	b.Configure(&config.Node{ID: nid("b"), Name: "b", Exit: true})
	if _, err := a.Dial(t.Context(), "c", "tcp", address); err == nil {
		t.Fatal("b being an exit let c connect")
	}
	c.Configure(&config.Node{ID: nid("c"), Name: "c", Exit: true})
	conn, err := a.Dial(t.Context(), "c", "tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	roundTrip(t, conn, "exit")

	// b is an exit now, so it connects off its machine for a too.
	if _, err := a.Dial(t.Context(), "b", "tcp", address); err != nil {
		t.Fatal(err)
	}
}

func TestOnThisMachine(t *testing.T) {
	for _, tt := range []struct {
		network, address string
		want             bool
	}{
		{"unix", "/run/app.sock", true},
		{"tcp", "localhost:80", true},
		{"tcp", "LocalHost:80", true},
		{"tcp", "127.0.0.1:80", true},
		{"tcp", "127.0.0.2:80", true},
		{"tcp", "[::1]:80", true},
		{"tcp", "0.0.0.0:80", false},
		{"tcp", "10.0.0.1:80", false},
		{"tcp", "[2001:db8::1]:80", false},
		{"tcp", "example.com:80", false},
		{"tcp", "localhost.example.com:80", false},
		{"tcp", "localhost", false},
	} {
		if got := onThisMachine(wire.Target{Network: tt.network, Address: tt.address}); got != tt.want {
			t.Errorf("onThisMachine(%s %s) = %v, want %v", tt.network, tt.address, got, tt.want)
		}
	}
}

func TestOpenChecksPath(t *testing.T) {
	a := newNode(t, "a")
	local := wire.Target{Network: "tcp", Address: "localhost:1"}
	long := make([]string, maxPath+1)
	long[0] = nid("a")
	for i := 1; i < len(long); i++ {
		long[i] = nid("x")
	}
	for _, tt := range []struct {
		name   string
		node   *Node
		path   []string
		target wire.Target
		code   string
	}{
		{"empty path", a, nil, local, wire.CodeInvalidParams},
		{"does not start at the node", a, []string{nid("b")}, local, wire.CodeInvalidParams},
		{"too long", a, long, local, wire.CodeInvalidParams},
		{"no link to the next node", a, []string{nid("a"), nid("b")}, local, wire.CodeNotFound},
		{"bad target", a, []string{nid("a")}, wire.Target{Network: "tcp", Address: "x"}, wire.CodeInvalidParams},
		{"no identity", newNode(t, ""), []string{nid("a")}, local, wire.CodeFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := tt.node.open(t.Context(), tt.path, tt.target)
			if err == nil {
				conn.Close()
			}
			wantCode(t, err, tt.code)
		})
	}

	// The longest path is accepted: it is only the links that are missing.
	_, err := a.open(t.Context(), long[:maxPath], local)
	wantCode(t, err, wire.CodeNotFound)
}

func TestRelayEndsWhenALinkIsLost(t *testing.T) {
	a, _, _, _, bc := chain(t)
	tcp := echo(t, "tcp", "127.0.0.1:0")
	conn, err := a.Dial(t.Context(), "c", "tcp", tcp)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	roundTrip(t, conn, "before")

	bc.cut()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err = conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("read succeeded after the link was lost")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("the relayed connection stayed open after a link was lost")
	}
}

func TestDialHonorsContext(t *testing.T) {
	a, _, _, _, _ := chain(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	conn, err := a.Dial(ctx, "c", "tcp", "localhost:1")
	if err == nil {
		conn.Close()
		t.Fatal("Dial succeeded with a done context")
	}
}

func TestFindNode(t *testing.T) {
	node := func(id, name string) wire.TopoNode { return wire.TopoNode{ID: id, Name: name} }
	const (
		laptop, homelab, devbox, devbox2 = "aaaa0000000000000000000000000001", "bbbb0000000000000000000000000002", "cccc0000000000000000000000000003", "cccd0000000000000000000000000004"
	)
	topo := wire.Topology{
		Nodes: []wire.TopoNode{node(laptop, "laptop"), node(homelab, "homelab"), node(devbox, "devbox"), node(devbox2, "devbox"), node("dddd0000000000000000000000000005", "other"), node("dddd1111111111111111111111111111", "dup")},
		Edges: []wire.TopoEdge{
			{From: laptop, To: homelab, Link: "box"},   // laptop calls homelab "box"
			{From: devbox, To: homelab, Link: "box"},   // another node calls homelab "box" too: still one machine
			{From: laptop, To: devbox, Link: "work"},   // "work" names devbox from laptop
			{From: homelab, To: devbox2, Link: "work"}, // but another "work" names devbox2: ambiguous
			{From: laptop, To: laptop, Link: "other"},  // a link name does not beat a node name
			{From: laptop, To: "dddd1111111111111111111111111111", Link: "laptop"},
		},
	}
	byID := map[string]wire.TopoNode{}
	for _, n := range topo.Nodes {
		byID[n.ID] = n
	}
	for _, tt := range []struct {
		ref  string
		want string // id, or "" with code
		code string
	}{
		{laptop, laptop, ""},
		{"LAPTOP", laptop, ""},
		{"laptop.gst", laptop, ""}, // the node name wins over the link called laptop
		{"HOMELAB.GST", homelab, ""},
		{"box", homelab, ""}, // link name, published twice for the same node
		{"other", "dddd0000000000000000000000000005", ""},
		{"devbox", "", wire.CodeInvalidParams}, // two nodes of that name
		{"work", "", wire.CodeInvalidParams},   // two links of that name to different nodes
		{"aaaa", laptop, ""},
		{"bbbb0000", homelab, ""},
		{"ccc", "", wire.CodeNotFound}, // prefixes need four characters
		{"cccc", devbox, ""},
		{"ccc0", "", wire.CodeNotFound},
		{"dddd", "", wire.CodeInvalidParams}, // two ids share the prefix
		{"nowhere", "", wire.CodeNotFound},
		{"", "", wire.CodeNotFound},
	} {
		got, err := FindNode(topo, tt.ref)
		if tt.code != "" {
			wantCode(t, err, tt.code)
			continue
		}
		if err != nil || got.ID != tt.want {
			t.Errorf("FindNode(%q) = %v, %v; want %s", tt.ref, got.ID, err, tt.want)
		}
	}
}

func TestFindNodeIDBeatsEverything(t *testing.T) {
	id := "abcd0000000000000000000000000001"
	topo := wire.Topology{Nodes: []wire.TopoNode{{ID: id, Name: "x"}, {ID: "eeee0000000000000000000000000002", Name: id}}}
	if n, err := FindNode(topo, id); err != nil || n.ID != id {
		t.Fatalf("got %v, %v", n, err)
	}
}

// shortDir returns a short temporary directory, as Unix socket paths are limited.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gst")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestCallReachesAnyNode(t *testing.T) {
	status := func(name string) func() wire.Status {
		return func() wire.Status {
			return wire.Status{Software: "status of " + name, Links: []wire.LinkState{{Name: "to-" + name}}}
		}
	}
	a, b, c := newNodeStatus(t, "a", status("a")), newNodeStatus(t, "b", status("b")), newNodeStatus(t, "c", status("c"))
	linkUp(t, a, b, "ab")
	linkUp(t, b, c, "bc")
	waitFor(t, "a reaches c", func() bool { return reaches(a, "c", "a", "b", "c") })

	for _, tt := range []struct{ name, ref string }{
		{"itself", "a"},
		{"a neighbor", "b.gst"},
		{"two hops away", "c"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var st wire.Status
			if err := a.Call(t.Context(), tt.ref, wire.MethodPeerStatus, nil, &st); err != nil {
				t.Fatal(err)
			}
			name := strings.TrimSuffix(tt.ref, ".gst")
			if st.Software != "status of "+name || len(st.Links) != 1 || st.Links[0].Name != "to-"+name {
				t.Fatalf("status = %+v, want the status of %s", st, name)
			}
		})
	}

	// The connection is closed after the call; calls do not interfere.
	var st wire.Status
	for range 2 {
		if err := a.Call(t.Context(), "c", wire.MethodPeerStatus, nil, &st); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCallErrors(t *testing.T) {
	a := newNodeStatus(t, "a", func() wire.Status { return wire.Status{} })
	b := newNode(t, "b") // reports no status
	linkUp(t, a, b, "ab")
	waitFor(t, "a reaches b", func() bool { return reaches(a, "b", "a", "b") })
	a.merge([]wire.Record{rec("far")})
	waitFor(t, "far is known", func() bool { _, ok := findTopo(a.Topology(), nid("far")); return ok })

	for _, tt := range []struct {
		name, ref string
		code      string
	}{
		{"a node without a status", "b", wire.CodeUnsupportedMethod},
		{"unknown machine", "nowhere", wire.CodeNotFound},
		{"unreachable machine", "far", wire.CodeNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var st wire.Status
			wantCode(t, a.Call(t.Context(), tt.ref, wire.MethodPeerStatus, nil, &st), tt.code)
		})
	}

	// A done context ends the call.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.Call(ctx, "b", wire.MethodPeerStatus, nil, nil); err == nil {
		t.Fatal("Call succeeded with a done context")
	}
}

func TestOpenGstTarget(t *testing.T) {
	a, _, _, _, _ := chain(t)
	path := []string{nid("a")}

	// The target is the node itself, so no address is allowed.
	_, err := a.open(t.Context(), path, wire.Target{Network: wire.NetworkGst, Address: "localhost:1"})
	wantCode(t, err, wire.CodeInvalidParams)

	// The exit rule is about addresses to connect to: a node that is not
	// an exit still answers calls, here and at the end of a path.
	for _, ref := range []string{"a", "c"} {
		conn, err := a.Dial(t.Context(), ref, wire.NetworkGst, "")
		if err != nil {
			t.Fatalf("Dial %s: %v", ref, err)
		}
		conn.Close()
	}
	if self := a.Self(); self.Exit {
		t.Fatal("a is an exit; the test needs a node that is not")
	}
}

func TestPing(t *testing.T) {
	a, _, _, _, _ := chain(t)

	for _, tt := range []struct {
		name, ref string
		node      string
		via       []string
	}{
		{"itself", "a", "a", nil},
		{"a neighbor", "b.gst", "b", nil},
		{"two hops away", "c", "c", []string{"b"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := a.Ping(t.Context(), tt.ref)
			if err != nil {
				t.Fatal(err)
			}
			if p.Node.ID != nid(tt.node) || p.Node.Name != tt.node {
				t.Errorf("node = %+v, want %s", p.Node, tt.node)
			}
			if p.Via == nil || len(p.Via) != len(tt.via) {
				t.Fatalf("via = %+v, want %v", p.Via, tt.via)
			}
			for i, name := range tt.via {
				if p.Via[i].ID != nid(name) || p.Via[i].Name != name {
					t.Errorf("via[%d] = %+v, want %s", i, p.Via[i], name)
				}
			}
			if p.TimeMS <= 0 {
				t.Errorf("TimeMS = %v, want a positive time", p.TimeMS)
			}
		})
	}
}

func TestPingErrors(t *testing.T) {
	a, _, _, _, bc := chain(t)
	a.merge([]wire.Record{rec("far")})
	waitFor(t, "far is known", func() bool { _, ok := findTopo(a.Topology(), nid("far")); return ok })

	for _, tt := range []struct{ name, ref, code string }{
		{"unknown machine", "nowhere", wire.CodeNotFound},
		{"unreachable machine", "far", wire.CodeNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := a.Ping(t.Context(), tt.ref)
			wantCode(t, err, tt.code)
			if !strings.Contains(err.Error(), "gst status") {
				t.Errorf("error %q does not point to gst status", err)
			}
		})
	}

	bc.cut()
	waitFor(t, "c is offline", func() bool { return !reaches(a, "c", "a", "b", "c") })
	if _, err := a.Ping(t.Context(), "c"); err == nil {
		t.Fatal("Ping reached a machine without a running link")
	}
}
