package direct

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

var loopback = netip.MustParseAddr("127.0.0.1")

func never(netip.Addr) bool { return false }

func transport(t *testing.T) *Transport {
	t.Helper()
	tr, err := Listen(0, func() []netip.Addr { return []netip.Addr{loopback} }, never)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tr.Close)
	return tr
}

func within(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)
	return ctx
}

// connect runs a try between two transports. change, when not nil, sees
// what each side gets from the other.
func connect(ctx context.Context, t *testing.T, client, server *Transport, change func(offer, answer *wire.DirectOffer)) (*Path, *Path, error) {
	t.Helper()
	c, err := client.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	offer := c.Offer()
	s, err := server.Join(offer, nil)
	if err != nil {
		t.Fatal(err)
	}
	answer := s.Offer()
	if change != nil {
		change(&offer, &answer)
	}
	t.Cleanup(func() { c.Close(); s.Close() })
	failed := make(chan error, 1)
	go func() { failed <- s.Connect(ctx, offer) }()
	if err := c.Connect(ctx, answer); err != nil {
		return nil, nil, err
	}
	if err := <-failed; err != nil {
		return nil, nil, err
	}
	return c, s, nil
}

func mustConnect(t *testing.T, client, server *Transport) (*Path, *Path) {
	t.Helper()
	c, s, err := connect(within(t, 10*time.Second), t, client, server, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

// exchange checks that packets go both ways.
func exchange(t *testing.T, a, b *Path) {
	t.Helper()
	for _, ends := range [][2]*Path{{a, b}, {b, a}} {
		got := make(chan string, 16)
		ends[1].HandlePackets(func(p []byte) { got <- string(p) })
		if err := ends[0].WritePacket([]byte("\x04ping")); err != nil {
			t.Fatal(err)
		}
		select {
		case p := <-got:
			if p != "\x04ping" {
				t.Fatalf("got %q", p)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the packet did not arrive")
		}
	}
}

func TestConnect(t *testing.T) {
	start := time.Now()
	c, s := mustConnect(t, transport(t), transport(t))
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("connecting took %v, as if it waited for the context", took)
	}
	exchange(t, c, s)
	if !strings.HasPrefix(c.Local(), "*:") || c.Remote() == "" {
		t.Fatalf("local %q, remote %q", c.Local(), c.Remote())
	}
}

// Paths to several machines share one socket, and packets reach the path
// of the machine they came from.
func TestPathsShareAPort(t *testing.T) {
	hub, b, c := transport(t), transport(t), transport(t)
	toB, atB := mustConnect(t, hub, b)
	toC, atC := mustConnect(t, hub, c)
	if toB.Local() != toC.Local() {
		t.Fatalf("ports %s and %s", toB.Local(), toC.Local())
	}
	var fromB, fromC atomic.Int32
	toB.HandlePackets(func([]byte) { fromB.Add(1) })
	toC.HandlePackets(func([]byte) { fromC.Add(1) })
	atB.WritePacket([]byte("\x04b"))
	atC.WritePacket([]byte("\x04c"))
	atC.WritePacket([]byte("\x04c"))
	waitFor(t, "the packets arrive", func() bool { return fromB.Load() == 1 && fromC.Load() == 2 })
	exchange(t, toB, atB)
	exchange(t, toC, atC)

	// One path ending leaves the other.
	toB.Close()
	waitFor(t, "the other side ends", atB.ended)
	exchange(t, toC, atC)
}

// Bursts of packets arrive, as the frames of a video do.
func TestPackets(t *testing.T) {
	c, s := mustConnect(t, transport(t), transport(t))
	const burst = 500
	got := make(chan string, burst)
	s.HandlePackets(func(p []byte) { got <- string(p) })
	for i := range burst {
		if err := c.WritePacket(fmt.Appendf(nil, "\x04%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for len(seen) < burst*9/10 {
		select {
		case p := <-got:
			seen[p] = true
		case <-deadline:
			t.Fatalf("%d of %d packets arrived", len(seen), burst)
		}
	}
}

// What is not WireGuard's stays out, and so does what comes from an
// address that did not prove itself.
func TestStrayPackets(t *testing.T) {
	client, server := transport(t), transport(t)
	c, s := mustConnect(t, client, server)
	got := make(chan string, 16)
	s.HandlePackets(func(p []byte) { got <- string(p) })
	stranger, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(loopback, 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer stranger.Close()
	to := netip.AddrPortFrom(loopback, server.v4.port)
	stranger.WriteToUDPAddrPort([]byte("\x04stranger"), to)
	c.WritePacket([]byte("\x7fnot wireguard"))
	c.WritePacket([]byte("\x04mine"))
	select {
	case p := <-got:
		if p != "\x04mine" {
			t.Fatalf("got %q", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the packet did not arrive")
	}
}

// A side that cannot be reached, as behind NAT, offers addresses that
// lead nowhere. The other side finds it from the probes that it sends.
func TestPathFromProbes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hidden func(offer, answer *wire.DirectOffer)
	}{
		{"client hidden", func(offer, _ *wire.DirectOffer) { offer.Addresses = []string{"127.0.0.1:9"} }},
		{"server hidden", func(_, answer *wire.DirectOffer) { answer.Addresses = []string{"127.0.0.1:9"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, s, err := connect(within(t, 10*time.Second), t, transport(t), transport(t), tc.hidden)
			if err != nil {
				t.Fatal(err)
			}
			exchange(t, c, s)
		})
	}
}

// Probes with another token prove nothing: the other side stays silent.
func TestWrongToken(t *testing.T) {
	client, server := transport(t), transport(t)
	c, err := client.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := server.Join(wire.DirectOffer{Token: hex.EncodeToString(make([]byte, tokenLen))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := c.Connect(within(t, time.Second), s.Offer()); err == nil {
		t.Fatal("connected with a wrong token")
	}
}

func TestInvalidOffers(t *testing.T) {
	tr := transport(t)
	if _, err := tr.Join(wire.DirectOffer{Token: "xx"}, nil); err == nil {
		t.Error("joined with an invalid token")
	}
	p, err := tr.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for name, offer := range map[string]wire.DirectOffer{
		"address":   {Addresses: []string{"nonsense"}},
		"addresses": {Addresses: make([]string, maxAddresses+1)},
	} {
		if err := p.Connect(t.Context(), offer); err == nil {
			t.Errorf("connected with an invalid %s", name)
		}
	}
}

// A machine may reach an address of its own, when another machine has the
// same one, as on the bridge of a container runtime, and the same port. A
// try must not take itself for the other side.
func TestProbesThatReachThisMachine(t *testing.T) {
	tr := transport(t)
	p, err := tr.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	own := netip.AddrPortFrom(loopback, tr.v4.port).String()
	if err := p.Connect(within(t, time.Second), wire.DirectOffer{Addresses: []string{own}}); err == nil {
		t.Fatal("connected to itself")
	}
}

// Addresses of the virtual network are no way to a machine, whether this
// machine has them, the other side offers them, or packets come from them.
func TestSkippedAddresses(t *testing.T) {
	gst := netip.MustParseAddr("10.99.0.1")
	skip := func(a netip.Addr) bool { return a == gst || a == loopback }
	tr, err := Listen(0, func() []netip.Addr { return []netip.Addr{gst, netip.MustParseAddr("192.0.2.1")} }, skip)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	p, err := tr.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if got := p.Offer().Addresses; len(got) != 1 || !strings.HasPrefix(got[0], "192.0.2.1:") {
		t.Fatalf("offer %v", got)
	}
	list, err := tr.addresses(wire.DirectOffer{Addresses: []string{"10.99.0.1:5000", "192.0.2.2:5000"}})
	if err != nil || len(list) != 1 || list[0].String() != "192.0.2.2:5000" {
		t.Fatalf("addresses %v, %v", list, err)
	}
	// A probe from such an address gets no answer.
	stranger, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(loopback, 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer stranger.Close()
	probe := make([]byte, probeLen)
	probe[0] = kindProbe
	copy(probe[1:], p.token[:])
	stranger.WriteToUDPAddrPort(probe, netip.AddrPortFrom(loopback, tr.v4.port))
	stranger.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, _, err := stranger.ReadFromUDPAddrPort(make([]byte, 128)); err == nil {
		t.Fatalf("got %d bytes", n)
	}
}

// What another machine sees of this one goes into the offers to others.
func TestPublicAddress(t *testing.T) {
	tr := transport(t)
	public := netip.MustParseAddrPort("203.0.113.7:41000")
	tr.sawPublic(public)
	tr.sawPublic(netip.MustParseAddrPort("[::]:41000"))
	if got := tr.offerAddresses(); !slices.Contains(got, public.String()) || len(got) != 2 {
		t.Fatalf("offer %v", got)
	}
	for i := range 2 * maxPublic {
		tr.sawPublic(netip.AddrPortFrom(netip.MustParseAddr("203.0.113.8"), uint16(1000+i)))
	}
	if got := len(tr.offerAddresses()); got != 1+maxPublic {
		t.Fatalf("%d addresses", got)
	}
}

// An answer tells the machine that probed where it came from.
func TestAnswersTellTheSource(t *testing.T) {
	client, server := transport(t), transport(t)
	c, _ := mustConnect(t, client, server)
	packet := answer(c.token, make([]byte, 8+addrLen), netip.MustParseAddrPort("203.0.113.7:41000"))
	if seen, ok := parseAddr(packet[len(packet)-addrLen:]); !ok || seen.String() != "203.0.113.7:41000" {
		t.Fatalf("seen %v", seen)
	}
}

func newTestPath(t *testing.T, addrs ...netip.AddrPort) *Path {
	t.Helper()
	p, err := transport(t).Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	for _, a := range addrs {
		p.add(a)
	}
	return p
}

// A faster address takes over once it is measured to be clearly faster,
// and only then: paths must not hop between equals.
func TestChoosingTheFastestAddress(t *testing.T) {
	slow, fast, near := netip.MustParseAddrPort("192.0.2.1:1"), netip.MustParseAddrPort("192.0.2.2:2"), netip.MustParseAddrPort("192.0.2.3:3")
	p := newTestPath(t, slow, fast, near)
	now := time.Now()
	p.record(slow, 50*time.Millisecond, now)
	if p.Remote() != slow.String() {
		t.Fatalf("using %s", p.Remote())
	}
	for range 10 {
		p.record(near, 49*time.Millisecond, now)
		p.record(slow, 50*time.Millisecond, now)
	}
	if p.Remote() != slow.String() {
		t.Fatalf("hopped to %s", p.Remote())
	}
	p.record(fast, 5*time.Millisecond, now)
	p.record(fast, 5*time.Millisecond, now)
	if p.Remote() != slow.String() {
		t.Fatal("moved on too few measures")
	}
	p.record(fast, 5*time.Millisecond, now)
	if p.Remote() != fast.String() {
		t.Fatalf("still using %s", p.Remote())
	}
}

// A private address wins when it is about as fast as a public one, but not
// when it is clearly slower; and a public address must be clearly faster
// still to take its place.
func TestPreferringPrivateAddresses(t *testing.T) {
	public, private := netip.MustParseAddrPort("192.0.2.1:1"), netip.MustParseAddrPort("10.0.0.2:2")
	measure := func(p *Path, addr netip.AddrPort, rtt time.Duration) {
		for range minSamples {
			p.record(addr, rtt, time.Now())
		}
	}
	for _, c := range []struct {
		name            string
		first, then     netip.AddrPort
		firstRTT, other time.Duration
		want            netip.AddrPort
	}{
		{"as fast on a LAN", public, private, time.Millisecond, time.Millisecond, private},
		{"a little slower", public, private, 40 * time.Millisecond, 43 * time.Millisecond, private},
		{"clearly slower", public, private, 40 * time.Millisecond, 50 * time.Millisecond, public},
		{"public a little faster", private, public, 43 * time.Millisecond, 40 * time.Millisecond, private},
		{"public faster by the margin", private, public, 50 * time.Millisecond, 40 * time.Millisecond, private},
		{"public much faster", private, public, 50 * time.Millisecond, 30 * time.Millisecond, public},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newTestPath(t, public, private)
			measure(p, c.first, c.firstRTT)
			measure(p, c.then, c.other)
			if got := *p.best.Load(); got != c.want {
				t.Fatalf("using %s", got)
			}
		})
	}
}

// An address in use that stopped answering gives way to any that answers.
func TestStaleAddress(t *testing.T) {
	first, other := netip.MustParseAddrPort("192.0.2.1:1"), netip.MustParseAddrPort("192.0.2.2:2")
	p := newTestPath(t, first, other)
	now := time.Now()
	p.record(first, time.Millisecond, now)
	p.record(other, 90*time.Millisecond, now.Add(staleLimit+time.Second))
	if p.Remote() != other.String() {
		t.Fatalf("still using %s", p.Remote())
	}
}

// Probes go to every address during a sweep and to the one in use between
// sweeps.
func TestSweeps(t *testing.T) {
	first, other := netip.MustParseAddrPort("192.0.2.1:1"), netip.MustParseAddrPort("192.0.2.2:2")
	p := newTestPath(t, first, other)
	now := time.Now()
	if got := p.due(now); len(got) != 2 {
		t.Fatalf("before the path: %v", got)
	}
	p.record(first, time.Millisecond, now)
	if got := p.due(now); len(got) != 2 {
		t.Fatalf("right after it came up: %v", got)
	}
	later := now.Add(sweepTime)
	p.record(first, time.Millisecond, later)
	if got := p.due(later); len(got) != 1 || got[0] != first {
		t.Fatalf("between sweeps: %v", got)
	}
	if got := p.due(later.Add(probeInterval)); len(got) != 0 {
		t.Fatalf("before the next beat: %v", got)
	}
	next := now.Add(sweepInterval)
	p.record(first, time.Millisecond, next)
	if got := p.due(next); len(got) != 2 {
		t.Fatalf("at the next sweep: %v", got)
	}
}

// A side that moved to another address keeps its path: its probes prove
// the new address.
func TestMovingToAnotherAddress(t *testing.T) {
	client, server := transport(t), transport(t)
	c, s := mustConnect(t, client, server)
	got := make(chan string, 16)
	s.HandlePackets(func(p []byte) { got <- string(p) })
	moved, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(loopback, 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer moved.Close()
	to := netip.AddrPortFrom(loopback, server.v4.port)
	moved.WriteToUDPAddrPort([]byte("\x04early"), to)
	probe := make([]byte, 0, probeLen)
	probe = append(probe, kindProbe)
	probe = append(probe, c.token[:]...)
	probe = append(probe, make([]byte, idLen+8)...)
	probe = appendAddr(probe, to)
	moved.WriteToUDPAddrPort(probe, to)
	buf := make([]byte, 128)
	moved.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, _, err := moved.ReadFromUDPAddrPort(buf); err != nil || n != answerLen {
		t.Fatalf("answer: %d bytes, %v", n, err)
	}
	moved.WriteToUDPAddrPort([]byte("\x04moved"), to)
	select {
	case p := <-got:
		if p != "\x04moved" {
			t.Fatalf("got %q", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the packet did not arrive")
	}
}

// Learned addresses have a limit, and the one in use survives it.
func TestTargetLimit(t *testing.T) {
	first := netip.MustParseAddrPort("192.0.2.1:1")
	p := newTestPath(t, first)
	p.record(first, time.Millisecond, time.Now())
	for i := range 3 * maxTargets {
		p.probed(netip.AddrPortFrom(netip.MustParseAddr("192.0.2.2"), uint16(1000+i)))
	}
	if len(p.targets) > maxTargets || p.targets[first] == nil {
		t.Fatalf("%d targets, first %v", len(p.targets), p.targets[first])
	}
}

// Closing a path tells the other side at once.
func TestClose(t *testing.T) {
	c, s := mustConnect(t, transport(t), transport(t))
	c.Close()
	select {
	case <-s.Done():
	case <-time.After(silenceLimit / 2):
		t.Fatal("the other side did not notice")
	}
	if c.FellSilent() || s.FellSilent() {
		t.Fatal("a closed path counts as fallen silent")
	}
}

// A path whose other end stopped answering ends by itself.
func TestSilence(t *testing.T) {
	c, s := mustConnect(t, transport(t), transport(t))
	s.end() // Without a word to the other side.
	select {
	case <-c.Done():
	case <-time.After(2 * silenceLimit):
		t.Fatal("the path did not end")
	}
	if !c.FellSilent() {
		t.Fatal("the path does not tell why it ended")
	}
}

// A path is idle while this side sends nothing on it: probes and what
// arrives do not count.
func TestIdle(t *testing.T) {
	c, s := mustConnect(t, transport(t), transport(t))
	time.Sleep(probeInterval)
	if err := c.WritePacket(wg()); err != nil {
		t.Fatal(err)
	}
	if c.Idle() >= probeInterval || s.Idle() < probeInterval {
		t.Fatalf("idle for %s and %s", c.Idle(), s.Idle())
	}
}

// A path keeps running while the two ends hear each other.
func TestBeats(t *testing.T) {
	c, s := mustConnect(t, transport(t), transport(t))
	select {
	case <-c.Done():
		t.Fatal("the path ended")
	case <-s.Done():
		t.Fatal("the path ended")
	case <-time.After(silenceLimit + beatInterval):
	}
	exchange(t, c, s)
}

// Closing the transport ends its paths, and no more can start.
func TestCloseTransport(t *testing.T) {
	client, server := transport(t), transport(t)
	c, s := mustConnect(t, client, server)
	client.Close()
	waitFor(t, "the paths end", func() bool { return c.ended() && s.ended() })
	if _, err := client.Start(nil); err == nil {
		t.Fatal("started a path on a closed transport")
	}
}

// A taken port does not keep a second daemon from running.
func TestPortTaken(t *testing.T) {
	first := transport(t)
	second, err := Listen(int(first.v4.port), first.addrs, never)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.v4.port == first.v4.port {
		t.Fatal("two sockets on one port")
	}
}

func TestAddresses(t *testing.T) {
	for _, addr := range Addresses() {
		if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsMulticast() || !addr.IsValid() {
			t.Errorf("unusable address %s", addr)
		}
	}
}

func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out: " + msg)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Answers from ever new sources stay within the limit on addresses, and
// answers to a probe of no address of the path count for nothing.
func TestAnswerSourcesAreBounded(t *testing.T) {
	first := netip.MustParseAddrPort("192.0.2.1:1")
	p := newTestPath(t, first)
	p.record(first, time.Millisecond, time.Now())
	for i := range 3 * maxTargets {
		p.answered(first, netip.AddrPortFrom(netip.MustParseAddr("192.0.2.2"), uint16(1000+i)), time.Millisecond)
	}
	waitFor(t, "the old sources go", func() bool {
		count := 0
		for _, q := range *p.t.sources.Load() {
			if q == p {
				count++
			}
		}
		return count <= maxTargets
	})
	if len(p.targets) > maxTargets {
		t.Fatalf("%d targets", len(p.targets))
	}

	stranger := netip.MustParseAddrPort("192.0.2.3:3")
	p.answered(netip.MustParseAddrPort("192.0.2.4:4"), stranger, time.Millisecond)
	if p.knows(stranger) || (*p.t.sources.Load())[stranger] != nil {
		t.Fatal("an answer to no probe of this path counted")
	}
}

// What an address measured long ago says nothing of it now: it must prove
// itself again before it takes over.
func TestOldMeasuresDoNotCount(t *testing.T) {
	fast, slow := netip.MustParseAddrPort("192.0.2.1:1"), netip.MustParseAddrPort("192.0.2.2:2")
	p := newTestPath(t, fast, slow)
	now := time.Now()
	p.record(slow, 50*time.Millisecond, now)
	for range minSamples {
		p.record(fast, time.Millisecond, now)
	}
	if got := *p.best.Load(); got != fast {
		t.Fatalf("best = %s", got)
	}
	p.best.Store(&slow)

	later := now.Add(measureLife + time.Second)
	p.record(slow, 50*time.Millisecond, later)
	p.record(fast, 60*time.Millisecond, later)
	if got := *p.best.Load(); got != slow {
		t.Fatalf("moved to %s on an old measure", got)
	}
	if got := p.targets[fast]; got.samples != 1 || got.rtt != 60*time.Millisecond {
		t.Fatalf("target: %+v", got)
	}
}

func TestRefresh(t *testing.T) {
	addr, fresh := netip.MustParseAddrPort("192.0.2.1:1"), netip.MustParseAddrPort("192.0.2.2:2")
	p := newTestPath(t, addr)
	p.record(addr, time.Millisecond, time.Now())

	if err := p.Refresh(wire.DirectOffer{Token: "00", Addresses: []string{fresh.String()}}); err == nil {
		t.Fatal("took the update of another path")
	}
	if err := p.Refresh(wire.DirectOffer{Token: p.Offer().Token, Addresses: []string{fresh.String()}}); err != nil {
		t.Fatal(err)
	}
	if !p.knows(fresh) || p.targets[addr].samples != 0 {
		t.Fatalf("targets: %v", p.targets)
	}
	if got := p.due(time.Now()); len(got) != 2 {
		t.Fatalf("probing %v", got)
	}
}

// A change of addresses is told once, and what others saw is gone with it.
func TestAddressesChange(t *testing.T) {
	addrs := []netip.Addr{loopback}
	tr, err := Listen(0, func() []netip.Addr { return slices.Clone(addrs) }, never)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tr.Close)
	changed := func() bool {
		select {
		case <-tr.Changed():
			return true
		default:
			return false
		}
	}
	tr.look()
	if changed() {
		t.Fatal("nothing changed")
	}
	public := netip.MustParseAddrPort("203.0.113.7:4000")
	tr.sawPublic(public)
	addrs = []netip.Addr{netip.MustParseAddr("192.0.2.1"), loopback}
	tr.look()
	if !changed() {
		t.Fatal("no word of new addresses")
	}
	if slices.Contains(tr.offerAddresses(), public.String()) {
		t.Fatal("kept what others saw on the old network")
	}
	// A new default route: the same addresses, but packets leave from
	// another.
	addrs = []netip.Addr{loopback, netip.MustParseAddr("192.0.2.1")}
	tr.look()
	if !changed() {
		t.Fatal("no word of another address to leave from")
	}
	tr.look()
	if changed() {
		t.Fatal("nothing changed since")
	}
}

func TestStats(t *testing.T) {
	tr := transport(t)
	tr.receive(tr.v4, wg(), netip.MustParseAddrPort("192.0.2.1:1"))
	s := tr.Stats()
	if s.Port == 0 || s.ReadBuffer <= 0 || s.WriteBuffer <= 0 || s.Strays != 1 {
		t.Fatalf("stats: %+v", s)
	}
}

func wg() []byte { return []byte{4, 0, 0, 0} }
