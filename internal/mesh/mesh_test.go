package mesh

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/mesh/state"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// nid returns a valid node id derived from a short name.
func nid(name string) string {
	h := hex.EncodeToString([]byte(name))
	return h + strings.Repeat("0", 32-len(h))
}

// newNode returns a node named name, which runs until the test ends. An
// empty name gives a node without identity.
func newNode(t *testing.T, name string) *Node {
	t.Helper()
	return newNodeConfig(t, name, Config{})
}

func newNodeConfig(t *testing.T, name string, cfg Config) *Node {
	t.Helper()
	cfg.Software = "gst test"
	cfg.Log = log.New(io.Discard, "", 0)
	n := New(cfg)
	if name != "" {
		n.Configure(&config.Node{ID: nid(name), Name: name})
	}
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		n.Run(t.Context())
	}()
	// Its last save before the test's directories go.
	t.Cleanup(func() { <-ran })
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

// openLink opens a link from client to server and returns without
// waiting for it.
func openLink(t *testing.T, client, server *Node, admit bool) *pipeLink {
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
	go func() { l.done <- client.Link(ctx, admit, a, func(p wire.NodeInfo) { l.peer <- p }) }()
	return l
}

// linkUp opens a link and waits until it runs.
func linkUp(t *testing.T, client, server *Node) *pipeLink {
	t.Helper()
	return waitUp(t, openLink(t, client, server, false))
}

func waitUp(t *testing.T, l *pipeLink) *pipeLink {
	t.Helper()
	select {
	case <-l.peer:
	case err := <-l.done:
		t.Fatalf("link ended: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("link did not come up")
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

// reaches reports whether n reaches the node called name, along the given
// path (by names); a nil path accepts any.
func reaches(n *Node, name string, path ...string) bool {
	node, ok := findTopo(n.Topology(), nid(name))
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

// isMember reports whether n lists the node called name.
func isMember(n *Node, name string) bool {
	_, ok := findTopo(n.Topology(), nid(name))
	return ok
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var werr *wire.Error
	if !errors.As(err, &werr) || werr.Code != code {
		t.Fatalf("got error %v, want code %s", err, code)
	}
}

// inbox collects the packets that a node receives.
type inbox struct {
	mu  sync.Mutex
	got map[string]bool // "node packet"
}

func (i *inbox) handle(node string, packet []byte) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.got == nil {
		i.got = map[string]bool{}
	}
	i.got[node+" "+string(packet)] = true
}

func (i *inbox) has(node, packet string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.got[node+" "+packet]
}

// Links introduce the nodes to each other, and messages find their way
// along them.
func TestLinkIntroducesNodes(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	linkUp(t, a, b)
	linkUp(t, c, b)
	waitFor(t, "a reaches c through b", func() bool { return reaches(a, "c", "a", "b", "c") })
	waitFor(t, "c reaches a through b", func() bool { return reaches(c, "a", "c", "b", "a") })
	topo := a.Topology()
	if topo.Nodes[0].ID != nid("a") || len(topo.Nodes) != 3 {
		t.Fatalf("nodes %+v", topo.Nodes)
	}
	if want := []wire.TopoEdge{{From: nid("a"), To: nid("b")}, {From: nid("c"), To: nid("b")}}; !equalEdges(topo.Edges, want) {
		t.Fatalf("edges %+v", topo.Edges)
	}
	if n, _ := findTopo(topo, nid("c")); n.Seen.IsZero() {
		t.Fatal("no time c was seen")
	}
	p, err := a.Ping(t.Context(), "c")
	if err != nil || p.Node.ID != nid("c") || len(p.Via) != 1 || p.Via[0].Name != "b" {
		t.Fatalf("ping %+v, %v", p, err)
	}
}

func equalEdges(a, b []wire.TopoEdge) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Of the shortest ways, messages take the one whose first hop has the
// lower id.
func TestRoutesTakeTheShortestWay(t *testing.T) {
	a, b, c, d := newNode(t, "a"), newNode(t, "b"), newNode(t, "c"), newNode(t, "d")
	linkUp(t, a, b)
	linkUp(t, a, c)
	linkUp(t, b, d)
	linkUp(t, c, d)
	waitFor(t, "a reaches d through b", func() bool { return reaches(a, "d", "a", "b", "d") })
	waitFor(t, "d reaches a through b", func() bool { return reaches(d, "a", "d", "b", "a") })
	linkUp(t, a, d)
	waitFor(t, "a reaches d directly", func() bool { return reaches(a, "d", "a", "d") })
}

// A member stays one while it is offline.
func TestOfflineMembersStay(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	l := linkUp(t, a, b)
	waitFor(t, "a has b's entry", func() bool { _, ok := a.table.Get(state.NodeKey(nid("b"))); return ok })
	l.cut()
	l.wait(t)
	waitFor(t, "b is offline", func() bool {
		n, ok := findTopo(a.Topology(), nid("b"))
		return ok && !n.Online() && n.Name == "b"
	})
	err := a.Call(t.Context(), "b", wire.MethodPeerPing, nil, nil)
	wantCode(t, err, wire.CodeUnreachable)
	if !strings.Contains(err.Error(), "b is offline") {
		t.Fatalf("error %v", err)
	}
}

// Without a UDP path, packets go along the links, also through a machine
// in between.
func TestPacketsAlongTheMesh(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	linkUp(t, a, b)
	linkUp(t, c, b)
	waitFor(t, "a reaches c", func() bool { return reaches(a, "c", "a", "b", "c") })
	waitFor(t, "c reaches a", func() bool { return reaches(c, "a", "c", "b", "a") })
	var atA, atC inbox
	a.HandlePackets(atA.handle)
	c.HandlePackets(atC.handle)
	a.SendPacket(nid("c"), []byte("hello"))
	waitFor(t, "c gets the packet", func() bool { return atC.has(nid("a"), "hello") })
	if got := a.PacketPath(nid("c")); got != wire.PathSSH {
		t.Fatalf("path = %q", got)
	}
	c.SendPacket(nid("a"), []byte("back"))
	waitFor(t, "a gets the packet", func() bool { return atA.has(nid("c"), "back") })
	if got := a.PacketPath(nid("zz")); got != "" {
		t.Fatalf("path to nowhere = %q", got)
	}
}

// Removing a machine takes it out of every node's view, ends its links,
// and tells it, so that it leaves.
func TestRemove(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	linkUp(t, a, b)
	cb := linkUp(t, c, b)
	waitFor(t, "a reaches c", func() bool { return reaches(a, "c") })

	r, err := a.Remove("b")
	if err != nil || r.Node.Name != "b" || r.Self || len(r.Stranded) != 1 || r.Stranded[0].Name != "c" {
		t.Fatalf("removal %+v, %v", r, err)
	}
	waitFor(t, "b knows", func() bool { return b.Removed() != nil })
	if got := b.Removed(); got.By.ID != nid("a") || got.By.Name != "a" || got.At.IsZero() {
		t.Fatalf("removed %+v", got)
	}
	waitFor(t, "a forgets b", func() bool { return !isMember(a, "b") })
	waitFor(t, "c forgets b", func() bool { return !isMember(c, "b") })
	cb.wait(t)
	if topo := b.Topology(); len(topo.Nodes) != 1 {
		t.Fatalf("b still sees %+v", topo.Nodes)
	}
	// c keeps a, offline.
	if n, ok := findTopo(c.Topology(), nid("a")); !ok || n.Online() {
		t.Fatalf("c sees a as %+v", n)
	}

	// b links no more, until a person adds it again.
	if err := openLink(t, b, c, false).wait(t); err == nil || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("b linked: %v", err)
	}
	if err := openLink(t, c, b, false).wait(t); err == nil || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("c linked to b: %v", err)
	}
}

// The admit of a link that ran is spent: a removal that comes over it
// later stands.
func TestRemoveOverAnAddedLink(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	l := waitUp(t, openLink(t, a, b, true))
	waitFor(t, "b knows a", func() bool { return isMember(b, "a") })
	if _, err := b.Remove("a"); err != nil {
		t.Fatal(err)
	}
	var retry *RetryError
	if err := l.wait(t); errors.As(err, &retry) {
		t.Fatalf("the link ended with %v", err)
	}
	waitFor(t, "a knows", func() bool { return a.Removed() != nil })
	if a.Removed() == nil || isMember(b, "a") {
		t.Fatal("a came back")
	}
}

// Two machines have at most one link each way: a new one replaces the
// one before, and the other way has its own.
func TestOneLinkEachWay(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	first := linkUp(t, a, b)
	second := linkUp(t, a, b)
	// Either end may close it first.
	if err := first.wait(t); err == nil {
		t.Fatal("the first link ended without an error")
	}
	back := linkUp(t, b, a)
	waitFor(t, "a and b list a link each way", func() bool {
		for _, n := range []*Node{a, b} {
			if len(n.Topology().Edges) != 2 {
				return false
			}
		}
		return true
	})
	for _, l := range []*pipeLink{second, back} {
		select {
		case err := <-l.done:
			t.Fatalf("a link ended: %v", err)
		default:
		}
	}
}

// A machine that removes itself leaves the network.
func TestRemoveSelf(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	l := linkUp(t, a, b)
	waitFor(t, "a reaches b", func() bool { return reaches(a, "b") })
	r, err := a.Remove("a")
	if err != nil || !r.Self {
		t.Fatalf("removal %+v, %v", r, err)
	}
	waitFor(t, "a left", func() bool { return a.Removed() != nil })
	l.wait(t)
	waitFor(t, "b forgets a", func() bool { return !isMember(b, "a") })
	if _, err := a.Remove("a"); err == nil {
		t.Fatal("left twice")
	}
}

// gst add brings a removed machine back, from either end of the link.
func TestAddBringsBack(t *testing.T) {
	saved := make(chan int64, 4)
	a, b := newNode(t, "a"), newNodeConfig(t, "b", Config{SaveGeneration: func(g int64) error { saved <- g; return nil }})
	l := linkUp(t, a, b)
	waitFor(t, "a reaches b", func() bool { return reaches(a, "b") })
	if _, err := a.Remove("b"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b knows", func() bool { return b.Removed() != nil })
	l.wait(t)

	// b adds a link to a: it comes back with the next generation.
	waitUp(t, openLink(t, b, a, true))
	if g := <-saved; g != 1 {
		t.Fatalf("generation %d", g)
	}
	waitFor(t, "b is back", func() bool { return b.Removed() == nil && reaches(a, "b") && reaches(b, "a") })
}

// A member that adds a link to a machine removed while it was offline
// tells it, and the link's next try brings it back.
func TestAddBringsBackFromTheOtherEnd(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	l := linkUp(t, a, b)
	linkUp(t, c, b)
	waitFor(t, "a reaches c", func() bool { return reaches(a, "c") })
	l.cut()
	l.wait(t)
	if _, err := c.Remove("a"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b forgets a", func() bool { return !isMember(b, "a") })
	if a.Removed() != nil {
		t.Fatal("a heard of it")
	}

	err := openLink(t, b, a, true).wait(t)
	var retry *RetryError
	if !errors.As(err, &retry) {
		t.Fatalf("the first try: %v", err)
	}
	waitFor(t, "a learns", func() bool { return a.Removed() != nil })
	waitUp(t, openLink(t, b, a, true))
	waitFor(t, "a is back", func() bool { return a.Removed() == nil && reaches(c, "a") })
}

// A node that finds its own entry of an earlier run with a higher
// generation takes it on.
func TestOwnEntryOfAnEarlierRun(t *testing.T) {
	saved := make(chan int64, 1)
	a := newNodeConfig(t, "a", Config{SaveGeneration: func(g int64) error { saved <- g; return nil }})
	waitFor(t, "a publishes", func() bool { _, ok := a.table.Get(state.NodeKey(nid("a"))); return ok })
	a.flood.Put(wire.Entry{Key: state.NodeKey(nid("a")), Version: []int64{3, time.Now().Add(time.Hour).UnixNano()}, Value: []byte(`{"name":"a"}`)})
	if g := <-saved; g != 3 {
		t.Fatalf("generation %d", g)
	}
	waitFor(t, "a publishes again", func() bool {
		e, _ := a.table.Get(state.NodeKey(nid("a")))
		return strings.Contains(string(e.Value), "gst test")
	})
}

// The table outlives the daemon.
func TestTableIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	ctx, cancel := context.WithCancel(t.Context())
	a := New(Config{Software: "gst test", Log: log.New(io.Discard, "", 0), StatePath: path})
	a.Configure(&config.Node{ID: nid("a"), Name: "a"})
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	b := newNode(t, "b")
	linkUp(t, a, b)
	waitFor(t, "a has b's entry", func() bool { _, ok := a.table.Get(state.NodeKey(nid("b"))); return ok })
	cancel()
	<-done

	again := newNodeConfig(t, "a", Config{StatePath: path})
	if n, ok := findTopo(again.Topology(), nid("b")); !ok || n.Name != "b" || n.Online() {
		t.Fatalf("a after a restart sees b as %+v", n)
	}
	// Another identity starts afresh.
	other := newNodeConfig(t, "x", Config{StatePath: path})
	if isMember(other, "b") {
		t.Fatal("x took a's table")
	}
}

// A new identity is a new machine.
func TestConfigure(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	l := linkUp(t, a, b)
	waitFor(t, "a knows b", func() bool { return isMember(a, "b") })
	a.Configure(&config.Node{ID: nid("a"), Name: "a2"})
	waitFor(t, "b hears the new name", func() bool { n, _ := findTopo(b.Topology(), nid("a")); return n.Name == "a2" })

	a.Configure(&config.Node{ID: nid("n"), Name: "a"})
	l.wait(t)
	if isMember(a, "b") {
		t.Fatal("the new identity kept the table")
	}
	a.Configure(nil)
	if a.Self().ID != "" {
		t.Fatal("identity kept")
	}
	if err := openLink(t, a, b, false).wait(t); err == nil {
		t.Fatal("linked without identity")
	}
}

type failConn struct {
	net.Conn
	text string
}

func (c failConn) Failure() string { return c.text }

// A remote gst that fails before the handshake explains the link's
// failure.
func TestLinkReportsFailureOfRemoteGst(t *testing.T) {
	a := newNode(t, "a")
	x, y := net.Pipe()
	y.Close()
	err := a.Link(t.Context(), false, failConn{x, "gst: command not found"}, nil)
	var remote *RemoteError
	if !errors.As(err, &remote) || !strings.Contains(err.Error(), "command not found") {
		t.Fatalf("error %v", err)
	}
	x, y = net.Pipe()
	y.Close()
	if err := a.Link(t.Context(), false, x, nil); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("error %v", err)
	}
}

// The subnet of the network: a person's choice beats gst's, and the
// latest person's choice wins.
// Setting this node's part in the virtual network again, unchanged, does
// not count as a change: the virtual network sets it after every change.
func TestSetNetQuiet(t *testing.T) {
	a := newNode(t, "a")
	a.SetNet(&wire.NodeNet{Address: "10.99.0.1", PublicKey: strings.Repeat("ab", 32)})
	waitFor(t, "a publishes its net", func() bool { return a.Topology().Nodes[0].Net != nil })
	time.Sleep(100 * time.Millisecond)
	changed := a.Changed()
	a.SetNet(&wire.NodeNet{Address: "10.99.0.1", PublicKey: strings.Repeat("ab", 32)})
	select {
	case <-changed:
		t.Fatal("an unchanged net is a change")
	case <-time.After(300 * time.Millisecond):
	}
}

// gst's choice on a machine alone, however early, loses to a network's.
func TestLoneChoiceLoses(t *testing.T) {
	lone, a, b := newNode(t, "lone"), newNode(t, "a"), newNode(t, "b")
	if err := lone.SetNetwork("10.1.0.0/16", false); err != nil {
		t.Fatal(err)
	}
	linkUp(t, a, b)
	if err := a.SetNetwork("10.2.0.0/16", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b takes it", func() bool { v, _ := b.Network(); return v.Subnet == "10.2.0.0/16" })
	linkUp(t, lone, b)
	waitFor(t, "lone joins the network", func() bool { v, _ := lone.Network(); return v.Subnet == "10.2.0.0/16" })
	if v, _ := a.Network(); v.Subnet != "10.2.0.0/16" {
		t.Fatalf("the network moved to %s", v.Subnet)
	}
}

func TestSetNetwork(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	linkUp(t, a, b)
	if err := a.SetNetwork("10.1.0.0/16", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b takes it", func() bool { v, ok := b.Network(); return ok && v.Subnet == "10.1.0.0/16" })
	if err := b.SetNetwork("10.2.0.0/16", true); err != nil {
		t.Fatal(err)
	}
	if err := a.SetNetwork("10.3.0.0/16", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a takes b's", func() bool { v, _ := a.Network(); return v.Subnet == "10.2.0.0/16" })
	if err := a.SetNetwork("10.4.0.0/16", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b takes a's", func() bool { v, _ := b.Network(); return v.Subnet == "10.4.0.0/16" })
	if err := a.SetNetwork("nonsense", true); err == nil {
		t.Fatal("took nonsense")
	}
}

// Calls of methods that nobody registered fail, along the links too.
func TestUnknownMethod(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	linkUp(t, a, b)
	waitFor(t, "a reaches b", func() bool { return reaches(a, "b") })
	wantCode(t, a.Call(t.Context(), "b", "nonsense", nil, nil), wire.CodeUnsupportedMethod)
	var info wire.NodeInfo
	if err := a.Call(t.Context(), "b", wire.MethodPeerPing, nil, &info); err != nil || info.Name != "b" {
		t.Fatalf("ping %+v, %v", info, err)
	}
}
