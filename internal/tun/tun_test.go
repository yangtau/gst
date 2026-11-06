package tun

import (
	"context"
	"errors"
	"io"
	"log"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSpecParse(t *testing.T) {
	good := Spec{Subnet: "10.99.0.0/16", Address: "10.99.1.2", Hosts: map[string]string{"devbox.gst": "10.99.3.4"}}
	if _, err := good.parse(); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Spec{
		"public subnet":          {Subnet: "8.8.8.0/24", Address: "8.8.8.8"},
		"address outside":        {Subnet: "10.99.0.0/16", Address: "10.98.1.2"},
		"bad address":            {Subnet: "10.99.0.0/16", Address: "nonsense"},
		"host outside":           {Subnet: "10.99.0.0/16", Address: "10.99.1.2", Hosts: map[string]string{"devbox.gst": "10.98.3.4"}},
		"host of another domain": {Subnet: "10.99.0.0/16", Address: "10.99.1.2", Hosts: map[string]string{"example.com": "10.99.3.4"}},
		// A name is a line of /etc/hosts.
		"host with a newline": {Subnet: "10.99.0.0/16", Address: "10.99.1.2", Hosts: map[string]string{"a\n8.8.8.8 example.com.gst": "10.99.3.4"}},
	} {
		if _, err := s.parse(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestWriteHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	const original = "127.0.0.1 localhost\n10.0.0.1 router"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	address := netip.MustParseAddr("10.99.1.2")
	if err := writeHosts(path, map[string]netip.Addr{"laptop.gst": address, "devbox.gst": address}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := original + "\n# gst begin\n10.99.1.2 devbox.gst\n10.99.1.2 laptop.gst\n# gst end\n"
	if string(got) != want {
		t.Fatalf("hosts:\n%s", got)
	}
	if err := writeHosts(path, map[string]netip.Addr{"laptop.gst": address}); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if strings.Contains(string(got), "devbox") || strings.Count(string(got), hostsBegin) != 1 {
		t.Fatalf("hosts:\n%s", got)
	}
	if err := writeHosts(path, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != original+"\n" {
		t.Fatalf("hosts:\n%q", got)
	}
}

type commands struct{ calls []string }

func (c *commands) run(name string, args ...string) error {
	c.calls = append(c.calls, strings.Join(append([]string{name}, args...), " "))
	return nil
}

func TestDarwinSystem(t *testing.T) {
	var c commands
	d := &darwin{run: c.run, resolver: filepath.Join(t.TempDir(), "resolver", "gst")}
	address, subnet := netip.MustParseAddr("10.99.1.2"), netip.MustParsePrefix("10.99.0.0/16")
	if err := d.number("utun7", address, subnet); err != nil {
		t.Fatal(err)
	}
	d.unnumber("utun7", address, subnet)
	want := []string{
		"/sbin/ifconfig utun7 inet 10.99.1.2 10.99.1.2 mtu 1168 up",
		"/sbin/route -q -n add -inet 10.99.0.0/16 -interface utun7",
		"/sbin/route -q -n delete -inet 10.99.0.0/16 -interface utun7",
		"/sbin/ifconfig utun7 inet 10.99.1.2 delete",
	}
	if !slices.Equal(c.calls, want) {
		t.Fatalf("calls: %q", c.calls)
	}
	if err := d.resolve(nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(d.resolver)
	if err != nil || !strings.Contains(string(got), "nameserver 127.0.0.1\nport 15353\n") {
		t.Fatalf("resolver: %q, %v", got, err)
	}
	d.unresolve()
	if _, err := os.Stat(d.resolver); err == nil {
		t.Fatal("the resolver file is left")
	}
}

func TestLinuxSystem(t *testing.T) {
	var c commands
	dir := t.TempDir()
	l := &linux{
		run:      c.run,
		resolved: filepath.Join(dir, "resolve"),
		dropIn:   filepath.Join(dir, "resolved.conf.d", "gst.conf"),
		hosts:    filepath.Join(dir, "hosts"),
	}
	address, subnet := netip.MustParseAddr("10.99.1.2"), netip.MustParsePrefix("10.99.0.0/16")
	if err := l.number("gst0", address, subnet); err != nil {
		t.Fatal(err)
	}
	l.unnumber("gst0", address, subnet)
	want := []string{
		"ip link set dev gst0 mtu 1168 up",
		"ip address replace 10.99.1.2/16 dev gst0",
		"ip address delete 10.99.1.2/16 dev gst0",
	}
	if !slices.Equal(c.calls, want) {
		t.Fatalf("calls: %q", c.calls)
	}

	// Without systemd-resolved, the names go in the hosts file.
	hosts := map[string]netip.Addr{"devbox.gst": address}
	if err := l.resolve(hosts); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(l.hosts); !strings.Contains(string(got), "10.99.1.2 devbox.gst") {
		t.Fatalf("hosts: %q", got)
	}
	l.unresolve()
	if got, _ := os.ReadFile(l.hosts); strings.Contains(string(got), "devbox") {
		t.Fatalf("hosts: %q", got)
	}

	// With it, .gst goes to the daemon, and it restarts only for a change.
	if err := os.Mkdir(l.resolved, 0o755); err != nil {
		t.Fatal(err)
	}
	c.calls = nil
	for range 2 {
		if err := l.resolve(hosts); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(l.dropIn)
	if err != nil || !strings.Contains(string(got), "DNS=127.0.0.1:15353\nDomains=~gst\n") {
		t.Fatalf("drop-in: %q, %v", got, err)
	}
	if !slices.Equal(c.calls, []string{"systemctl restart systemd-resolved"}) {
		t.Fatalf("calls: %q", c.calls)
	}
	l.unresolve()
	if _, err := os.Stat(l.dropIn); err == nil {
		t.Fatal("the drop-in is left")
	}
}

// fakeSystem records what the helper does.
type fakeSystem struct {
	mu     sync.Mutex
	events []string
	peer   *os.File // the other end of the "device"
	fail   error
}

func (f *fakeSystem) add(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func (f *fakeSystem) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.events)
}

func (f *fakeSystem) open() (*os.File, string, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, "", err
	}
	f.peer = w
	f.add("open")
	return r, "fake0", nil
}

func (f *fakeSystem) number(name string, address netip.Addr, subnet netip.Prefix) error {
	f.add("number " + name + " " + address.String() + " " + subnet.String())
	return f.fail
}

func (f *fakeSystem) unnumber(name string, address netip.Addr, subnet netip.Prefix) {
	f.add("unnumber " + name + " " + address.String() + " " + subnet.String())
}

func (f *fakeSystem) resolve(hosts map[string]netip.Addr) error {
	f.add("resolve " + strings.Join(slices.Sorted(maps.Keys(hosts)), ","))
	return nil
}

func (f *fakeSystem) unresolve() { f.add("unresolve") }

func runHelper(t *testing.T, sys system) string {
	t.Helper()
	// The path of a socket has a short limit, which t.TempDir can exceed.
	dir, err := os.MkdirTemp("", "gst")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "net.sock")
	h := &Helper{Socket: socket, UID: os.Getuid(), GID: os.Getgid(), Log: log.New(io.Discard, "", 0), sys: sys}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			return socket
		}
		if time.Now().After(deadline) {
			t.Fatal("the helper does not listen")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitEvents(t *testing.T, f *fakeSystem, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Equal(f.seen(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("events:\n%q\nwant:\n%q", f.seen(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHelper(t *testing.T) {
	sys := &fakeSystem{}
	socket := runHelper(t, sys)
	info, err := os.Stat(socket)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v, %v", info, err)
	}

	spec := Spec{Subnet: "10.99.0.0/16", Address: "10.99.1.2", Hosts: map[string]string{"devbox.gst": "10.99.3.4"}}
	c, err := Dial(socket, spec)
	if err != nil {
		t.Fatal(err)
	}
	if name, _ := c.Device().Name(); name != "fake0" {
		t.Fatalf("name %q", name)
	}
	// The file came through the socket: what the helper's end gets written
	// reaches the client's.
	if _, err := sys.peer.WriteString("packet"); err != nil {
		t.Fatal(err)
	}
	base := []string{"open", "number fake0 10.99.1.2 10.99.0.0/16", "resolve devbox.gst"}
	waitEvents(t, sys, base...)

	// The same again changes nothing, and works as a check that the helper
	// is there.
	if name, err := c.Set(spec); err != nil || name != "fake0" {
		t.Fatalf("Set: %q, %v", name, err)
	}
	waitEvents(t, sys, base...)

	spec.Hosts["laptop.gst"] = "10.99.1.2"
	if _, err := c.Set(spec); err != nil {
		t.Fatal(err)
	}
	moved := Spec{Subnet: "10.98.0.0/16", Address: "10.98.1.2"}
	if _, err := c.Set(moved); err != nil {
		t.Fatal(err)
	}
	base = append(base, "resolve devbox.gst,laptop.gst", "unnumber fake0 10.99.1.2 10.99.0.0/16", "number fake0 10.98.1.2 10.98.0.0/16", "resolve ")
	waitEvents(t, sys, base...)

	if _, err := c.Set(Spec{Subnet: "8.8.8.0/24", Address: "8.8.8.8"}); err == nil {
		t.Fatal("the helper took a public subnet")
	}
	waitEvents(t, sys, base...)

	c.Device().Close()
	c.Close()
	base = append(base, "unresolve", "unnumber fake0 10.98.1.2 10.98.0.0/16")
	waitEvents(t, sys, base...)
}

func TestHelperFailure(t *testing.T) {
	sys := &fakeSystem{fail: errors.New("no such device")}
	socket := runHelper(t, sys)
	_, err := Dial(socket, Spec{Subnet: "10.99.0.0/16", Address: "10.99.1.2"})
	if err == nil || !strings.Contains(err.Error(), "no such device") {
		t.Fatalf("Dial: %v", err)
	}
}

// A new daemon takes over from the one before, which may be on its way out.
func TestHelperServesTheLatestDaemon(t *testing.T) {
	sys := &fakeSystem{}
	socket := runHelper(t, sys)
	spec := Spec{Subnet: "10.99.0.0/16", Address: "10.99.1.2"}
	first, err := Dial(socket, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Dial(socket, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := first.Set(spec); err == nil {
		t.Fatal("the old daemon still has the interface")
	}
	if _, err := second.Set(spec); err != nil {
		t.Fatal(err)
	}
}

func TestDialWithoutHelper(t *testing.T) {
	_, err := Dial(filepath.Join(t.TempDir(), "none.sock"), Spec{})
	if !errors.Is(err, ErrNoHelper) {
		t.Fatalf("Dial: %v", err)
	}
}
