package mesh

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// nid returns a valid node id derived from a short name.
func nid(name string) string {
	h := hex.EncodeToString([]byte(name))
	return h + strings.Repeat("0", 32-len(h))
}

// newNode returns a node named name, which publishes its record until the
// test ends. An empty name gives a node without identity.
func newNode(t *testing.T, name string) *Node {
	t.Helper()
	return newNodeStatus(t, name, nil)
}

// newNodeStatus is newNode for a node that reports status to the others.
func newNodeStatus(t *testing.T, name string, status func() wire.Status) *Node {
	t.Helper()
	n := New("gst test", log.New(io.Discard, "", 0), status)
	if name != "" {
		n.Configure(&config.Node{ID: nid(name), Name: name})
	}
	go n.Run(t.Context())
	return n
}

func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out: " + msg)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// pipeLink is a link that client opened to server over an in-process pipe.
type pipeLink struct {
	cancel context.CancelFunc
	conn   net.Conn // the client's end
	remote net.Conn
	peer   chan wire.NodeInfo // receives the peer once the link runs
	done   chan error         // receives what Link returned
}

// openLink opens the link called name from client to server and returns
// without waiting for it.
func openLink(t *testing.T, client, server *Node, name string) *pipeLink {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	a, b := net.Pipe()
	go func() {
		theirs, err := wire.ReadHello(b)
		if err != nil {
			b.Close()
			return
		}
		server.Accept(ctx, b, theirs)
	}()
	l := &pipeLink{cancel: cancel, conn: a, remote: b, peer: make(chan wire.NodeInfo, 1), done: make(chan error, 1)}
	go func() { l.done <- client.Link(ctx, config.LinkID(name), a, func(p wire.NodeInfo) { l.peer <- p }) }()
	return l
}

// linkUp opens a link and waits until it runs.
func linkUp(t *testing.T, client, server *Node, name string) *pipeLink {
	t.Helper()
	l := openLink(t, client, server, name)
	select {
	case <-l.peer:
	case err := <-l.done:
		t.Fatalf("link %s ended: %v", name, err)
	case <-time.After(10 * time.Second):
		t.Fatalf("link %s did not come up", name)
	}
	return l
}

// cut breaks the connection under the link, as a lost SSH connection would.
func (l *pipeLink) cut() { l.conn.Close(); l.remote.Close() }

func (l *pipeLink) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-l.done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("link did not end")
		return nil
	}
}

// findTopo returns the node of topo with id, if any.
func findTopo(topo wire.Topology, id string) (wire.TopoNode, bool) {
	for _, n := range topo.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return wire.TopoNode{}, false
}

// reaches reports whether n's view has the node called name reachable
// with the given path (by names); a nil path accepts any.
func reaches(n *Node, name string, path ...string) bool {
	topo := n.Topology()
	node, ok := findTopo(topo, nid(name))
	// Until a node's record arrives, it is named after the link to it.
	if !ok || !node.Reachable || node.Name != name {
		return false
	}
	if path == nil {
		return true
	}
	if len(node.Path) != len(path) {
		return false
	}
	for i, id := range node.Path {
		if id != nid(path[i]) {
			return false
		}
	}
	return true
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var werr *wire.Error
	if !errors.As(err, &werr) || werr.Code != code {
		t.Fatalf("got error %v, want code %s", err, code)
	}
}

func TestLinkIntroducesNodes(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	l := openLink(t, a, b, "ab")
	var peer wire.NodeInfo
	select {
	case peer = <-l.peer:
	case err := <-l.done:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("link did not come up")
	}
	if peer.ID != nid("b") || peer.Name != "b" || peer.Software != "gst test" {
		t.Fatalf("peer = %+v", peer)
	}

	// Each side learns the other, and the link's name from the side that opened it.
	waitFor(t, "a reaches b", func() bool { return reaches(a, "b", "a", "b") })
	waitFor(t, "b reaches a", func() bool { return reaches(b, "a", "b", "a") })
	want := []wire.TopoEdge{{From: nid("a"), To: nid("b"), Link: config.LinkID("ab")}}
	for _, n := range []*Node{a, b} {
		waitFor(t, "edge", func() bool {
			edges := n.Topology().Edges
			return len(edges) == 1 && edges[0] == want[0]
		})
	}
	if topo := b.Topology(); topo.Self != nid("b") {
		t.Fatalf("self = %s", topo.Self)
	}
}

func TestLinkEndsWhenContextIsDone(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	l := linkUp(t, a, b, "ab")
	l.cancel()
	if err := l.wait(t); err != nil {
		t.Fatalf("Link returned %v after its context was done", err)
	}
	waitFor(t, "a drops the link", func() bool { return len(a.Topology().Edges) == 0 })
}

func TestLinkEndsWhenConnectionIsLost(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	l := linkUp(t, a, b, "ab")
	l.cut()
	if err := l.wait(t); err == nil {
		t.Fatal("Link returned nil for a lost connection")
	}
	waitFor(t, "a drops the link", func() bool { return len(a.Topology().Edges) == 0 && !reaches(a, "b") })
	waitFor(t, "b drops the link", func() bool { _, ok := b.sessionTo(nid("a")); return !ok })

	// A new link works again.
	linkUp(t, a, b, "ab2")
	waitFor(t, "a reaches b again", func() bool { return reaches(a, "b") })
}

func TestLinkStaleEdgeAfterLoss(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	l := linkUp(t, a, b, "ab")
	waitFor(t, "b reaches a", func() bool { return reaches(b, "a") })
	l.cut()
	waitFor(t, "b drops the link", func() bool { _, ok := b.sessionTo(nid("a")); return !ok })
	if reaches(b, "a") {
		t.Fatal("b still reaches a without a link")
	}
}

func TestLinkRefusals(t *testing.T) {
	for _, tt := range []struct {
		name           string
		client, server string
		want           string
		remote         bool // a *RemoteError
	}{
		{"server without identity", "a", "", "no identity", true},
		{"self", "a", "a", "this machine", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := openLink(t, newNode(t, tt.client), newNode(t, tt.server), "x")
			err := l.wait(t)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %q", err, tt.want)
			}
			var remote *RemoteError
			if errors.As(err, &remote) != tt.remote {
				t.Fatalf("RemoteError = %v, want %v (%v)", !tt.remote, tt.remote, err)
			}
		})
	}
}

func TestAcceptRefusesPeerWithoutYamux(t *testing.T) {
	n := newNode(t, "a")
	c, s := net.Pipe()
	defer c.Close()
	go func() {
		theirs, err := wire.ReadHello(s)
		if err == nil {
			n.Accept(t.Context(), s, theirs)
		}
	}()
	_, err := wire.Dial(c, wire.Hello{Software: "old", Role: wire.RolePeer})
	wantCode(t, err, wire.CodeUnsupportedRole)
}

// failConn is a connection whose remote gst failed with a message, as an
// sshconn.Session reports it.
type failConn struct {
	net.Conn
	text string
}

func (c failConn) Failure() string { return c.text }

func TestLinkReportsFailureOfRemoteGst(t *testing.T) {
	for _, tt := range []struct {
		name   string
		text   string
		remote bool
		want   string
	}{
		{"failure text", "gst: command not found", true, "the remote gst failed: gst: command not found"},
		{"no text", "", false, "did not answer"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			x, y := net.Pipe()
			y.Close()
			err := newNode(t, "a").Link(t.Context(), "x", failConn{x, tt.text}, nil)
			var remote *RemoteError
			if err == nil || !strings.Contains(err.Error(), tt.want) || errors.As(err, &remote) != tt.remote {
				t.Fatalf("err = %v (RemoteError %v), want %q", err, errors.As(err, &remote), tt.want)
			}
		})
	}
}

func TestLinkStopsWhileWaitingForAnswer(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- newNode(t, "a").Link(ctx, "x", a, nil) }()
	// The remote end reads the Hello and never answers.
	if _, err := wire.ReadHello(b); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Link returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Link did not stop")
	}
}

// fakePeer accepts one link on conn as a peer that answers node.info with info.
func fakePeer(t *testing.T, conn net.Conn, info wire.NodeInfo) {
	t.Helper()
	theirs, err := wire.ReadHello(conn)
	if err != nil {
		t.Error(err)
		return
	}
	mine := wire.Hello{Software: "fake", Role: wire.RolePeer, Features: []string{wire.FeatureYamux}}
	if err := wire.Answer(conn, theirs, mine, nil); err != nil {
		t.Error(err)
		return
	}
	session, err := yamux.Server(conn, yamuxConfig())
	if err != nil {
		t.Error(err)
		return
	}
	defer session.Close()
	var server wire.Server
	wire.Handle(&server, wire.MethodNodeInfo, func(context.Context, struct{}) (any, error) { return info, nil })
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		go server.Serve(t.Context(), stream)
	}
}

func TestLinkRejectsInvalidPeerID(t *testing.T) {
	a, b := net.Pipe()
	go fakePeer(t, b, wire.NodeInfo{ID: "not-an-id", Name: "x"})
	err := newNode(t, "a").Link(t.Context(), "x", a, nil)
	if err == nil || !strings.Contains(err.Error(), "valid node id") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigure(t *testing.T) {
	t.Run("without identity", func(t *testing.T) {
		n := newNode(t, "")
		if n.Self().ID != "" {
			t.Fatalf("self = %+v", n.Self())
		}
		topo := n.Topology()
		if len(topo.Nodes) != 0 || len(topo.Edges) != 0 || topo.Nodes == nil || topo.Edges == nil {
			t.Fatalf("topology = %+v", topo)
		}
		n.Configure(&config.Node{ID: nid("a"), Name: "a"})
		if got := n.Self(); got.ID != nid("a") || got.Name != "a" || got.Software != "gst test" {
			t.Fatalf("self = %+v", got)
		}
		n.Configure(nil)
		if n.Self().ID != "" {
			t.Fatalf("self = %+v after Configure(nil)", n.Self())
		}
	})

	t.Run("same identity keeps links and publishes changes", func(t *testing.T) {
		a, b := newNode(t, "a"), newNode(t, "b")
		l := linkUp(t, a, b, "ab")
		b.Configure(&config.Node{ID: nid("b"), Name: "b2"})
		waitFor(t, "a sees the new name and exit", func() bool {
			// a's own view of b comes from b's record.
			n, ok := findTopo(a.Topology(), nid("b"))
			return ok && n.Name == "b2" && n.Reachable
		})
		select {
		case err := <-l.done:
			t.Fatalf("link ended: %v", err)
		default:
		}
	})

	t.Run("new identity forgets records and closes links", func(t *testing.T) {
		a, b := newNode(t, "a"), newNode(t, "b")
		l := linkUp(t, a, b, "ab")
		waitFor(t, "b knows a", func() bool { return reaches(b, "a") })
		b.Configure(&config.Node{ID: nid("c"), Name: "c"})
		if err := l.wait(t); err == nil {
			t.Fatal("link survived a new identity")
		}
		topo := b.Topology()
		if len(topo.Nodes) != 1 || topo.Nodes[0].ID != nid("c") {
			t.Fatalf("topology after new identity = %+v", topo)
		}
	})
}

func TestRecordsExpireAndRefresh(t *testing.T) {
	a := New("gst test", log.New(io.Discard, "", 0), nil)
	a.ttl = 150 * time.Millisecond
	a.Configure(&config.Node{ID: nid("a"), Name: "a"})
	runCtx, stopRun := context.WithCancel(t.Context())
	defer stopRun()
	go a.Run(runCtx)
	b := newNode(t, "b")
	linkUp(t, a, b, "ab")
	waitFor(t, "b knows a", func() bool { return reaches(b, "a") })

	seq := func() uint64 {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.records[nid("a")].Seq
	}
	// While a runs it refreshes its record, which reaches b.
	first := seq()
	waitFor(t, "a refreshes its record", func() bool { return seq() > first })
	waitFor(t, "a still known", func() bool { return reaches(b, "a") })

	// Once a stops publishing, its record expires from b's view.
	stopRun()
	waitFor(t, "a's record expires", func() bool {
		_, ok := findTopo(b.Topology(), nid("a"))
		return !ok
	})
}

func TestOwnOldRecordIsOutdone(t *testing.T) {
	a := newNode(t, "a")
	waitFor(t, "a publishes", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.records[nid("a")]
		return ok
	})
	// An earlier run of this daemon published a record with a larger Seq.
	old := uint64(1) << 62
	a.merge([]wire.Record{{Node: wire.NodeInfo{ID: nid("a"), Name: "a"}, Seq: old, Expires: time.Now().Add(time.Hour)}})
	waitFor(t, "a outdoes the old record", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.records[nid("a")].Seq > old
	})
}

func TestUnknownPeerMethod(t *testing.T) {
	n := newNode(t, "a")
	a, b := net.Pipe()
	defer a.Close()
	go n.peer.Serve(t.Context(), b)
	wantCode(t, wire.Call(a, "mesh.unknown", nil, nil), wire.CodeUnsupportedMethod)
	// The stream keeps working.
	var info wire.NodeInfo
	if err := wire.Call(a, wire.MethodNodeInfo, nil, &info); err != nil || info.ID != nid("a") {
		t.Fatalf("node.info = %+v, %v", info, err)
	}
}
