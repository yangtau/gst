package vnet

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.zx2c4.com/wireguard/conn"
	wgtun "golang.zx2c4.com/wireguard/tun"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/tun"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func prefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }

// macOS routes a machine's own address into the interface, and only this
// brings such packets back.
func TestPacketsToThisMachine(t *testing.T) {
	var w world
	a := start(t, &w, "a0", "laptop")
	b := start(t, &w, "b0", "devbox")
	a.manager.Configure(t.Context(), &config.Net{})
	b.manager.Configure(t.Context(), &config.Net{})
	waitFor(t, "a is up", up(a, 1))
	waitFor(t, "b is up", up(b, 1))
	wantPacket(t, a, b, "before")

	self := a.manager.Status().Address
	sent := ipPacket(self, self, "to myself")
	a.device().in <- sent
	select {
	case got := <-a.device().out:
		if !bytes.Equal(got, sent) {
			t.Fatalf("got %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the packet did not come back")
	}
	wantPacket(t, a, b, "after")
}

func TestChooseSubnet(t *testing.T) {
	a, ok := chooseSubnet("one", nil)
	if !ok || !prefix("10.0.0.0/8").Contains(a.Addr()) || a.Bits() != 16 {
		t.Fatalf("chose %v, %v", a, ok)
	}
	if b, _ := chooseSubnet("one", nil); b != a {
		t.Fatal("the choice is not stable")
	}
	used := []netip.Prefix{a, prefix("192.168.1.0/24")}
	b, ok := chooseSubnet("one", used)
	if !ok || b == a {
		t.Fatalf("chose %v, %v with %v in use", b, ok, a)
	}
	// All of 10/8 in use leaves the rest.
	c, ok := chooseSubnet("one", []netip.Prefix{prefix("10.0.0.0/8")})
	if !ok || !prefix("172.16.0.0/12").Contains(c.Addr()) {
		t.Fatalf("chose %v, %v", c, ok)
	}
	if d, ok := chooseSubnet("one", []netip.Prefix{prefix("10.0.0.0/8"), prefix("172.16.0.0/12")}); ok {
		t.Fatalf("chose %v with nothing free", d)
	}
}

func TestChooseAddress(t *testing.T) {
	subnet := prefix("10.1.2.0/28")
	taken := map[netip.Addr]bool{}
	for range 14 {
		a, err := chooseAddress("node", subnet, taken)
		if err != nil {
			t.Fatal(err)
		}
		if !subnet.Contains(a) || a == subnet.Addr() || a == netip.MustParseAddr("10.1.2.15") || taken[a] {
			t.Fatalf("chose %s from %v", a, taken)
		}
		taken[a] = true
	}
	if a, err := chooseAddress("node", subnet, taken); err == nil {
		t.Fatalf("chose %s from a full subnet", a)
	}
}

func TestNewer(t *testing.T) {
	if !newer("10.2.0.0/16", 2, "10.1.0.0/16", 1) || newer("10.2.0.0/16", 1, "10.1.0.0/16", 2) {
		t.Fatal("the later choice does not win")
	}
	if newer("10.2.0.0/16", 1, "10.1.0.0/16", 1) == newer("10.1.0.0/16", 1, "10.2.0.0/16", 1) {
		t.Fatal("two nodes disagree on a tie")
	}
}

func TestLocalNetworksLeaveOutWhatCannotConflict(t *testing.T) {
	for _, p := range LocalNetworks("") {
		if p.Bits() == 0 || p.Addr().IsLoopback() || !p.Addr().Is4() {
			t.Errorf("LocalNetworks has %s", p)
		}
	}
}

func query(t *testing.T, name string, kind dnsmessage.Type) []byte {
	t.Helper()
	m := dnsmessage.Message{Header: dnsmessage.Header{ID: 7, RecursionDesired: true}, Questions: []dnsmessage.Question{
		{Name: dnsmessage.MustNewName(name), Type: kind, Class: dnsmessage.ClassINET},
	}}
	packed, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return packed
}

func TestDNSAnswer(t *testing.T) {
	address := netip.MustParseAddr("10.99.1.2")
	lookup := func(name string) (netip.Addr, bool) { return address, name == "devbox" }
	reply := func(name string, kind dnsmessage.Type) dnsmessage.Message {
		t.Helper()
		var m dnsmessage.Message
		if err := m.Unpack(answer(query(t, name, kind), lookup)); err != nil {
			t.Fatal(err)
		}
		if m.ID != 7 || !m.Response {
			t.Fatalf("bad header: %+v", m.Header)
		}
		return m
	}
	m := reply("DevBox.gst.", dnsmessage.TypeA)
	if len(m.Answers) != 1 || m.Answers[0].Body.(*dnsmessage.AResource).A != address.As4() {
		t.Fatalf("A: %+v", m)
	}
	if m := reply("devbox.gst.", dnsmessage.TypeAAAA); m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
		t.Fatalf("AAAA: %+v", m)
	}
	if m := reply("nobody.gst.", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeNameError {
		t.Fatalf("unknown machine: %+v", m)
	}
	if m := reply("devbox.example.", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeNameError {
		t.Fatalf("another domain: %+v", m)
	}
	if got := answer([]byte("nonsense"), lookup); got != nil {
		t.Fatalf("answered nonsense: %v", got)
	}
}

// world is a mesh of fake nodes that pass packets to each other.
type world struct {
	mu    sync.Mutex
	nodes map[string]*fakeMesh
}

func (w *world) topology(self string) wire.Topology {
	w.mu.Lock()
	defer w.mu.Unlock()
	topo := wire.Topology{Self: self}
	alone := false
	for _, n := range w.nodes {
		alone = alone || n.info.ID == self && n.alone
	}
	for _, id := range []string{"a0", "b0", "c0"} {
		if n := w.nodes[id]; n != nil && (n.info.ID == self || !alone && !n.alone) {
			topo.Nodes = append(topo.Nodes, wire.TopoNode{ID: n.info.ID, Name: n.info.Name, Reachable: true, Net: n.net})
		}
	}
	return topo
}

func (w *world) add(id, name string) *fakeMesh {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.nodes == nil {
		w.nodes = map[string]*fakeMesh{}
	}
	fullID := id + strings.Repeat("0", 30)
	n := &fakeMesh{world: w, info: wire.NodeInfo{ID: fullID, Name: name}, changed: make(chan struct{})}
	w.nodes[id] = n
	return n
}

func (w *world) changed() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, n := range w.nodes {
		close(n.changed)
		n.changed = make(chan struct{})
	}
}

type fakeMesh struct {
	world    *world
	info     wire.NodeInfo
	net      *wire.NetInfo
	changed  chan struct{}
	handle   func(string, []byte)
	networks []string
	alone    bool   // sees no other node, and none sees it
	sendAs   string // the id that packets seem to come from, when not empty
}

// setAlone cuts the node off the others, or joins it to them.
func (m *fakeMesh) setAlone(alone bool) {
	m.world.mu.Lock()
	m.alone = alone
	m.world.mu.Unlock()
	m.world.changed()
}

func (m *fakeMesh) Self() wire.NodeInfo {
	m.world.mu.Lock()
	defer m.world.mu.Unlock()
	return m.info
}
func (m *fakeMesh) Topology() wire.Topology {
	return m.world.topology(m.info.ID)
}
func (m *fakeMesh) Changed() <-chan struct{} {
	m.world.mu.Lock()
	defer m.world.mu.Unlock()
	return m.changed
}
func (m *fakeMesh) SetNet(info *wire.NetInfo) {
	m.world.mu.Lock()
	same := (m.net == nil) == (info == nil) && (info == nil || *m.net == *info)
	m.net = info
	m.world.mu.Unlock()
	if !same {
		m.world.changed()
	}
}
func (m *fakeMesh) SendPacket(node string, packet []byte) {
	m.world.mu.Lock()
	var handle func(string, []byte)
	for _, n := range m.world.nodes {
		if n.info.ID == node {
			handle = n.handle
		}
	}
	from := cmp.Or(m.sendAs, m.info.ID)
	m.world.mu.Unlock()
	if handle != nil {
		handle(from, packet)
	}
}
func (m *fakeMesh) HandlePackets(handle func(string, []byte)) {
	m.world.mu.Lock()
	defer m.world.mu.Unlock()
	m.handle = handle
}
func (m *fakeMesh) PacketPath(string) string     { return wire.PathSSH }
func (m *fakeMesh) PacketEndpoint(string) string { return "" }
func (m *fakeMesh) Call(_ context.Context, ref, method string, _, result any) error {
	if method != wire.MethodNetNetworks {
		return errors.New("unknown method")
	}
	m.world.mu.Lock()
	defer m.world.mu.Unlock()
	for _, n := range m.world.nodes {
		if n.info.ID == ref {
			result.(*wire.Networks).Prefixes = n.networks
			return nil
		}
	}
	return errors.New("unknown node")
}

// fakeInterface is an interface whose packets the test reads and writes.
type fakeInterface struct {
	in, out chan []byte
	closed  chan struct{}
	events  chan wgtun.Event
	once    sync.Once

	mu   sync.Mutex
	spec tun.Spec
}

func newFakeInterface(spec tun.Spec) *fakeInterface {
	f := &fakeInterface{in: make(chan []byte, 16), out: make(chan []byte, 16), closed: make(chan struct{}), events: make(chan wgtun.Event, 1), spec: spec}
	f.events <- wgtun.EventUp
	return f
}

func (f *fakeInterface) Device() wgtun.Device { return f }
func (f *fakeInterface) Set(spec tun.Spec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spec = spec
	return "fake0", nil
}
func (f *fakeInterface) current() tun.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spec
}
func (f *fakeInterface) File() *os.File             { return nil }
func (f *fakeInterface) MTU() (int, error)          { return tun.MTU, nil }
func (f *fakeInterface) Name() (string, error)      { return "fake0", nil }
func (f *fakeInterface) Events() <-chan wgtun.Event { return f.events }
func (f *fakeInterface) BatchSize() int             { return 1 }
func (f *fakeInterface) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}
func (f *fakeInterface) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case p := <-f.in:
		sizes[0] = copy(bufs[0][offset:], p)
		return 1, nil
	case <-f.closed:
		return 0, os.ErrClosed
	}
}
func (f *fakeInterface) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		select {
		case f.out <- append([]byte(nil), b[offset:]...):
		case <-f.closed:
			return 0, os.ErrClosed
		}
	}
	return len(bufs), nil
}

type testNode struct {
	mesh    *fakeMesh
	manager *Manager
	mu      sync.Mutex
	iface   *fakeInterface
	dir     string
}

func (n *testNode) device() *fakeInterface {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.iface
}

func start(t *testing.T, w *world, id, name string, networks ...netip.Prefix) *testNode {
	t.Helper()
	n := &testNode{mesh: w.add(id, name), dir: t.TempDir()}
	for _, p := range networks {
		n.mesh.networks = append(n.mesh.networks, p.String())
	}
	n.run(t, networks...)
	return n
}

func (n *testNode) run(t *testing.T, networks ...netip.Prefix) {
	n.manager = New(Config{
		Mesh:      n.mesh,
		Log:       log.New(io.Discard, "", 0),
		StatePath: filepath.Join(n.dir, "net.json"),
		Open: func(spec tun.Spec) (Interface, error) {
			f := newFakeInterface(spec)
			n.mu.Lock()
			defer n.mu.Unlock()
			n.iface = f
			return f, nil
		},
		Networks: func(string) []netip.Prefix { return networks },
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		n.manager.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
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

// ipPacket is an IPv4 packet with a body, as far as WireGuard looks.
func ipPacket(from, to, body string) []byte {
	p := make([]byte, 20, 20+len(body))
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:], uint16(20+len(body)))
	src, dst := netip.MustParseAddr(from).As4(), netip.MustParseAddr(to).As4()
	copy(p[12:], src[:])
	copy(p[16:], dst[:])
	return append(p, body...)
}

// wantPacket sends a packet into from's interface until it comes out of
// to's, which proves the handshake and the keys.
func wantPacket(t *testing.T, from, to *testNode, body string) {
	t.Helper()
	src, dst := from.manager.Status().Address, to.manager.Status().Address
	deadline := time.After(10 * time.Second)
	for {
		select {
		case from.device().in <- ipPacket(src, dst, body):
		default:
		}
		select {
		case p := <-to.device().out:
			if strings.HasSuffix(string(p), body) {
				return
			}
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatalf("no packet from %s to %s", src, dst)
		}
	}
}

func up(n *testNode, peers int) func() bool {
	return func() bool {
		st := n.manager.Status()
		return st != nil && st.Interface != "" && st.Error == "" && len(st.Peers) == peers
	}
}

func TestTwoMachines(t *testing.T) {
	var w world
	a := start(t, &w, "a0", "laptop", prefix("192.168.1.0/24"))
	b := start(t, &w, "b0", "devbox")
	a.manager.Configure(t.Context(), &config.Net{})
	b.manager.Configure(t.Context(), &config.Net{})
	waitFor(t, "a is up", up(a, 1))
	waitFor(t, "b is up", up(b, 1))
	waitFor(t, "they agree", func() bool { return a.manager.Status().Subnet == b.manager.Status().Subnet })
	sa, sb := a.manager.Status(), b.manager.Status()
	if sa.Address == sb.Address {
		t.Fatalf("both have address %s", sa.Address)
	}
	wantPacket(t, a, b, "ping")
	wantPacket(t, b, a, "pong")

	waitFor(t, "a knows the handshake", func() bool {
		p := a.manager.Status().Peers[0]
		return !p.Handshake.IsZero() && p.Sent > 0 && p.Received > 0
	})
	if p := a.manager.Status().Peers[0]; p.Name != "devbox" || p.Address != sb.Address || p.Path != wire.PathSSH {
		t.Fatalf("peer: %+v", p)
	}
	spec := a.device().current()
	if spec.Hosts["laptop.gst"] != sa.Address || spec.Hosts["devbox.gst"] != sb.Address {
		t.Fatalf("hosts: %v", spec.Hosts)
	}

	// The private key stays in a file only its owner reads.
	info, err := os.Stat(filepath.Join(a.dir, "net.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state: %v, %v", info, err)
	}

}

func TestChoosingASubnetAsksTheOthers(t *testing.T) {
	// Whatever the first choice would be, it is taken on the other machine.
	var w world
	first, _ := chooseSubnet("a0"+strings.Repeat("0", 30), nil)
	w.add("b0", "devbox").networks = []string{first.String()}
	a := start(t, &w, "a0", "laptop")
	waitFor(t, "a is up", up(a, 0))
	if got := a.manager.Status().Subnet; got == first.String() {
		t.Fatalf("chose %s, which the other machine uses", got)
	}
}

func TestLatestSubnetWins(t *testing.T) {
	var w world
	a := start(t, &w, "a0", "laptop")
	b := start(t, &w, "b0", "devbox")
	a.manager.Configure(t.Context(), &config.Net{Subnet: "10.50.0.0/16", Since: 1})
	b.manager.Configure(t.Context(), &config.Net{})
	waitFor(t, "b follows", func() bool {
		st := b.manager.Status()
		return st != nil && st.Subnet == "10.50.0.0/16" && st.Interface != ""
	})
	b.manager.Configure(t.Context(), &config.Net{Subnet: "10.60.0.0/16", Since: 2})
	waitFor(t, "a follows", func() bool {
		st := a.manager.Status()
		return st.Subnet == "10.60.0.0/16" && strings.HasPrefix(st.Address, "10.60.") && len(st.Peers) == 1
	})
	waitFor(t, "b sees a", up(b, 1))
	wantPacket(t, a, b, "ping")
	if spec := a.device().current(); spec.Subnet != "10.60.0.0/16" {
		t.Fatalf("spec: %+v", spec)
	}

	// An older choice in the configuration changes nothing.
	a.manager.Configure(t.Context(), &config.Net{Subnet: "10.50.0.0/16", Since: 1})
	if got := a.manager.Status().Subnet; got != "10.60.0.0/16" {
		t.Fatalf("subnet = %s", got)
	}
}

// A machine that picked a subnet on its own, before it knew of the others,
// must not move the network it then joins.
func TestAutomaticChoiceYields(t *testing.T) {
	var w world
	a := start(t, &w, "a0", "laptop")
	waitFor(t, "a chose", up(a, 0))
	chosen := a.manager.Status().Subnet

	lone := &testNode{mesh: w.add("b0", "devbox"), dir: t.TempDir()}
	lone.mesh.setAlone(true)
	lone.run(t)
	waitFor(t, "b chose", up(lone, 0))
	lone.mesh.setAlone(false)
	waitFor(t, "b joins", func() bool {
		st := lone.manager.Status()
		return st.Subnet == chosen && len(st.Peers) == 1
	})
	if got := a.manager.Status().Subnet; got != chosen {
		t.Fatalf("a moved from %s to %s", chosen, got)
	}

	// Any choice of a person beats them.
	lone.manager.Configure(t.Context(), &config.Net{Subnet: "10.55.0.0/16", Since: 1})
	waitFor(t, "a follows", func() bool { return a.manager.Status().Subnet == "10.55.0.0/16" })
}

func TestAddressConflict(t *testing.T) {
	// Two machines that settled on one address, as after a merge of two
	// networks: the higher id moves.
	var w world
	a := start(t, &w, "a0", "laptop")
	subnet := &config.Net{Subnet: "10.70.0.0/16", Since: 1}
	a.manager.Configure(t.Context(), subnet)
	waitFor(t, "a is up", up(a, 0))
	address := a.manager.Status().Address

	b := &testNode{mesh: w.add("b0", "devbox"), dir: t.TempDir()}
	state := `{"private_key":"` + strings.Repeat("11", 32) + `","subnet":"10.70.0.0/16","since":1,"address":"` + address + `"}`
	if err := os.WriteFile(filepath.Join(b.dir, "net.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	b.run(t)
	b.manager.Configure(t.Context(), subnet)
	waitFor(t, "they differ", func() bool {
		st := b.manager.Status()
		return st != nil && st.Address != "" && st.Address != address && len(st.Peers) == 1
	})
	if got := a.manager.Status().Address; got != address {
		t.Fatalf("the lower id moved from %s to %s", address, got)
	}
}

func TestStateSurvivesARestart(t *testing.T) {
	var w world
	a := start(t, &w, "a0", "laptop")
	a.manager.Configure(t.Context(), &config.Net{})
	waitFor(t, "a is up", up(a, 0))
	before := a.manager.Status()
	key := published(&w, a.mesh).PublicKey

	again := &testNode{mesh: a.mesh, dir: a.dir}
	again.run(t)
	again.manager.Configure(t.Context(), &config.Net{})
	waitFor(t, "a is up again", up(again, 0))
	after := again.manager.Status()
	if after.Subnet != before.Subnet || after.Address != before.Address || published(&w, a.mesh).PublicKey != key {
		t.Fatalf("%+v became %+v", before, after)
	}
}

// A new identity is a new machine: records of the old one stay around for
// a while, so it must not share their key or address. The subnet stays, as
// it belongs to the network.
func TestANewIdentityGetsItsOwnKeyAndAddress(t *testing.T) {
	var w world
	a := start(t, &w, "a0", "laptop")
	b := start(t, &w, "b0", "devbox")
	a.manager.Configure(t.Context(), &config.Net{})
	b.manager.Configure(t.Context(), &config.Net{})
	waitFor(t, "a is up", up(a, 1))
	waitFor(t, "b is up", up(b, 1))
	wantPacket(t, a, b, "before")
	before := a.manager.Status()
	key := published(&w, a.mesh).PublicKey

	w.mu.Lock()
	a.mesh.info.ID = "f" + strings.Repeat("0", 31)
	w.mu.Unlock()
	a.manager.Configure(t.Context(), &config.Net{})
	waitFor(t, "a has a new key", func() bool { return published(&w, a.mesh).PublicKey != key })
	if after := a.manager.Status(); after.Subnet != before.Subnet {
		t.Fatalf("%+v became %+v", before, after)
	}
	data, err := os.ReadFile(filepath.Join(a.dir, "net.json"))
	if err != nil || !strings.Contains(string(data), a.mesh.Self().ID) {
		t.Fatalf("state: %s, %v", data, err)
	}

	// The two machines find each other again by the new key.
	w.changed()
	waitFor(t, "a is up", up(a, 1))
	waitFor(t, "b knows the new key", up(b, 1))
	wantPacket(t, a, b, "after")
	wantPacket(t, b, a, "back")
}

// A failure to save the new identity's key leaves everything as it was,
// and the next look tries again, so that the key in the record is always
// the one that WireGuard has.
func TestANewIdentityThatCannotBeSaved(t *testing.T) {
	var w world
	a := start(t, &w, "a0", "laptop")
	b := start(t, &w, "b0", "devbox")
	a.manager.Configure(t.Context(), &config.Net{})
	b.manager.Configure(t.Context(), &config.Net{})
	waitFor(t, "a is up", up(a, 1))
	waitFor(t, "b is up", up(b, 1))
	key := published(&w, a.mesh).PublicKey

	if err := os.Chmod(a.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(a.dir, 0o700) })
	w.mu.Lock()
	a.mesh.info.ID = "f" + strings.Repeat("0", 31)
	w.mu.Unlock()
	a.manager.Configure(t.Context(), &config.Net{})
	if st := a.manager.Status(); st.Error == "" {
		t.Fatalf("no error: %+v", st)
	}
	if got := published(&w, a.mesh).PublicKey; got != key {
		t.Fatal("the record has a key that was not saved")
	}

	if err := os.Chmod(a.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	a.manager.Configure(t.Context(), &config.Net{})
	waitFor(t, "a has a new key", func() bool { return published(&w, a.mesh).PublicKey != key })
	w.changed()
	waitFor(t, "a is up", up(a, 1))
	waitFor(t, "b knows the new key", up(b, 1))
	wantPacket(t, a, b, "after")
	wantPacket(t, b, a, "back")
}

// The node that a packet seems to come from may be wrong: two machines
// behind one NAT can have the same source. The answers still go to the
// machine that WireGuard knows, not to that node.
func TestPacketsFromTheWrongNode(t *testing.T) {
	var w world
	a := start(t, &w, "a0", "laptop")
	b := start(t, &w, "b0", "devbox")
	c := start(t, &w, "c0", "homelab")
	for _, n := range []*testNode{a, b, c} {
		n.manager.Configure(t.Context(), &config.Net{})
	}
	for _, n := range []*testNode{a, b, c} {
		waitFor(t, "the machine is up", up(n, 2))
	}
	w.mu.Lock()
	a.mesh.sendAs = c.mesh.info.ID
	w.mu.Unlock()
	wantPacket(t, a, b, "ping")
	wantPacket(t, b, a, "pong")
}

func published(w *world, m *fakeMesh) *wire.NetInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	return m.net
}

func TestProblemsAreReported(t *testing.T) {
	var w world
	mesh := w.add("a0", "laptop")
	dir := t.TempDir()
	var helper atomic.Bool
	m := New(Config{
		Mesh: mesh, Log: log.New(io.Discard, "", 0), StatePath: filepath.Join(dir, "net.json"),
		Open: func(spec tun.Spec) (Interface, error) {
			if !helper.Load() {
				return nil, tun.ErrNoHelper
			}
			return newFakeInterface(spec), nil
		},
		Networks: func(string) []netip.Prefix { return []netip.Prefix{prefix("10.80.0.0/24")} },
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitFor(t, "the manager runs", m.running.Load)
	m.Configure(t.Context(), &config.Net{})
	st := m.Status()
	if !strings.Contains(st.Hint, "gst up") || st.Interface != "" {
		t.Fatalf("without the helper: %+v", st)
	}
	// The other machines must not send to a machine that cannot answer.
	if published(&w, mesh) != nil {
		t.Fatal("published the machine without an interface")
	}

	helper.Store(true)
	m.Configure(t.Context(), &config.Net{Subnet: "10.80.0.0/16", Since: time.Now().UnixNano()})
	st = m.Status()
	if published(&w, mesh) == nil {
		t.Fatal("published nothing with an interface")
	}
	if !strings.Contains(st.Error, "overlaps 10.80.0.0/24") || !strings.Contains(st.Hint, "gst subnet") {
		t.Fatalf("with an overlap: %+v", st)
	}
}

func TestBind(t *testing.T) {
	var sent []string
	b := newBind(func(node string, packet []byte) { sent = append(sent, node+" "+string(packet)) }, &atomic.Uint64{})
	receive, _, err := b.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := b.ParseEndpoint("node")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Send([][]byte{[]byte("one"), []byte("two")}, ep); err != nil {
		t.Fatal(err)
	}
	if strings.Join(sent, ",") != "node one,node two" {
		t.Fatalf("sent %v", sent)
	}

	b.deliver("peer", []byte("hello"))
	packets, sizes, eps := [][]byte{make([]byte, 100)}, make([]int, 1), make([]conn.Endpoint, 1)
	n, err := receive[0](packets, sizes, eps)
	if err != nil || n != 1 || string(packets[0][:sizes[0]]) != "hello" || eps[0].DstToString() != "peer" {
		t.Fatalf("received %d, %q from %v: %v", n, packets[0][:sizes[0]], eps[0], err)
	}

	// A closed bind ends its receive function and can open again.
	b.Close()
	if _, err := receive[0](packets, sizes, eps); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("receive on a closed bind: %v", err)
	}
	if _, _, err := b.Open(0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Open(0); !errors.Is(err, conn.ErrBindAlreadyOpen) {
		t.Fatalf("second open: %v", err)
	}
}
