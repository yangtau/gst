package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/vnet"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// secret stands for what the logs must not contain: the text that a remote
// gst's failure quotes.
const secret = "secret.example.com"

func nid(name string) string {
	h := hex.EncodeToString([]byte(name))
	return h + strings.Repeat("0", 32-len(h))
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// world is the nodes of a test; a link's destination names the node
// that it leads to.
type world struct {
	t   *testing.T
	dir string

	mu      sync.Mutex
	sockets map[string]string // machine name -> control socket
}

func newWorld(t *testing.T) *world {
	t.Helper()
	// Unix socket paths are short on macOS, so avoid t.TempDir's long names.
	dir, err := os.MkdirTemp("", "gst")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return &world{t: t, dir: dir, sockets: map[string]string{}}
}

func (w *world) socket(name string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sockets[name]
}

type attempt struct {
	destination string
	at          time.Time
}

// testNode is a daemon with its own configuration, whose links connect to
// the daemons of the other nodes of its world through their control
// sockets, as gst peer-bridge does.
type testNode struct {
	t       *testing.T
	w       *world
	name    string
	d       *Daemon
	cfgPath string
	socket  string
	log     *syncBuffer

	mu        sync.Mutex
	attempts  []attempt
	conns     []net.Conn
	flakyOpen int // the number of "flaky" attempts that fail

	stop   context.CancelFunc
	served chan error
}

// failConn is a connection whose remote gst failed, as an sshconn.Session
// reports it.
type failConn struct {
	net.Conn
	text string
}

func (c failConn) Failure() string { return c.text }

// newTestNode prepares a node named name with node identity (unless
// bare) and no links.
func newTestNode(t *testing.T, w *world, name string, bare bool, retry time.Duration) *testNode {
	t.Helper()
	dir := filepath.Join(w.dir, name)
	m := &testNode{t: t, w: w, name: name, cfgPath: filepath.Join(dir, "config.json"), socket: filepath.Join(dir, "run", "gst.sock"),
		log: &syncBuffer{}}
	if !bare {
		m.update(func(f *config.File) {
			f.Node = &config.Node{ID: nid(name), Name: name}
		})
	}
	m.d = New(Config{
		ConfigPath: m.cfgPath, Socket: m.socket, Software: "gst test", Build: "sha256:test",
		Connect: m.connect, RetryDelay: retry, Log: log.New(m.log, "", 0),
	})
	w.mu.Lock()
	w.sockets[name] = m.socket
	w.mu.Unlock()
	return m
}

// startTestNode creates a node and serves its daemon until the test ends.
func startTestNode(t *testing.T, w *world, name string) *testNode {
	t.Helper()
	m := newTestNode(t, w, name, false, time.Millisecond)
	m.start()
	return m
}

func (m *testNode) start() {
	m.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m.stop = cancel
	m.served = make(chan error, 1)
	go func() { m.served <- m.d.Serve(ctx) }()
	m.t.Cleanup(func() { m.shutdown() })
	dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
	defer dcancel()
	c, err := DialWait(dctx, m.socket, "gst test", "sha256:cli")
	if err != nil {
		m.t.Fatalf("daemon %s does not answer: %v", m.name, err)
	}
	c.Close()
}

// shutdown stops the daemon and returns what Serve returned.
func (m *testNode) shutdown() error {
	m.stop()
	select {
	case err := <-m.served:
		m.served <- err
		return err
	case <-time.After(10 * time.Second):
		m.t.Errorf("daemon %s did not stop", m.name)
		return nil
	}
}

func (m *testNode) update(fn func(*config.File)) {
	m.t.Helper()
	if err := config.Update(m.cfgPath, func(f *config.File) error { fn(f); return nil }); err != nil {
		m.t.Fatal(err)
	}
}

// apply changes the configuration and reloads the daemon.
func (m *testNode) apply(fn func(*config.File)) wire.Status {
	m.t.Helper()
	m.update(fn)
	if err := m.d.Reload(context.Background()); err != nil {
		m.t.Fatal(err)
	}
	return m.d.Status()
}

// connect is Config.Connect. Destinations: "refuse" fails, "hang" waits
// for the link to stop, "secret" leads to a remote gst that failed with a
// message that quotes a host, "flaky" fails its first flakyOpen attempts,
// and any other is the name of a node to connect to, after a user and
// "@".
func (m *testNode) connect(ctx context.Context, l *config.Link) (io.ReadWriteCloser, error) {
	m.mu.Lock()
	m.attempts = append(m.attempts, attempt{l.Destination, time.Now()})
	flakyFails := 0
	for _, a := range m.attempts {
		if a.destination == "flaky" {
			flakyFails++
		}
	}
	failFlaky := flakyFails <= m.flakyOpen
	m.mu.Unlock()

	dest := l.Destination
	switch dest {
	case "refuse":
		return nil, errors.New("connection refused")
	case "hang":
		<-ctx.Done()
		return nil, ctx.Err()
	case "secret":
		a, b := net.Pipe()
		b.Close()
		return failConn{a, secret + ": gst: command not found"}, nil
	case "flaky":
		if failFlaky {
			return nil, errors.New("connection refused")
		}
		dest = "b"
	}
	if _, host, ok := strings.Cut(dest, "@"); ok {
		dest = host
	}
	socket := m.w.socket(dest)
	if socket == "" {
		return nil, errors.New("unknown destination")
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.conns = append(m.conns, c)
	m.mu.Unlock()
	return c, nil
}

// dropConnections breaks every link connection that this node opened.
func (m *testNode) dropConnections() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.conns {
		c.Close()
	}
	m.conns = nil
}

func (m *testNode) attemptsTo(destination string) []attempt {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []attempt
	for _, a := range m.attempts {
		if a.destination == destination {
			out = append(out, a)
		}
	}
	return out
}

func (m *testNode) link(dest string) (wire.LinkState, bool) {
	for _, l := range m.d.Status().Links {
		if l.Destination == dest {
			return l, true
		}
	}
	return wire.LinkState{}, false
}

// linkTo adds a link to dest and waits until it runs.
func (m *testNode) linkTo(dest string) {
	m.t.Helper()
	m.apply(func(f *config.File) { addLinks(f, dest) })
	waitFor(m.t, m.name+" link to "+dest+" runs", func() bool { l, _ := m.link(dest); return l.State == wire.StateRunning })
}

// addLinks adds links to the destinations dests.
func addLinks(f *config.File, dests ...string) {
	for _, dest := range dests {
		f.Links = append(f.Links, config.Link{Destination: dest})
	}
}

// removeLink removes the link to dest.
func removeLink(f *config.File, dest string) {
	f.Links = slices.DeleteFunc(f.Links, func(l config.Link) bool { return l.Destination == dest })
}

// linkLog is how the daemon log names the link to dest.
func linkLog(dest string) string { return "link " + config.LinkID(dest) }

// reaches reports whether this node's daemon can route to the node called name.
func (m *testNode) reaches(name string) bool {
	for _, n := range m.d.mesh.Topology().Nodes {
		if n.ID == nid(name) && n.Reachable && n.Name == name { // not a placeholder
			return true
		}
	}
	return false
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

// pair returns running nodes a and b where a has a link "bee" to b, and
// both know each other.
func pair(t *testing.T) (a, b *testNode) {
	t.Helper()
	w := newWorld(t)
	b = startTestNode(t, w, "b")
	a = startTestNode(t, w, "a")
	a.linkTo("b")
	waitFor(t, "b reaches a", func() bool { return b.reaches("a") })
	return a, b
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var werr *wire.Error
	if !errors.As(err, &werr) || werr.Code != code {
		t.Fatalf("got error %v, want code %s", err, code)
	}
}

func TestStatus(t *testing.T) {
	a, b := pair(t)
	st := a.d.Status()
	if st.Software != "gst test" || st.Build != "sha256:test" || st.PID != os.Getpid() || st.Config != a.cfgPath {
		t.Fatalf("status = %+v", st)
	}
	if st.Node.ID != nid("a") || st.Node.Name != "a" {
		t.Fatalf("node = %+v", st.Node)
	}
	if len(st.Links) != 1 {
		t.Fatalf("links = %+v", st.Links)
	}
	l := st.Links[0]
	if l.Destination != "b" || l.State != wire.StateRunning || l.Error != "" || l.Since.IsZero() || l.Peer == nil || l.Peer.ID != nid("b") || l.Peer.Name != "b" {
		t.Fatalf("link = %+v", l)
	}

	// The accepting side lists no link: only links that a node opened are its own.
	if links := b.d.Status().Links; len(links) != 0 {
		t.Fatalf("b links = %+v", links)
	}

	// The link logs into the daemon's log, with its id.
	if log, want := a.log.String(), linkLog("b")+": up"; !strings.Contains(log, want) {
		t.Fatalf("daemon log = %q, want %s", log, want)
	}
}

// The control socket checks a ping's size by itself: gst is not its only
// client, and the size becomes a buffer.
func TestPingSizeIsChecked(t *testing.T) {
	a, _ := pair(t)
	for _, size := range []int{-1, vnet.MaxPingSize + 1} {
		_, err := a.d.netPing(t.Context(), "b", size)
		if err == nil || !strings.Contains(err.Error(), "size") {
			t.Fatalf("size %d: %v", size, err)
		}
	}
}

func TestLinkStates(t *testing.T) {
	w := newWorld(t)
	bare := newTestNode(t, w, "bare", true, time.Millisecond)
	bare.start()
	a := startTestNode(t, w, "a")
	a.apply(func(f *config.File) {
		addLinks(f, "refuse", "hang", "secret", "bare", "a")
	})
	for _, tt := range []struct {
		link, state, errText string
	}{
		{"refuse", wire.StateDown, "connection refused"},
		{"hang", wire.StateConnecting, ""},
		{"secret", wire.StateDown, "the remote gst failed: " + secret},
		{"bare", wire.StateDown, "no identity"},
		{"a", wire.StateDown, "this machine"},
	} {
		waitFor(t, tt.link+" is "+tt.state, func() bool {
			l, _ := a.link(tt.link)
			return l.State == tt.state && strings.Contains(l.Error, tt.errText)
		})
		if l, _ := a.link(tt.link); l.Peer != nil {
			t.Errorf("%s has peer %+v while %s", tt.link, l.Peer, l.State)
		}
	}
	// A link that keeps failing keeps trying.
	waitFor(t, "reconnects", func() bool { l, _ := a.link("refuse"); return l.Reconnects >= 3 })
	if l, _ := a.link("hang"); l.Reconnects != 0 || l.Error != "" {
		t.Fatalf("hang = %+v", l)
	}
	// In the order of the configuration.
	var dests []string
	for _, l := range a.d.Status().Links {
		dests = append(dests, l.Destination)
	}
	if strings.Join(dests, " ") != "refuse hang secret bare a" {
		t.Fatalf("links = %v", dests)
	}
}

func TestLinkRecoversAfterFailures(t *testing.T) {
	w := newWorld(t)
	startTestNode(t, w, "b")
	a := newTestNode(t, w, "a", false, time.Millisecond)
	a.flakyOpen = 2
	a.start()
	a.linkTo("flaky")
	l, _ := a.link("flaky")
	if l.Reconnects != 2 || l.Peer == nil {
		t.Fatalf("link = %+v, want up after 2 reconnects", l)
	}
	if len(a.attemptsTo("flaky")) != 3 {
		t.Fatalf("attempts = %d", len(a.attemptsTo("flaky")))
	}
}

func TestBackoffDoublesAndResets(t *testing.T) {
	w := newWorld(t)
	startTestNode(t, w, "b")
	const base = 50 * time.Millisecond
	a := newTestNode(t, w, "a", false, base)
	a.flakyOpen = 3
	a.start()
	a.linkTo("flaky")

	at := a.attemptsTo("flaky")
	if len(at) != 4 {
		t.Fatalf("%d attempts, want 3 failures and a success", len(at))
	}
	for i, min := range []time.Duration{base, 2 * base, 4 * base} {
		if gap := at[i+1].at.Sub(at[i].at); gap < min {
			t.Errorf("wait after failure %d = %v, want at least %v", i+1, gap, min)
		}
	}

	// Having been up, the next failure waits for the base delay again, not
	// for the 8 x base that the next doubling would give.
	a.mu.Lock()
	a.flakyOpen = 1000
	a.mu.Unlock()
	dropped := time.Now()
	a.dropConnections()
	waitFor(t, "another attempt", func() bool { return len(a.attemptsTo("flaky")) > 4 })
	next := a.attemptsTo("flaky")[4].at
	if gap := next.Sub(dropped); gap < base || gap > 6*base {
		t.Errorf("wait after the link was lost = %v, want about %v", gap, base)
	}
}

func TestLinkLogsLeaveOutRemoteText(t *testing.T) {
	w := newWorld(t)
	a := startTestNode(t, w, "a")
	a.apply(func(f *config.File) { addLinks(f, "secret") })
	waitFor(t, "link fails", func() bool { l, _ := a.link("secret"); return strings.Contains(l.Error, secret) })
	a.apply(func(f *config.File) { removeLink(f, "secret") })
	a.shutdown()

	log := a.log.String()
	if strings.Contains(log, secret) {
		t.Errorf("daemon log quotes the remote gst:\n%s", log)
	}
	for _, want := range []string{linkLog("secret") + ": started", linkLog("secret") + ": the remote gst failed; see gst status", linkLog("secret") + ": stopped"} {
		if !strings.Contains(log, want) {
			t.Errorf("daemon log lacks %q:\n%s", want, log)
		}
	}
}

func TestIncomingLinkLogLeavesOutPeerText(t *testing.T) {
	w := newWorld(t)
	b := startTestNode(t, w, "b")
	// A peer that sends a Hello and then garbage.
	c, err := net.Dial("unix", b.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Its Hello names no node, which the daemon refuses.
	wire.Dial(c, wire.Hello{Software: secret, Role: wire.RolePeer})
	c.Write([]byte(secret + secret + secret))
	c.Close()
	b.shutdown()
	if strings.Contains(b.log.String(), secret) {
		t.Fatalf("daemon log quotes the peer:\n%s", b.log.String())
	}
}

func TestReloadStartsAndStopsLinks(t *testing.T) {
	w := newWorld(t)
	startTestNode(t, w, "b")
	startTestNode(t, w, "c")
	a := startTestNode(t, w, "a")

	st := a.apply(func(f *config.File) { addLinks(f, "x@b", "y@c") })
	if len(st.Links) != 2 || st.Links[0].Destination != "x@b" || st.Links[1].Destination != "y@c" {
		t.Fatalf("links = %+v", st.Links)
	}
	waitFor(t, "both run", func() bool {
		x, _ := a.link("x@b")
		y, _ := a.link("y@c")
		return x.State == wire.StateRunning && y.State == wire.StateRunning
	})
	since, _ := a.link("x@b")

	// Removing a link ends it and leaves the other running, untouched.
	st = a.apply(func(f *config.File) { removeLink(f, "y@c") })
	if len(st.Links) != 1 || st.Links[0].Destination != "x@b" {
		t.Fatalf("links = %+v, want only x@b", st.Links)
	}
	if again, _ := a.link("x@b"); !again.Since.Equal(since.Since) || again.Reconnects != 0 || again.State != wire.StateRunning {
		t.Fatalf("removing y@c restarted x@b: %+v -> %+v", since, again)
	}
	if want := linkLog("y@c") + ": stopped"; !strings.Contains(a.log.String(), want) {
		t.Errorf("daemon log = %q, want %s", a.log.String(), want)
	}

	// Removing the last one makes b unreachable.
	a.apply(func(f *config.File) { removeLink(f, "x@b") })
	waitFor(t, "b is no longer linked to", func() bool { return !a.reaches("b") })

	// Adding a link brings it up; reloading leaves a running one alone.
	a.apply(func(f *config.File) { addLinks(f, "b") })
	waitFor(t, "b runs", func() bool { l, _ := a.link("b"); return l.State == wire.StateRunning })
	since, _ = a.link("b")
	a.apply(func(f *config.File) {})
	if again, _ := a.link("b"); !again.Since.Equal(since.Since) || again.Reconnects != 0 {
		t.Fatalf("a reload restarted a running link: %+v -> %+v", since, again)
	}
}

func TestReloadRestartsChangedLink(t *testing.T) {
	w := newWorld(t)
	startTestNode(t, w, "b")
	startTestNode(t, w, "c")
	a := startTestNode(t, w, "a")
	a.apply(func(f *config.File) { addLinks(f, "hang", "b") })
	waitFor(t, "hang connects", func() bool { return len(a.attemptsTo("hang")) == 1 })
	waitFor(t, "b runs", func() bool { l, _ := a.link("b"); return l.State == wire.StateRunning })
	same, _ := a.link("b")

	// Replacing a link cancels the old attempt, and the new link connects.
	st := a.apply(func(f *config.File) { removeLink(f, "hang"); addLinks(f, "x@c") })
	if len(st.Links) != 2 {
		t.Fatalf("links = %+v", st.Links)
	}
	waitFor(t, "x@c runs", func() bool { l, _ := a.link("x@c"); return l.State == wire.StateRunning })
	if len(a.attemptsTo("hang")) != 1 {
		t.Errorf("attempts to the old destination = %d, want 1", len(a.attemptsTo("hang")))
	}

	// Changed ssh arguments restart a link, which connects again.
	before := len(a.attemptsTo("x@c"))
	a.apply(func(f *config.File) { f.Link("x@c").SSHArgs = []string{"-p", "2222"} })
	waitFor(t, "x@c connects again", func() bool { return len(a.attemptsTo("x@c")) > before })
	waitFor(t, "x@c runs again", func() bool { l, _ := a.link("x@c"); return l.State == wire.StateRunning })

	// A link added again restarts, and the run spends its admit.
	before = len(a.attemptsTo("x@c"))
	a.apply(func(f *config.File) { f.Link("x@c").Admit = true })
	waitFor(t, "x@c connects once more", func() bool { return len(a.attemptsTo("x@c")) > before })
	waitFor(t, "the admit is spent", func() bool {
		f, err := config.Load(a.cfgPath)
		l, _ := a.link("x@c")
		return err == nil && !f.Link("x@c").Admit && l.State == wire.StateRunning
	})

	// The links that did not change keep running as they are.
	if again, _ := a.link("b"); !again.Since.Equal(same.Since) || again.Reconnects != 0 {
		t.Fatalf("an unrelated link restarted: %+v -> %+v", same, again)
	}
	log := a.log.String()
	if strings.Count(log, linkLog("x@c")+": started") != 3 || strings.Count(log, linkLog("x@c")+": stopped") != 2 ||
		strings.Count(log, linkLog("hang")+": stopped") != 1 {
		t.Errorf("daemon log = %q, want x@c started three times and stopped twice, and hang stopped", log)
	}
}

func TestReloadWithBadConfigKeepsServing(t *testing.T) {
	w := newWorld(t)
	a := newTestNode(t, w, "a", false, time.Millisecond)
	if err := os.WriteFile(a.cfgPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.start() // the daemon starts without a usable configuration
	c, err := Dial(t.Context(), a.socket, "gst test", "sha256:cli")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Call(t.Context(), wire.MethodReload, nil, nil)
	wantCode(t, err, wire.CodeFailed)
	if err := c.Call(t.Context(), wire.MethodStatus, nil, nil); err != nil {
		t.Fatal(err)
	}

	// Once it is fixed, a reload starts the links.
	if err := os.Remove(a.cfgPath); err != nil {
		t.Fatal(err)
	}
	a.update(func(f *config.File) {
		f.Node = &config.Node{ID: nid("a"), Name: "a"}
		addLinks(f, "hang")
	})
	st, err := call[wire.Status](t.Context(), c, wire.MethodReload, nil)
	if err != nil || len(st.Links) != 1 || st.Node.ID != nid("a") {
		t.Fatalf("Reload = %+v, %v", st, err)
	}
}

func TestReloadChangesNodeIdentity(t *testing.T) {
	a, b := pair(t)
	a.apply(func(f *config.File) { f.Node.Name = "renamed" })
	waitFor(t, "b sees a's new name", func() bool {
		for _, n := range b.d.mesh.Topology().Nodes {
			if n.ID == nid("a") {
				return n.Name == "renamed"
			}
		}
		return false
	})
	if a.d.Status().Node.Name != "renamed" {
		t.Fatal("status does not show the name")
	}
}

func TestControlClient(t *testing.T) {
	a, _ := pair(t)
	c, err := Dial(t.Context(), a.socket, "gst cli", "sha256:cli")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if h := c.Daemon; h.Software != "gst test" || h.Build != "sha256:test" || h.Role != wire.RoleControl || h.Protocol != wire.Protocol {
		t.Fatalf("hello = %+v", h)
	}

	// Several calls share the connection.
	st, err := call[wire.Status](t.Context(), c, wire.MethodStatus, nil)
	if err != nil || st.Node.Name != "a" || len(st.Links) != 1 {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	topo, err := call[wire.Topology](t.Context(), c, wire.MethodTopology, nil)
	if err != nil || topo.Self != nid("a") || len(topo.Nodes) != 2 || len(topo.Links) != 1 {
		t.Fatalf("Nodes = %+v, %v", topo, err)
	}
	for _, n := range topo.Nodes {
		if !n.Reachable {
			t.Fatalf("node %+v is not reachable", n)
		}
	}
	a.update(func(f *config.File) { addLinks(f, "hang") })
	if st, err = call[wire.Status](t.Context(), c, wire.MethodReload, nil); err != nil || len(st.Links) != 2 {
		t.Fatalf("Reload = %+v, %v", st, err)
	}
	p, err := call[wire.Ping](t.Context(), c, wire.MethodPing, wire.NodeParams{Node: "b"})
	if err != nil || p.Node.ID != nid("b") || p.Node.Name != "b" || p.Via == nil || len(p.Via) != 0 || p.TimeMS <= 0 {
		t.Fatalf("Ping = %+v, %v", p, err)
	}
	_, err = call[wire.Ping](t.Context(), c, wire.MethodPing, wire.NodeParams{Node: "nowhere"})
	wantCode(t, err, wire.CodeNotFound)

	// Unknown methods fail without ending the connection.
	wantCode(t, c.Call(t.Context(), "no.such", nil, nil), wire.CodeUnsupportedMethod)
	if err := c.Call(t.Context(), wire.MethodStatus, nil, nil); err != nil {
		t.Fatal(err)
	}

	// A canceled context closes the connection.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Call(ctx, wire.MethodStatus, nil, nil); err == nil {
		t.Fatal("Status succeeded with a done context")
	}
}

func TestNodeStatusShowsAnyNodesConfiguration(t *testing.T) {
	a, b := pair(t)
	b.apply(func(f *config.File) {
		addLinks(f, "a", "ssh.example.com")
	})
	waitFor(t, "b's link back runs", func() bool { l, _ := b.link("a"); return l.State == wire.StateRunning })

	c, err := Dial(t.Context(), a.socket, "gst cli", "sha256:cli")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Another node answers with its own configuration, not ours.
	st, err := call[wire.Status](t.Context(), c, wire.MethodNodeStatus, wire.NodeParams{Node: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Node.ID != nid("b") {
		t.Fatalf("node = %+v", st.Node)
	}
	if len(st.Links) != 2 || st.Links[0].Destination != "a" || st.Links[0].State != wire.StateRunning ||
		st.Links[1].Destination != "ssh.example.com" || st.Links[1].State == wire.StateRunning {
		t.Fatalf("links = %+v, want back running and off not", st.Links)
	}

	// This node answers for itself.
	st, err = call[wire.Status](t.Context(), c, wire.MethodNodeStatus, wire.NodeParams{Node: "a.gst"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Node.ID != nid("a") || len(st.Links) != 1 || st.Links[0].Destination != "b" {
		t.Fatalf("status of a = %+v", st)
	}

	_, err = call[wire.Status](t.Context(), c, wire.MethodNodeStatus, wire.NodeParams{Node: "nowhere"})
	wantCode(t, err, wire.CodeNotFound)
}

func TestControlRefusesPeerlessRoles(t *testing.T) {
	a, _ := pair(t)
	conn, err := net.Dial("unix", a.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = wire.Dial(conn, wire.Hello{Software: "x", Role: "admin"})
	wantCode(t, err, wire.CodeUnsupportedRole)
}

func TestSingleInstance(t *testing.T) {
	w := newWorld(t)
	a := startTestNode(t, w, "a")
	second := New(Config{ConfigPath: a.cfgPath, Socket: a.socket, Software: "gst test", Log: log.New(io.Discard, "", 0)})
	err := second.Serve(t.Context())
	if err == nil || !strings.Contains(err.Error(), "another gst daemon") {
		t.Fatalf("second Serve = %v", err)
	}
	// The first is undisturbed.
	c, err := Dial(t.Context(), a.socket, "gst cli", "sha256:cli")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Call(t.Context(), wire.MethodStatus, nil, nil); err != nil {
		t.Fatal(err)
	}
	c.Close()

	info, err := os.Stat(a.socket)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket = %v, %v; want mode 0600", info, err)
	}
	if info, err := os.Stat(filepath.Dir(a.socket)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("socket directory = %v, %v; want mode 0700", info, err)
	}

	// After the first stops, another can take the socket.
	if err := a.shutdown(); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(t.Context(), a.socket, "gst cli", "sha256:cli"); err == nil {
		t.Fatal("a stopped daemon still answers")
	}
	again := newTestNode(t, w, "a", false, time.Millisecond)
	again.start()
}

func TestStaleSocketIsReplaced(t *testing.T) {
	w := newWorld(t)
	m := newTestNode(t, w, "a", false, time.Millisecond)
	if err := os.MkdirAll(filepath.Dir(m.socket), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", m.socket)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close() // leaves the socket file, as a crashed daemon does
	m.start()
}

func TestServeStopsEverything(t *testing.T) {
	w := newWorld(t)
	startTestNode(t, w, "b")
	a := startTestNode(t, w, "a")
	a.linkTo("b")

	if err := a.shutdown(); err != nil {
		t.Fatal(err)
	}
	st := a.d.Status()
	if len(st.Links) != 1 || st.Links[0].State != wire.StateDown {
		t.Fatalf("status after stop = %+v, want the configuration with nothing running", st)
	}
}

// Even without a helper, reload reports persisted rules as waiting and removes
// them synchronously. No listener may fall back to another local address.
func TestReloadExposesWithoutNetwork(t *testing.T) {
	w := newWorld(t)
	a := startTestNode(t, w, "a")
	rule, _ := config.ParseExpose("8080", 5999, false)
	st := a.apply(func(f *config.File) { f.Exposes = []config.Expose{rule} })
	if len(st.Exposes) != 1 || st.Exposes[0].State != "waiting" || st.Exposes[0].Address != "" {
		t.Fatalf("exposes: %+v", st.Exposes)
	}
	st = a.apply(func(f *config.File) { f.Exposes = nil })
	if len(st.Exposes) != 0 {
		t.Fatalf("removed rule survived: %+v", st.Exposes)
	}
}

// A link keeps the node that it reached, and runs as any other once it
// did.
func TestLinkKeepsItsNode(t *testing.T) {
	w := newWorld(t)
	startTestNode(t, w, "b")
	a := startTestNode(t, w, "a")
	a.apply(func(f *config.File) { f.Links = append(f.Links, config.Link{Destination: "b", Admit: true}) })
	waitFor(t, "the link keeps b", func() bool {
		f, err := config.Load(a.cfgPath)
		return err == nil && len(f.Links) == 1 && f.Links[0].Target == nid("b") && !f.Links[0].Admit
	})
	// Another link that a person adds to b replaces it.
	a.apply(func(f *config.File) { f.Links = append(f.Links, config.Link{Destination: "x@b", Admit: true}) })
	waitFor(t, "the new link replaces it", func() bool {
		f, err := config.Load(a.cfgPath)
		return err == nil && len(f.Links) == 1 && f.Links[0].Destination == "x@b" && f.Links[0].Target == nid("b")
	})
	// A link that nobody added, from an older configuration, to a node
	// that a link reaches already goes.
	a.apply(func(f *config.File) { f.Links = append(f.Links, config.Link{Destination: "y@b"}) })
	waitFor(t, "the second link goes", func() bool {
		f, err := config.Load(a.cfgPath)
		return err == nil && len(f.Links) == 1 && f.Links[0].Destination == "x@b"
	})
	waitFor(t, "a links to b once", func() bool { return len(a.d.mesh.Topology().Links) == 1 && a.reaches("b") })
}

// Removing a node drops the links to it, and the node drops its
// own; a link that a person adds brings it back.
func TestRemoveDropsLinks(t *testing.T) {
	w := newWorld(t)
	b := startTestNode(t, w, "b")
	c := startTestNode(t, w, "c")
	a := startTestNode(t, w, "a")
	a.linkTo("b")
	c.linkTo("b")
	waitFor(t, "a reaches c", func() bool { return a.reaches("c") })
	waitFor(t, "the link keeps b", func() bool { f, _ := config.Load(c.cfgPath); return f.Links[0].Target == nid("b") })

	cl, err := Dial(t.Context(), a.socket, "gst test", "sha256:cli")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	r, err := call[wire.Removal](t.Context(), cl, wire.MethodRemove, wire.NodeParams{Node: "b"})
	if err != nil || r.Node.ID != nid("b") || len(r.Stranded) != 1 || r.Stranded[0].Name != "c" {
		t.Fatalf("Remove = %+v, %v", r, err)
	}
	waitFor(t, "b knows", func() bool { return b.d.Status().Removed != nil })
	waitFor(t, "c drops its link to b", func() bool { f, _ := config.Load(c.cfgPath); return len(f.Links) == 0 })
	waitFor(t, "a drops its link to b", func() bool { f, _ := config.Load(a.cfgPath); return len(f.Links) == 0 })

	// b adds a link to c: it is back.
	b.apply(func(f *config.File) { f.Links = append(f.Links, config.Link{Destination: "c", Admit: true}) })
	waitFor(t, "b is back", func() bool { return b.d.Status().Removed == nil && c.reaches("b") && b.reaches("c") })
	waitFor(t, "b saved its generation", func() bool { f, _ := config.Load(b.cfgPath); return f.Node.Generation == 1 })
}

// A node that removes itself leaves the network and drops its links.
func TestRemoveSelf(t *testing.T) {
	a, b := pair(t)
	cl, err := Dial(t.Context(), a.socket, "gst test", "sha256:cli")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if r, err := call[wire.Removal](t.Context(), cl, wire.MethodRemove, wire.NodeParams{Node: "a"}); err != nil || !r.Self {
		t.Fatalf("Remove = %+v, %v", r, err)
	}
	waitFor(t, "a drops its links", func() bool { f, _ := config.Load(a.cfgPath); return len(f.Links) == 0 })
	waitFor(t, "b forgets a", func() bool {
		topo := b.d.mesh.Topology()
		return !slices.ContainsFunc(topo.Nodes, func(n wire.TopoNode) bool { return n.ID == nid("a") })
	})
	wantCode(t, cl.Call(t.Context(), wire.MethodSubnet, wire.SubnetParams{Subnet: "10.9.0.0/16"}, nil), wire.CodeRemoved)
}

func TestSetSubnet(t *testing.T) {
	a, b := pair(t)
	cl, err := Dial(t.Context(), a.socket, "gst test", "sha256:cli")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if err := cl.Call(t.Context(), wire.MethodSubnet, wire.SubnetParams{Subnet: "10.9.0.0/16"}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b takes it", func() bool { return b.d.mesh.Topology().Subnet == "10.9.0.0/16" })
	wantCode(t, cl.Call(t.Context(), wire.MethodSubnet, wire.SubnetParams{Subnet: "nonsense"}, nil), wire.CodeInvalidParams)
}

// call calls method of the daemon that c leads to, and returns its result.
func call[R any](ctx context.Context, c *Client, method string, params any) (R, error) {
	var r R
	err := c.Call(ctx, method, params, &r)
	return r, err
}

// A process that cannot serve a socket, as on a phone, runs the same
// daemon with Run, and Call answers what the socket would.
func TestRunAndCallWithoutASocket(t *testing.T) {
	w := newWorld(t)
	b := startTestNode(t, w, "b")
	p := &testNode{t: t, w: w, name: "p", cfgPath: filepath.Join(w.dir, "p", "config.json"), log: &syncBuffer{}}
	p.update(func(f *config.File) {
		f.Node = &config.Node{ID: nid("p"), Name: "p"}
		addLinks(f, "b")
	})
	p.d = New(Config{
		ConfigPath: p.cfgPath, StateDir: filepath.Join(w.dir, "p", "state"), Software: "gst test",
		Connect: p.connect, RetryDelay: time.Millisecond, Log: log.New(p.log, "", 0),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.d.Run(ctx)
	}()
	defer cancel()

	call := func(method string, result any) *wire.Error {
		resp := p.d.Call(ctx, wire.Request{Method: method})
		if resp.Error == nil && result != nil {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				t.Fatal(err)
			}
		}
		return resp.Error
	}
	waitFor(t, "p's link runs", func() bool {
		var st wire.Status
		return call(wire.MethodStatus, &st) == nil && len(st.Links) == 1 && st.Links[0].State == wire.StateRunning
	})
	waitFor(t, "b reaches p", func() bool { return b.reaches("p") })
	var topo wire.Topology
	if err := call(wire.MethodTopology, &topo); err != nil || len(topo.Nodes) != 2 || topo.Self != nid("p") {
		t.Fatalf("topology = %+v, %v", topo, err)
	}
	if err := call("nonsense", nil); err == nil || err.Code != wire.CodeUnsupportedMethod {
		t.Fatalf("an unknown method: %v", err)
	}
	resp := p.d.Call(ctx, wire.Request{Method: wire.MethodPing, Params: json.RawMessage(`{"node": "b"}`)})
	var ping wire.Ping
	if resp.Error != nil || json.Unmarshal(resp.Result, &ping) != nil || ping.Node.Name != "b" {
		t.Fatalf("ping = %s, %v", resp.Result, resp.Error)
	}
	waitFor(t, "the table is kept in the state directory", func() bool {
		_, err := os.Stat(filepath.Join(w.dir, "p", "state", "state.json"))
		return err == nil
	})

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
	waitFor(t, "b lost p", func() bool { return !b.reaches("p") })
}

// A machine that moves to another network tries its links at once, and
// waits no longer than at first after that.
func TestMovingRetriesLinksAtOnce(t *testing.T) {
	w := newWorld(t)
	const base = 300 * time.Millisecond
	a := newTestNode(t, w, "a", false, base)
	a.start()
	a.apply(func(f *config.File) { addLinks(f, "refuse") })
	// Past the first waits, which are short anyway.
	waitFor(t, "the wait has grown", func() bool { return len(a.attemptsTo("refuse")) >= 3 })
	before := len(a.attemptsTo("refuse"))
	failed := a.attemptsTo("refuse")[before-1].at

	moved := time.Now()
	a.d.mu.Lock()
	for _, s := range a.d.links {
		s.retry()
	}
	a.d.mu.Unlock()
	waitFor(t, "a new attempt", func() bool { return len(a.attemptsTo("refuse")) > before })
	at := a.attemptsTo("refuse")
	// The wait that it cut short was 4 x base.
	if gap := at[before].at.Sub(failed); gap >= 4*base && at[before].at.Sub(moved) > base {
		t.Errorf("the attempt after the move came %v after the failure", gap)
	}
	waitFor(t, "the attempt after that", func() bool { return len(a.attemptsTo("refuse")) > before+1 })
	at = a.attemptsTo("refuse")
	// Without the move, it would be 8 x base by now.
	if gap := at[before+1].at.Sub(at[before].at); gap < base || gap >= 4*base {
		t.Errorf("the wait after the move = %v, want about %v", gap, base)
	}
}
