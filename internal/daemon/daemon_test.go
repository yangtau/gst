package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
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

// world is the machines of a test; a link's destination names the machine
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

// machine is a daemon with its own configuration, whose links connect to
// the daemons of the other machines of its world through their control
// sockets, as gst peer-bridge does.
type machine struct {
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

// newMachine prepares a machine named name with node identity (unless
// bare) and no links.
func newMachine(t *testing.T, w *world, name string, bare bool, retry time.Duration) *machine {
	t.Helper()
	dir := filepath.Join(w.dir, name)
	m := &machine{t: t, w: w, name: name, cfgPath: filepath.Join(dir, "config.json"), socket: filepath.Join(dir, "run", "gst.sock"),
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

// startMachine creates a machine and serves its daemon until the test ends.
func startMachine(t *testing.T, w *world, name string) *machine {
	t.Helper()
	m := newMachine(t, w, name, false, time.Millisecond)
	m.start()
	return m
}

func (m *machine) start() {
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
func (m *machine) shutdown() error {
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

func (m *machine) update(fn func(*config.File)) {
	m.t.Helper()
	if err := config.Update(m.cfgPath, func(f *config.File) error { fn(f); return nil }); err != nil {
		m.t.Fatal(err)
	}
}

// apply changes the configuration and reloads the daemon.
func (m *machine) apply(fn func(*config.File)) wire.Status {
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
// and any other is the name of a machine to connect to, after a user and
// "@".
func (m *machine) connect(ctx context.Context, l *config.Link) (io.ReadWriteCloser, error) {
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

// dropConnections breaks every link connection that this machine opened.
func (m *machine) dropConnections() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.conns {
		c.Close()
	}
	m.conns = nil
}

func (m *machine) attemptsTo(destination string) []attempt {
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

func (m *machine) link(dest string) (wire.LinkState, bool) {
	for _, l := range m.d.Status().Links {
		if l.Destination == dest {
			return l, true
		}
	}
	return wire.LinkState{}, false
}

// linkTo adds a link to dest and waits until it runs.
func (m *machine) linkTo(dest string) {
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

// reaches reports whether this machine's daemon can route to the node called name.
func (m *machine) reaches(name string) bool {
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

// pair returns running machines a and b where a has a link "bee" to b, and
// both know each other.
func pair(t *testing.T) (a, b *machine) {
	t.Helper()
	w := newWorld(t)
	b = startMachine(t, w, "b")
	a = startMachine(t, w, "a")
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

	// The accepting side lists no link: only links that a machine opened are its own.
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
	bare := newMachine(t, w, "bare", true, time.Millisecond)
	bare.start()
	a := startMachine(t, w, "a")
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
	startMachine(t, w, "b")
	a := newMachine(t, w, "a", false, time.Millisecond)
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
	startMachine(t, w, "b")
	const base = 50 * time.Millisecond
	a := newMachine(t, w, "a", false, base)
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
	a := startMachine(t, w, "a")
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
	b := startMachine(t, w, "b")
	// A peer that sends a Hello and then garbage.
	c, err := net.Dial("unix", b.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := wire.Dial(c, wire.Hello{Software: secret, Role: wire.RolePeer, Features: []string{wire.FeatureYamux}}); err != nil {
		t.Fatal(err)
	}
	c.Write([]byte(secret + secret + secret))
	c.Close()
	b.shutdown()
	if strings.Contains(b.log.String(), secret) {
		t.Fatalf("daemon log quotes the peer:\n%s", b.log.String())
	}
}

func TestReloadStartsAndStopsLinks(t *testing.T) {
	w := newWorld(t)
	startMachine(t, w, "b")
	a := startMachine(t, w, "a")

	st := a.apply(func(f *config.File) { addLinks(f, "x@b", "y@b") })
	if len(st.Links) != 2 || st.Links[0].Destination != "x@b" || st.Links[1].Destination != "y@b" {
		t.Fatalf("links = %+v", st.Links)
	}
	waitFor(t, "both run", func() bool {
		x, _ := a.link("x@b")
		y, _ := a.link("y@b")
		return x.State == wire.StateRunning && y.State == wire.StateRunning
	})
	since, _ := a.link("x@b")

	// Removing a link ends it and leaves the other running, untouched.
	st = a.apply(func(f *config.File) { removeLink(f, "y@b") })
	if len(st.Links) != 1 || st.Links[0].Destination != "x@b" {
		t.Fatalf("links = %+v, want only x@b", st.Links)
	}
	if again, _ := a.link("x@b"); !again.Since.Equal(since.Since) || again.Reconnects != 0 || again.State != wire.StateRunning {
		t.Fatalf("removing y@b restarted x@b: %+v -> %+v", since, again)
	}
	if want := linkLog("y@b") + ": stopped"; !strings.Contains(a.log.String(), want) {
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
	startMachine(t, w, "b")
	a := startMachine(t, w, "a")
	a.apply(func(f *config.File) { addLinks(f, "hang", "b") })
	waitFor(t, "hang connects", func() bool { return len(a.attemptsTo("hang")) == 1 })
	waitFor(t, "b runs", func() bool { l, _ := a.link("b"); return l.State == wire.StateRunning })
	same, _ := a.link("b")

	// Replacing a link cancels the old attempt, and the new link connects.
	st := a.apply(func(f *config.File) { removeLink(f, "hang"); addLinks(f, "x@b") })
	if len(st.Links) != 2 {
		t.Fatalf("links = %+v", st.Links)
	}
	waitFor(t, "x@b runs", func() bool { l, _ := a.link("x@b"); return l.State == wire.StateRunning })
	if len(a.attemptsTo("hang")) != 1 {
		t.Errorf("attempts to the old destination = %d, want 1", len(a.attemptsTo("hang")))
	}

	// A changed remote gst restarts a link, which connects again.
	before := len(a.attemptsTo("x@b"))
	a.apply(func(f *config.File) { f.Link("x@b").RemoteGst = "/opt/gst" })
	waitFor(t, "x@b connects again", func() bool { return len(a.attemptsTo("x@b")) > before })
	waitFor(t, "x@b runs again", func() bool { l, _ := a.link("x@b"); return l.State == wire.StateRunning })

	// The links that did not change keep running as they are.
	if again, _ := a.link("b"); !again.Since.Equal(same.Since) || again.Reconnects != 0 {
		t.Fatalf("an unrelated link restarted: %+v -> %+v", same, again)
	}
	log := a.log.String()
	if strings.Count(log, linkLog("x@b")+": started") != 2 || strings.Count(log, linkLog("x@b")+": stopped") != 1 ||
		strings.Count(log, linkLog("hang")+": stopped") != 1 {
		t.Errorf("daemon log = %q, want x@b started twice and stopped once, and hang stopped", log)
	}
}

func TestReloadWithBadConfigKeepsServing(t *testing.T) {
	w := newWorld(t)
	a := newMachine(t, w, "a", false, time.Millisecond)
	if err := os.WriteFile(a.cfgPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.start() // the daemon starts without a usable configuration
	c, err := Dial(t.Context(), a.socket, "gst test", "sha256:cli")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Reload(t.Context())
	wantCode(t, err, wire.CodeFailed)
	if _, err := c.Status(t.Context()); err != nil {
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
	st, err := c.Reload(t.Context())
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
	st, err := c.Status(t.Context())
	if err != nil || st.Node.Name != "a" || len(st.Links) != 1 {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	topo, err := c.Nodes(t.Context())
	if err != nil || topo.Self != nid("a") || len(topo.Nodes) != 2 || len(topo.Edges) != 1 {
		t.Fatalf("Nodes = %+v, %v", topo, err)
	}
	for _, n := range topo.Nodes {
		if !n.Reachable {
			t.Fatalf("node %+v is not reachable", n)
		}
	}
	a.update(func(f *config.File) { addLinks(f, "hang") })
	if st, err = c.Reload(t.Context()); err != nil || len(st.Links) != 2 {
		t.Fatalf("Reload = %+v, %v", st, err)
	}
	p, err := c.Ping(t.Context(), "b")
	if err != nil || p.Node.ID != nid("b") || p.Node.Name != "b" || p.Via == nil || len(p.Via) != 0 || p.TimeMS <= 0 {
		t.Fatalf("Ping = %+v, %v", p, err)
	}
	_, err = c.Ping(t.Context(), "nowhere")
	wantCode(t, err, wire.CodeNotFound)

	// Unknown methods fail without ending the connection.
	wantCode(t, c.call(t.Context(), "no.such", nil, nil), wire.CodeUnsupportedMethod)
	if _, err := c.Status(t.Context()); err != nil {
		t.Fatal(err)
	}

	// A canceled context closes the connection.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Status(ctx); err == nil {
		t.Fatal("Status succeeded with a done context")
	}
}

func TestNodeStatusShowsAnyMachinesConfiguration(t *testing.T) {
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

	// Another machine answers with its own configuration, not ours.
	st, err := c.NodeStatus(t.Context(), "b")
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

	// This machine answers for itself.
	st, err = c.NodeStatus(t.Context(), "a.gst")
	if err != nil {
		t.Fatal(err)
	}
	if st.Node.ID != nid("a") || len(st.Links) != 1 || st.Links[0].Destination != "b" {
		t.Fatalf("status of a = %+v", st)
	}

	_, err = c.NodeStatus(t.Context(), "nowhere")
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
	a := startMachine(t, w, "a")
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
	if _, err := c.Status(t.Context()); err != nil {
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
	again := newMachine(t, w, "a", false, time.Millisecond)
	again.start()
}

func TestStaleSocketIsReplaced(t *testing.T) {
	w := newWorld(t)
	m := newMachine(t, w, "a", false, time.Millisecond)
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
	startMachine(t, w, "b")
	a := startMachine(t, w, "a")
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
	a := startMachine(t, w, "a")
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
