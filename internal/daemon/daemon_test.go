package daemon

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
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
	name, destination string
	at                time.Time
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
	logDir  string
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
// bare), the proxy off, and no links, maps, or routes.
func newMachine(t *testing.T, w *world, name string, bare bool, retry time.Duration) *machine {
	t.Helper()
	dir := filepath.Join(w.dir, name)
	m := &machine{t: t, w: w, name: name, cfgPath: filepath.Join(dir, "config.json"), socket: filepath.Join(dir, "run", "gst.sock"),
		logDir: filepath.Join(dir, "logs"), log: &syncBuffer{}}
	if !bare {
		m.update(func(f *config.File) {
			f.Node = &config.Node{ID: nid(name), Name: name}
			f.Proxy = config.ProxyOff
		})
	}
	m.d = New(Config{
		ConfigPath: m.cfgPath, Socket: m.socket, LogDir: m.logDir, Software: "gst test", Build: "sha256:test",
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
// and any other is the name of a machine to connect to.
func (m *machine) connect(ctx context.Context, name string, l *config.Link) (io.ReadWriteCloser, error) {
	m.mu.Lock()
	m.attempts = append(m.attempts, attempt{name, l.Destination, time.Now()})
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

func (m *machine) link(name string) (wire.LinkState, bool) {
	for _, l := range m.d.Status().Links {
		if l.Name == name {
			return l, true
		}
	}
	return wire.LinkState{}, false
}

// linkTo adds a link called name to the machine called dest and waits until it runs.
func (m *machine) linkTo(name, dest string) {
	m.t.Helper()
	m.apply(func(f *config.File) { f.Links[name] = &config.Link{Destination: dest} })
	waitFor(m.t, m.name+" link "+name+" runs", func() bool { l, _ := m.link(name); return l.State == wire.StateRunning })
}

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
	a.linkTo("bee", "b")
	waitFor(t, "b reaches a", func() bool { return b.reaches("a") })
	return a, b
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// greeter serves on network and address in the test: it greets each
// connection with name and then echoes. It returns the address.
func greeter(t *testing.T, network, address, name string) string {
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
			go func() {
				defer c.Close()
				c.Write([]byte(name))
				io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// expectGreeting reads the greeting of a greeter and checks that data
// round-trips.
func expectGreeting(t *testing.T, c net.Conn, name string) {
	t.Helper()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, len(name))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != name {
		t.Fatalf("greeting = %q, %v; want %q", buf, err, name)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf = make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
}

// expectClosed checks that c is closed by the other side without data.
func expectClosed(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("read %d bytes, %v; want a closed connection", n, err)
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("the connection stayed open")
	}
}

func dialTCP(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
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

// socksConnect asks the SOCKS5 proxy at proxy to connect to host:port. It
// returns the connection and the reply code.
func socksConnect(t *testing.T, proxy, host string, port int) (net.Conn, byte) {
	t.Helper()
	c := dialTCP(t, proxy)
	c.SetDeadline(time.Now().Add(10 * time.Second))
	req := append([]byte{5, 1, 0, 3, byte(len(host))}, host...)
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := c.Write(append([]byte{5, 1, 0}, req...)); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 12) // method selection, then the reply
	if _, err := io.ReadFull(c, reply); err != nil {
		t.Fatalf("SOCKS reply: %v", err)
	}
	c.SetDeadline(time.Time{})
	return c, reply[3]
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range p {
		n = n*10 + int(r-'0')
	}
	return n
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
	if st.Node.ID != nid("a") || st.Node.Name != "a" || st.Node.Exit {
		t.Fatalf("node = %+v", st.Node)
	}
	if st.Proxy.Listen != "" || len(st.Maps) != 0 {
		t.Fatalf("proxy %+v, maps %+v", st.Proxy, st.Maps)
	}
	if len(st.Links) != 1 {
		t.Fatalf("links = %+v", st.Links)
	}
	l := st.Links[0]
	if l.Name != "bee" || l.State != wire.StateRunning || l.Error != "" || l.Since.IsZero() || l.Peer == nil || l.Peer.ID != nid("b") || l.Peer.Name != "b" {
		t.Fatalf("link = %+v", l)
	}

	// The accepting side lists no link: only links that a machine opened are its own.
	if links := b.d.Status().Links; len(links) != 0 {
		t.Fatalf("b links = %+v", links)
	}

	// Each link has a log.
	data, err := os.ReadFile(filepath.Join(a.logDir, "bee.log"))
	if err != nil || !strings.Contains(string(data), "link up") {
		t.Fatalf("link log = %q, %v", data, err)
	}
}

func TestLinkStates(t *testing.T) {
	w := newWorld(t)
	bare := newMachine(t, w, "bare", true, time.Millisecond)
	bare.start()
	a := startMachine(t, w, "a")
	a.apply(func(f *config.File) {
		for name, dest := range map[string]string{"refused": "refuse", "waiting": "hang", "failed": "secret", "bare": "bare", "me": "a"} {
			f.Links[name] = &config.Link{Destination: dest}
		}
	})
	for _, tt := range []struct {
		link, state, errText string
	}{
		{"refused", wire.StateDown, "connection refused"},
		{"waiting", wire.StateConnecting, ""},
		{"failed", wire.StateDown, "the remote gst failed: " + secret},
		{"bare", wire.StateDown, "no node identity"},
		{"me", wire.StateDown, "this machine"},
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
	waitFor(t, "reconnects", func() bool { l, _ := a.link("refused"); return l.Reconnects >= 3 })
	if l, _ := a.link("waiting"); l.Reconnects != 0 || l.Error != "" {
		t.Fatalf("waiting = %+v", l)
	}
	// The sorted list.
	var names []string
	for _, l := range a.d.Status().Links {
		names = append(names, l.Name)
	}
	if strings.Join(names, " ") != "bare failed me refused waiting" {
		t.Fatalf("links = %v", names)
	}
}

func TestLinkRecoversAfterFailures(t *testing.T) {
	w := newWorld(t)
	startMachine(t, w, "b")
	a := newMachine(t, w, "a", false, time.Millisecond)
	a.flakyOpen = 2
	a.start()
	a.linkTo("f", "flaky")
	l, _ := a.link("f")
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
	a.linkTo("f", "flaky")

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
	a.apply(func(f *config.File) { f.Links["x"] = &config.Link{Destination: "secret"} })
	waitFor(t, "link fails", func() bool { l, _ := a.link("x"); return strings.Contains(l.Error, secret) })
	a.apply(func(f *config.File) { f.Links["x"].Stopped = true }) // flushes the link's log with its last lines
	a.shutdown()

	linkLog, err := os.ReadFile(filepath.Join(a.logDir, "x.log"))
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"daemon log": a.log.String(), "link log": string(linkLog)} {
		if strings.Contains(text, secret) {
			t.Errorf("%s quotes the remote gst:\n%s", name, text)
		}
	}
	if !strings.Contains(string(linkLog), "see gst link list") {
		t.Errorf("link log = %q", linkLog)
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

	st := a.apply(func(f *config.File) {
		f.Links["x"] = &config.Link{Destination: "b"}
		f.Links["y"] = &config.Link{Destination: "b"}
		f.Links["off"] = &config.Link{Destination: "b", Stopped: true}
	})
	if len(st.Links) != 2 {
		t.Fatalf("links = %+v; a stopped link must not run", st.Links)
	}
	waitFor(t, "both run", func() bool {
		x, _ := a.link("x")
		y, _ := a.link("y")
		return x.State == wire.StateRunning && y.State == wire.StateRunning
	})

	// Stopping or removing a link ends it and leaves the others running.
	st = a.apply(func(f *config.File) { f.Links["x"].Stopped = true; delete(f.Links, "y") })
	if len(st.Links) != 0 {
		t.Fatalf("links = %+v", st.Links)
	}
	waitFor(t, "b is no longer linked to", func() bool { return !a.reaches("b") })

	// Starting a stopped link brings it up; reloading leaves a running one alone.
	a.apply(func(f *config.File) { f.Links["off"].Stopped = false })
	waitFor(t, "off runs", func() bool { l, _ := a.link("off"); return l.State == wire.StateRunning })
	since, _ := a.link("off")
	a.apply(func(f *config.File) {})
	if again, _ := a.link("off"); !again.Since.Equal(since.Since) || again.Reconnects != 0 {
		t.Fatalf("a reload restarted a running link: %+v -> %+v", since, again)
	}
}

func TestChangedLinkRestartsOnlyOnRestart(t *testing.T) {
	w := newWorld(t)
	startMachine(t, w, "b")
	a := startMachine(t, w, "a")
	a.apply(func(f *config.File) { f.Links["h"] = &config.Link{Destination: "hang"} })
	waitFor(t, "h connects", func() bool { return len(a.attemptsTo("hang")) == 1 })

	a.apply(func(f *config.File) { f.Links["h"].Destination = "b" })
	if l, _ := a.link("h"); l.State != wire.StateConnecting {
		t.Fatalf("a changed link restarted on reload: %+v", l)
	}

	c, err := Dial(t.Context(), a.socket, "gst test", "sha256:cli")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	st, err := c.Restart(t.Context(), []string{"h", "h"})
	if err != nil || len(st.Links) != 1 {
		t.Fatalf("Restart = %+v, %v", st, err)
	}
	waitFor(t, "h runs with the new destination", func() bool { l, _ := a.link("h"); return l.State == wire.StateRunning })

	// Restart names started links only.
	for _, names := range [][]string{{"nowhere"}, {"h", "nowhere"}} {
		_, err := c.Restart(t.Context(), names)
		wantCode(t, err, wire.CodeNotFound)
	}
	a.apply(func(f *config.File) { f.Links["h"].Stopped = true })
	_, err = c.Restart(t.Context(), []string{"h"})
	wantCode(t, err, wire.CodeNotFound)
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
		f.Proxy = config.ProxyOff
		f.Links["h"] = &config.Link{Destination: "hang"}
	})
	st, err := c.Reload(t.Context())
	if err != nil || len(st.Links) != 1 || st.Node.ID != nid("a") {
		t.Fatalf("Reload = %+v, %v", st, err)
	}
}

func TestReloadChangesNodeIdentity(t *testing.T) {
	a, b := pair(t)
	a.apply(func(f *config.File) { f.Node.Exit = true })
	waitFor(t, "b sees a as an exit", func() bool {
		for _, n := range b.d.mesh.Topology().Nodes {
			if n.ID == nid("a") {
				return n.Exit
			}
		}
		return false
	})
	if !a.d.Status().Node.Exit {
		t.Fatal("status does not show exit")
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
	a.update(func(f *config.File) { f.Links["more"] = &config.Link{Destination: "hang"} })
	if st, err = c.Reload(t.Context()); err != nil || len(st.Links) != 2 {
		t.Fatalf("Reload = %+v, %v", st, err)
	}
	if st, err = c.Restart(t.Context(), []string{"bee"}); err != nil || len(st.Links) != 2 {
		t.Fatalf("Restart = %+v, %v", st, err)
	}

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

func TestConnectRelaysToTargetOnAnotherMachine(t *testing.T) {
	a, b := pair(t)
	tcp := greeter(t, "tcp", "127.0.0.1:0", "tcp-on-b")
	sock := filepath.Join(a.w.dir, "t.sock")
	greeter(t, "unix", sock, "unix-on-b")

	for _, tt := range []struct {
		name, node string
		target     wire.Target
		greeting   string
	}{
		{"tcp by node name", "b", wire.Target{Network: "tcp", Address: tcp}, "tcp-on-b"},
		{"tcp by link name", "bee.gst", wire.Target{Network: "tcp", Address: tcp}, "tcp-on-b"},
		{"unix socket", "b", wire.Target{Network: "unix", Address: sock}, "unix-on-b"},
		{"from the accepting side", "a", wire.Target{Network: "tcp", Address: tcp}, "tcp-on-b"}, // a's own machine
	} {
		t.Run(tt.name, func(t *testing.T) {
			from := a
			if tt.name == "from the accepting side" {
				from = b
			}
			c, err := Dial(t.Context(), from.socket, "gst cli", "sha256:cli")
			if err != nil {
				t.Fatal(err)
			}
			conn, err := c.Connect(t.Context(), tt.node, tt.target)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			expectGreeting(t, conn, tt.greeting)
		})
	}

	for _, tt := range []struct {
		name, node string
		target     wire.Target
		code       string
	}{
		{"unknown machine", "nowhere", wire.Target{Network: "tcp", Address: tcp}, wire.CodeNotFound},
		{"not an exit", "b", wire.Target{Network: "tcp", Address: "192.0.2.1:80"}, wire.CodeForbidden},
		{"bad target", "b", wire.Target{Network: "tcp", Address: "x"}, wire.CodeInvalidParams},
		{"refused", "b", wire.Target{Network: "tcp", Address: freeAddr(t)}, wire.CodeFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Dial(t.Context(), a.socket, "gst cli", "sha256:cli")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			conn, err := c.Connect(t.Context(), tt.node, tt.target)
			if err == nil {
				conn.Close()
				t.Fatal("Connect succeeded")
			}
			wantCode(t, err, tt.code)
		})
	}
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
	proxy, mapAddr := freeAddr(t), freeAddr(t)
	a.apply(func(f *config.File) {
		f.Proxy = proxy
		f.Maps = []config.Map{{Listen: mapAddr, Machine: "b", Target: "localhost:1"}}
	})
	a.linkTo("bee", "b")
	dialTCP(t, proxy).Close()

	if err := a.shutdown(); err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{proxy, mapAddr} {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			t.Errorf("%s still listens after the daemon stopped", addr)
		}
	}
	st := a.d.Status()
	if len(st.Links) != 0 || st.Proxy.Listen != "" || len(st.Maps) != 0 {
		t.Fatalf("status after stop = %+v", st)
	}
}
