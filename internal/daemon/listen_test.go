package daemon

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// listens reports whether something accepts connections on addr.
func listens(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func TestProxyListener(t *testing.T) {
	w := newWorld(t)
	a := startMachine(t, w, "a")
	p1, p2 := freeAddr(t), freeAddr(t)

	st := a.apply(func(f *config.File) { f.Proxy = p1 })
	if st.Proxy.Listen != p1 || st.Proxy.Error != "" || !listens(p1) {
		t.Fatalf("proxy = %+v", st.Proxy)
	}
	// Reloading without a change keeps the listener.
	a.d.mu.Lock()
	kept := a.d.proxy
	a.d.mu.Unlock()
	a.apply(func(f *config.File) {})
	a.d.mu.Lock()
	same := a.d.proxy == kept
	a.d.mu.Unlock()
	if !same {
		t.Fatal("a reload without changes replaced the proxy")
	}

	// It moves to another address.
	st = a.apply(func(f *config.File) { f.Proxy = p2 })
	if st.Proxy.Listen != p2 || !listens(p2) || listens(p1) {
		t.Fatalf("after the move: %+v, p1 listens %v", st.Proxy, listens(p1))
	}
	// "off" stops it.
	st = a.apply(func(f *config.File) { f.Proxy = config.ProxyOff })
	if st.Proxy.Listen != "" || listens(p2) {
		t.Fatalf("after off: %+v, p2 listens %v", st.Proxy, listens(p2))
	}
	// And it comes back.
	st = a.apply(func(f *config.File) { f.Proxy = p1 })
	if st.Proxy.Listen != p1 || !listens(p1) {
		t.Fatalf("after on: %+v", st.Proxy)
	}
}

func TestListenErrorsAreReportedAndRetried(t *testing.T) {
	w := newWorld(t)
	a := startMachine(t, w, "a")
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	taken := occupied.Addr().String()
	free := freeAddr(t)
	sockTaken := filepath.Join(w.dir, "taken.sock")
	sockLn, err := net.Listen("unix", sockTaken)
	if err != nil {
		t.Fatal(err)
	}
	defer sockLn.Close()

	st := a.apply(func(f *config.File) {
		f.Proxy = taken
		f.Maps = []config.Map{
			{Listen: taken[:strings.LastIndex(taken, ":")] + ":1", Machine: "b", Target: "localhost:1"}, // privileged: fails without root
			{Listen: sockTaken, Machine: "b", Target: "localhost:1"},
			{Listen: free, Machine: "b", Target: "localhost:1"},
		}
	})
	if st.Proxy.Error == "" || strings.Contains(st.Proxy.Error, "listen tcp") {
		t.Fatalf("proxy error = %q, want the cause without the operation", st.Proxy.Error)
	}
	bySock := map[string]wire.Listener{}
	for _, m := range st.Maps {
		bySock[m.Listen] = m.Listener
	}
	if bySock[sockTaken].Error != "socket in use" {
		t.Fatalf("socket map = %+v", bySock[sockTaken])
	}
	if bySock[free].Error != "" || !listens(free) {
		t.Fatalf("free map = %+v", bySock[free])
	}

	// Once the address is free, the next reload listens.
	occupied.Close()
	sockLn.Close()
	st = a.apply(func(f *config.File) {})
	if st.Proxy.Error != "" || !listens(taken) {
		t.Fatalf("proxy after freeing = %+v", st.Proxy)
	}
	for _, m := range st.Maps {
		if m.Listen == sockTaken && m.Error != "" {
			t.Fatalf("socket map after freeing = %+v", m)
		}
	}
}

func TestMapsAreAddedRemovedAndChanged(t *testing.T) {
	w := newWorld(t)
	startMachine(t, w, "b")
	a := startMachine(t, w, "a")
	a.linkTo("bee", "b")
	one := greeter(t, "tcp", "127.0.0.1:0", "one")
	two := greeter(t, "tcp", "127.0.0.1:0", "two")
	l1, l2 := freeAddr(t), freeAddr(t)

	st := a.apply(func(f *config.File) {
		f.Maps = []config.Map{{Listen: l2, Machine: "b", Target: one}, {Listen: l1, Machine: "bee", Target: one}}
	})
	if len(st.Maps) != 2 || st.Maps[0].Listen != l2 || st.Maps[1].Machine != "bee" {
		t.Fatalf("maps = %+v, want both in the order of the configuration", st.Maps)
	}
	expectGreeting(t, dialTCP(t, l1), "one")
	expectGreeting(t, dialTCP(t, l2), "one")

	// A map whose target changed is replaced; one that did not keeps serving.
	open := dialTCP(t, l1)
	expectGreeting(t, open, "one")
	a.apply(func(f *config.File) {
		f.Maps = []config.Map{{Listen: l2, Machine: "b", Target: two}, {Listen: l1, Machine: "bee", Target: one}}
	})
	expectGreeting(t, dialTCP(t, l2), "two")
	expectGreeting(t, dialTCP(t, l1), "one")
	// Connections that are open stay up.
	open.SetDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 4)
	if _, err := open.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(open, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("open connection: %q, %v", buf, err)
	}

	// A removed map stops listening.
	st = a.apply(func(f *config.File) { f.Maps = f.Maps[:1] })
	if len(st.Maps) != 1 || st.Maps[0].Listen != l2 || !listens(l2) || listens(l1) {
		t.Fatalf("maps = %+v; want only %s", st.Maps, l2)
	}
	st = a.apply(func(f *config.File) { f.Maps = nil })
	if len(st.Maps) != 0 || listens(l1) || listens(l2) {
		t.Fatalf("maps = %+v", st.Maps)
	}
}

func TestMapsToTCPAndUnixTargetsOnAnotherMachine(t *testing.T) {
	a, _ := pair(t)
	tcp := greeter(t, "tcp", "127.0.0.1:0", "tcp-target")
	sock := filepath.Join(a.w.dir, "target.sock")
	greeter(t, "unix", sock, "unix-target")
	listenSock := filepath.Join(a.w.dir, "map.sock")
	tcpListen, unixTargetListen, nowhereListen := freeAddr(t), freeAddr(t), freeAddr(t)

	st := a.apply(func(f *config.File) {
		f.Maps = []config.Map{
			{Listen: tcpListen, Machine: "b", Target: tcp},
			{Listen: unixTargetListen, Machine: "b", Target: sock},
			{Listen: listenSock, Machine: "b", Target: tcp}, // listening on a socket
			{Listen: nowhereListen, Machine: "nowhere", Target: tcp},
		}
	})
	for _, m := range st.Maps {
		if m.Error != "" {
			t.Fatalf("map %+v", m)
		}
	}
	expectGreeting(t, dialTCP(t, tcpListen), "tcp-target")
	expectGreeting(t, dialTCP(t, unixTargetListen), "unix-target")
	c, err := net.Dial("unix", listenSock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	expectGreeting(t, c, "tcp-target")
	if info, err := os.Stat(listenSock); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("map socket = %v, %v; want mode 0600", info, err)
	}
	// A map to a machine that is not in the mesh accepts and closes.
	expectClosed(t, dialTCP(t, nowhereListen))
	// So does one whose target refuses.
	refused := freeAddr(t)
	nothing := freeAddr(t)
	a.apply(func(f *config.File) { f.Maps = []config.Map{{Listen: refused, Machine: "b", Target: nothing}} })
	expectClosed(t, dialTCP(t, refused))
}

func TestMapsWorkBothWaysAcrossALink(t *testing.T) {
	_, b := pair(t)
	onA := greeter(t, "tcp", "127.0.0.1:0", "on-a")
	listen := freeAddr(t)
	// b did not open the link, but can reach a through it.
	b.apply(func(f *config.File) { f.Maps = []config.Map{{Listen: listen, Machine: "a", Target: onA}} })
	expectGreeting(t, dialTCP(t, listen), "on-a")
}

// proxied sets up a with a proxy and returns its address.
func proxied(t *testing.T, a *machine) string {
	t.Helper()
	addr := freeAddr(t)
	a.apply(func(f *config.File) { f.Proxy = addr })
	return addr
}

func TestProxyConnectsMachinesUnderGst(t *testing.T) {
	a, b := pair(t)
	addr := proxied(t, a)
	echo := greeter(t, "tcp", "127.0.0.1:0", "service")
	port := portOf(t, echo)

	for _, host := range []string{"b.gst", "B.GST", "b.gst.", "bee.gst", nid("b")[:8] + ".gst"} {
		c, code := socksConnect(t, addr, host, port)
		if code != 0 {
			t.Fatalf("%s: reply code %d", host, code)
		}
		expectGreeting(t, c, "service")
	}

	// Unknown machines are unreachable, and "a.gst" is this machine.
	if _, code := socksConnect(t, addr, "nowhere.gst", port); code != 4 {
		t.Errorf("unknown machine: reply code %d, want 4 (host unreachable)", code)
	}
	c, code := socksConnect(t, addr, "a.gst", port)
	if code != 0 {
		t.Fatalf("a.gst: reply code %d", code)
	}
	expectGreeting(t, c, "service")
	if _, code := socksConnect(t, addr, "b.gst", portOf(t, freeAddr(t))); code != 1 {
		t.Errorf("refused: reply code %d, want 1 (general failure)", code)
	}

	// It works the other way too: b's proxy reaches a through the link.
	bAddr := proxied(t, b)
	c, code = socksConnect(t, bAddr, "a.gst", port)
	if code != 0 {
		t.Fatalf("b to a: reply code %d", code)
	}
	expectGreeting(t, c, "service")

	// HTTP CONNECT too.
	h := dialTCP(t, addr)
	h.SetDeadline(time.Now().Add(10 * time.Second))
	h.Write([]byte("CONNECT b.gst:" + echoPort(echo) + " HTTP/1.1\r\nHost: b.gst\r\n\r\n"))
	br := bufio.NewReader(h)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT = %v, %v", resp, err)
	}
	greet := make([]byte, len("service"))
	if _, err := io.ReadFull(br, greet); err != nil || string(greet) != "service" {
		t.Fatalf("greeting = %q, %v", greet, err)
	}
	h2 := dialTCP(t, addr)
	h2.Write([]byte("CONNECT nowhere.gst:80 HTTP/1.1\r\nHost: nowhere.gst\r\n\r\n"))
	resp, err = http.ReadResponse(bufio.NewReader(h2), nil)
	if err != nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("CONNECT to unknown = %v, %v", resp, err)
	}
}

func echoPort(addr string) string { return addr[strings.LastIndex(addr, ":")+1:] }

func TestProxyRoutes(t *testing.T) {
	a, b := pair(t)
	addr := proxied(t, a)
	lan := lanAddress(t)
	service := greeter(t, "tcp", lan+":0", "service")
	port := portOf(t, service)

	connects := func(t *testing.T, want byte) net.Conn {
		t.Helper()
		c, code := socksConnect(t, addr, lan, port)
		if code != want {
			t.Fatalf("reply code %d, want %d", code, want)
		}
		return c
	}

	// Without a route a connects directly.
	expectGreeting(t, connects(t, 0), "service")

	// A route sends it through b, which must be an exit for addresses off its machine.
	a.apply(func(f *config.File) { f.Routes = config.Routes{{Match: "default", Via: "b"}} })
	connects(t, 2) // connection not allowed by ruleset
	b.apply(func(f *config.File) { f.Node.Exit = true })
	expectGreeting(t, connects(t, 0), "service")

	// The most specific route wins; the routes are replaced on reload.
	a.apply(func(f *config.File) {
		f.Routes = config.Routes{{Match: "default", Via: "b"}, {Match: lan + "/32", Via: "nowhere"}}
	})
	connects(t, 4) // host unreachable: nowhere is not in the mesh
	a.apply(func(f *config.File) { f.Routes = config.Routes{{Match: "other.example.com", Via: "b"}} })
	expectGreeting(t, connects(t, 0), "service") // no longer matches: direct
	a.apply(func(f *config.File) { f.Routes = nil })
	expectGreeting(t, connects(t, 0), "service")

	// The machines under .gst do not depend on routes.
	a.apply(func(f *config.File) { f.Routes = config.Routes{{Match: "default", Via: "nowhere"}} })
	local := greeter(t, "tcp", "127.0.0.1:0", "local")
	c, code := socksConnect(t, addr, "b.gst", portOf(t, local))
	if code != 0 {
		t.Fatalf("b.gst with a default route: code %d", code)
	}
	expectGreeting(t, c, "local")
}
