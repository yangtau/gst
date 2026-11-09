package mesh

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

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

// answers checks that a stream reaches the node called name.
func answers(t *testing.T, conn net.Conn, name string) {
	t.Helper()
	var info wire.NodeInfo
	if err := wire.Call(conn, wire.MethodNodeInfo, nil, &info); err != nil {
		t.Fatal(err)
	}
	if info.Name != name {
		t.Fatalf("%q answered, want %q", info.Name, name)
	}
}

func TestDialAlongPath(t *testing.T) {
	a, _, c, _, _ := chain(t)
	for _, tt := range []struct {
		name string
		from *Node
		ref  string
		want string
	}{
		{"two hops by name", a, "c", "c"},
		{"by name and .gst", a, "C.gst", "c"},
		{"by id prefix", a, nid("c")[:6], "c"},
		{"one hop", a, "b", "b"},
		{"the other way, against the links' directions", c, "a", "a"},
		{"to itself", a, "a", "a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := tt.from.Dial(t.Context(), tt.ref)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			answers(t, conn, tt.want)
			answers(t, conn, tt.want) // One call after another.
		})
	}
}

func TestDialErrors(t *testing.T) {
	a, _, _, _, bc := chain(t)
	a.merge([]wire.Record{rec("far")})
	waitFor(t, "far is known", func() bool { _, ok := findTopo(a.Topology(), nid("far")); return ok })
	for _, tt := range []struct{ name, ref, code string }{
		{"unknown machine", "nowhere", wire.CodeNotFound},
		{"unreachable machine", "far", wire.CodeNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := a.Dial(t.Context(), tt.ref)
			wantCode(t, err, tt.code)
		})
	}
	bc.cut()
	waitFor(t, "c is offline", func() bool { return !reaches(a, "c", "a", "b", "c") })
	_, err := a.Dial(t.Context(), "c")
	wantCode(t, err, wire.CodeNotFound)
}

func TestOpenChecksPath(t *testing.T) {
	a := newNode(t, "a")
	long := make([]string, maxPath+1)
	long[0] = nid("a")
	for i := 1; i < len(long); i++ {
		long[i] = nid("x")
	}
	for _, tt := range []struct {
		name string
		node *Node
		path []string
		code string
	}{
		{"empty path", a, nil, wire.CodeInvalidParams},
		{"does not start at the node", a, []string{nid("b")}, wire.CodeInvalidParams},
		{"too long", a, long, wire.CodeInvalidParams},
		{"no link to the next node", a, []string{nid("a"), nid("b")}, wire.CodeNotFound},
		{"no identity", newNode(t, ""), []string{nid("a")}, wire.CodeFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := tt.node.open(t.Context(), tt.path)
			if err == nil {
				conn.Close()
			}
			wantCode(t, err, tt.code)
		})
	}

	// The longest path is accepted: it is only the links that are missing.
	_, err := a.open(t.Context(), long[:maxPath])
	wantCode(t, err, wire.CodeNotFound)
}

func TestRelayEndsWhenALinkIsLost(t *testing.T) {
	a, _, _, _, bc := chain(t)
	conn, err := a.Dial(t.Context(), "c")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	answers(t, conn, "c")

	bc.cut()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err = conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("read succeeded after the link was lost")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("the relayed stream stayed open after a link was lost")
	}
}

func TestDialHonorsContext(t *testing.T) {
	a, _, _, _, _ := chain(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	conn, err := a.Dial(ctx, "c")
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
	}
	for _, tt := range []struct {
		ref  string
		want string // id, or "" with code
		code string
	}{
		{laptop, laptop, ""},
		{"LAPTOP", laptop, ""},
		{"laptop.gst", laptop, ""},
		{"HOMELAB.GST", homelab, ""},
		{"other", "dddd0000000000000000000000000005", ""},
		{"devbox", "", wire.CodeInvalidParams}, // two nodes of that name
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

func TestCallReachesAnyNode(t *testing.T) {
	status := func(name string) func() wire.Status {
		return func() wire.Status {
			return wire.Status{Software: "status of " + name, Links: []wire.LinkState{{Destination: "to-" + name}}}
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
			if st.Software != "status of "+name || len(st.Links) != 1 || st.Links[0].Destination != "to-"+name {
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
