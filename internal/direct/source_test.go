package direct

import (
	"net"
	"net/netip"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// localAddresses returns the loopback addresses of this machine that a
// socket can send from: all of 127.0.0.0/8 on Linux, of which three serve,
// and 127.0.0.1 alone on macOS. macOS also loses a packet that goes from
// one address of an interface to one of another now and then, so the
// addresses of the interfaces are of no use here.
func localAddresses() []netip.Addr {
	var list []netip.Addr
	for _, a := range []netip.Addr{loopback, netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.3")} {
		if c, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(a, 0))); err == nil {
			c.Close()
			list = append(list, a)
		}
	}
	return list
}

// several runs test with several addresses of this machine.
func several(t *testing.T, test func(t *testing.T, network string, locals []netip.Addr)) {
	locals := localAddresses()
	if len(locals) < 2 {
		t.Skipf("one address: %v", locals)
	}
	test(t, "udp4", locals)
}

// peer is a socket of the other side.
func peer(t *testing.T, network string, at netip.Addr) (*net.UDPConn, netip.AddrPort) {
	t.Helper()
	c, err := net.ListenUDP(network, net.UDPAddrFromAddrPort(netip.AddrPortFrom(at, 0)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, netip.AddrPortFrom(at, c.LocalAddr().(*net.UDPAddr).AddrPort().Port())
}

// sourceOf reads a packet and returns where it came from.
func sourceOf(t *testing.T, c *net.UDPConn) (netip.Addr, []byte) {
	t.Helper()
	buf := make([]byte, 2048)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, from, err := c.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatal(err)
	}
	return from.Addr().Unmap(), buf[:n]
}

// probeOf makes a probe of the other side of p, to the address to; works
// says that the pair works.
func probeOf(p *Path, to netip.AddrPort, works bool) []byte {
	packet := make([]byte, 0, probeLen)
	packet = append(packet, kindProbe)
	packet = append(packet, p.token[:]...)
	packet = append(packet, make([]byte, idLen+8)...)
	packet = appendAddr(packet, to)
	if works {
		return append(packet, flagWorks)
	}
	return append(packet, 0)
}

// An answer leaves from the address that its probe came to, whatever
// address the system would send from: what lies between knows that one.
func TestAnswersLeaveFromTheAddressProbed(t *testing.T) {
	several(t, func(t *testing.T, network string, locals []netip.Addr) {
		tr := transport(t)
		p, err := tr.Start(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		for i, local := range locals {
			// From another address, which the system would answer from.
			other, _ := peer(t, network, locals[(i+1)%len(locals)])
			to := netip.AddrPortFrom(local, tr.socketFor(local).port)
			other.WriteToUDPAddrPort(probeOf(p, to, false), to)
			if from, packet := sourceOf(t, other); from != local || packet[0] != kindAnswer {
				t.Errorf("probe to %s: answer from %s", local, from)
			}
		}
	})
}

// Probes to an address that answers, and the packets to it, leave from the
// address its answers came to; two paths on the shared socket may use two
// at once.
func TestPacketsLeaveFromWhereAnswersCame(t *testing.T) {
	several(t, func(t *testing.T, network string, locals []netip.Addr) {
		tr := transport(t)
		paths := make([]*Path, 2)
		peers := make([]*net.UDPConn, 2)
		for i := range paths {
			c, at := peer(t, network, locals[0])
			p, err := tr.Start(nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { p.Close() })
			p.mu.Lock()
			p.add(at)
			p.record(at, locals[len(locals)-1-i], time.Millisecond, true, time.Now())
			p.mu.Unlock()
			paths[i], peers[i] = p, c
		}
		const count = 200
		var wg sync.WaitGroup
		for _, p := range paths {
			wg.Go(func() {
				for range count {
					p.WritePacket(wg4())
				}
			})
		}
		wg.Wait()
		for i, c := range peers {
			want := locals[len(locals)-1-i]
			if got := paths[i].Local(); got != netip.AddrPortFrom(want, tr.socketFor(want).port).String() {
				t.Errorf("path %d: local %s", i, got)
			}
			for range count / 2 {
				if from, _ := sourceOf(t, c); from != want {
					t.Fatalf("path %d: a packet from %s, not %s", i, from, want)
				}
			}
			paths[i].probe(time.Now())
			for {
				from, packet := sourceOf(t, c)
				if packet[0] == kindProbe {
					if from != want {
						t.Fatalf("path %d: a probe from %s, not %s", i, from, want)
					}
					break
				}
			}
		}
	})
}

func wg4() []byte { return []byte{4, 0, 0, 0} }

// An address answered at one address of this machine keeps it while it
// answers: a late answer to a probe that left from another does not move
// it. Once it stopped answering, the next answer decides.
func TestAnswersAtAnotherAddress(t *testing.T) {
	remote := netip.MustParseAddrPort("192.0.2.1:1")
	first, second := netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("198.51.100.2")
	p := newTestPath(t, remote)
	now := time.Now()
	p.record(remote, first, time.Millisecond, true, now)
	p.record(remote, second, time.Millisecond, true, now.Add(time.Millisecond))
	if got := p.best.Load(); got.local != first || p.targets[remote].local != first {
		t.Fatalf("using %s", got)
	}
	if got := p.due(now); len(got) != 1 || got[0].local != first {
		t.Fatalf("probing %v", got)
	}
	later := now.Add(staleLimit + time.Second)
	if got := sources(p, later, remote); !slices.Equal(got, []netip.Addr{{}, loopback}) {
		t.Fatalf("probing from %v after it stopped answering", got)
	}
	p.record(remote, second, time.Millisecond, true, later)
	if got := p.best.Load(); got.local != second {
		t.Fatalf("using %s", got)
	}
}

// When this side changes network, no pair is known to work any more: the
// probes leave as at the start of a try, and the packets keep their address
// until an answer tells another.
func TestRemeasureStartsOver(t *testing.T) {
	remote := netip.MustParseAddrPort("192.0.2.1:1")
	first, second := netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("198.51.100.2")
	p := newTestPath(t, remote)
	now := time.Now()
	p.record(remote, first, time.Millisecond, true, now)
	p.Remeasure()
	if got := p.due(now); len(got) != 2 || got[0].local.IsValid() || got[1].local != loopback || got[0].works || got[1].works {
		t.Fatalf("probing %v", got)
	}
	if got := p.best.Load(); got.local != first {
		t.Fatalf("using %s before an answer", got)
	}
	// A late answer to a probe from before takes the place first, and the
	// next answer then waits until it stops answering.
	p.mu.Lock()
	p.record(remote, first, time.Millisecond, true, now.Add(time.Millisecond))
	p.record(remote, second, time.Millisecond, true, now.Add(2*time.Millisecond))
	p.mu.Unlock()
	if got := p.best.Load(); got.local != first {
		t.Fatalf("using %s", got)
	}
	later := now.Add(staleLimit + time.Second)
	if got := sources(p, later, remote); !slices.Equal(got, []netip.Addr{{}, loopback}) {
		t.Fatalf("probing from %v after it stopped answering", got)
	}
	p.mu.Lock()
	p.record(remote, second, time.Millisecond, true, later)
	p.mu.Unlock()
	if got := p.best.Load(); got.local != second {
		t.Fatalf("using %s", got)
	}
}

// An address of this machine that is gone fails the packets, which never
// leave from another unasked, and the probes, which then leave from where
// the system says; the answer to those decides.
func TestAddressGone(t *testing.T) {
	for _, c := range []struct {
		name, network string
		at, gone      netip.Addr
	}{
		{"IPv4", "udp4", loopback, netip.MustParseAddr("192.0.2.77")},
		{"IPv6", "udp6", netip.IPv6Loopback(), netip.MustParseAddr("2001:db8::77")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c, err := net.ListenUDP(c.network, net.UDPAddrFromAddrPort(netip.AddrPortFrom(c.at, 0))); err != nil {
				t.Skip(err)
			} else {
				c.Close()
			}
			other, at := peer(t, c.network, c.at)
			p := newTestPath(t, at)
			p.mu.Lock()
			p.record(at, c.gone, time.Millisecond, true, time.Now())
			p.mu.Unlock()
			if err := p.WritePacket(wg4()); err == nil || !gone(err) {
				t.Fatalf("sent from an address of no machine: %v", err)
			}
			p.probe(time.Now())
			if from, packet := sourceOf(t, other); packet[0] != kindProbe || from == c.gone {
				t.Fatalf("a probe from %s", from)
			}
			if p.targets[at].local.IsValid() {
				t.Fatal("still probing from the address that is gone")
			}
			p.mu.Lock()
			p.record(at, c.at, time.Millisecond, true, time.Now())
			p.mu.Unlock()
			if got := p.best.Load(); got.local != c.at {
				t.Fatalf("using %s", got)
			}
			if err := p.WritePacket(wg4()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Over IPv6 too, packets leave from where the answers came.
func TestConnectOverIPv6(t *testing.T) {
	if c, err := net.ListenUDP("udp6", net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.IPv6Loopback(), 0))); err != nil {
		t.Skip(err)
	} else {
		c.Close()
	}
	listen := func() *Transport {
		tr, err := Listen(0, Network{Addresses: func() []netip.Addr { return []netip.Addr{netip.IPv6Loopback()} }}, never)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(tr.Close)
		return tr
	}
	c, s := mustConnect(t, listen(), listen())
	exchange(t, c, s)
	if want := netip.AddrPortFrom(netip.IPv6Loopback(), c.t.v6.port).String(); c.Local() != want {
		t.Fatalf("local %q", c.Local())
	}
}

// fakeAnswers answers the probes that c receives, to the port they came
// from but at the address at, as a NAT in front of the prober might, and
// hands on the rest.
func fakeAnswers(c *net.UDPConn, at netip.Addr, rest chan<- netip.AddrPort) {
	buf := make([]byte, 2048)
	for {
		n, from, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		if n == probeLen && buf[0] == kindProbe {
			token := [tokenLen]byte(buf[1 : 1+tokenLen])
			c.WriteToUDPAddrPort(answer(token, buf[1+tokenLen+idLen:n], from), netip.AddrPortFrom(at, from.Port()))
			continue
		}
		select {
		case rest <- from:
		default:
		}
	}
}

// Where answers arrive is read from the packets themselves: two paths whose
// answers come to two addresses send from those, at once.
func TestAnswersPinTheSource(t *testing.T) {
	several(t, func(t *testing.T, network string, locals []netip.Addr) {
		tr := transport(t)
		type side struct {
			p    *Path
			at   netip.Addr
			rest chan netip.AddrPort
		}
		sides := make([]side, 2)
		for i := range sides {
			c, addr := peer(t, network, locals[0])
			at := locals[len(locals)-1-i]
			rest := make(chan netip.AddrPort, 256)
			go fakeAnswers(c, at, rest)
			p, err := tr.Start(nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { p.Close() })
			if err := p.Connect(within(t, 5*time.Second), wire.DirectOffer{Addresses: []string{addr.String()}}); err != nil {
				t.Fatal(err)
			}
			sides[i] = side{p, at, rest}
		}
		const count = 100
		var wg sync.WaitGroup
		for _, s := range sides {
			if got := s.p.Local(); got != netip.AddrPortFrom(s.at, tr.v4.port).String() {
				t.Fatalf("local %s, answers come to %s", got, s.at)
			}
			wg.Go(func() {
				for range count {
					s.p.WritePacket(wg4())
				}
			})
		}
		wg.Wait()
		for i, s := range sides {
			for range count / 2 {
				select {
				case got := <-s.rest:
					if got.Addr() != s.at {
						t.Fatalf("path %d: a packet from %s, not %s", i, got, s.at)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("path %d: the packets did not arrive", i)
				}
			}
		}
	})
}

// sources returns where the probes to remote that are due leave from; an
// address that is not valid stands for where the system says.
func sources(p *Path, now time.Time, remote netip.AddrPort) []netip.Addr {
	var list []netip.Addr
	for _, w := range p.due(now) {
		if w.remote == remote {
			list = append(list, w.local)
		}
	}
	return list
}

// Until an address of the other side answers, the probes leave from where
// the system says and from each address of this machine of the family: the
// system's pick may be one that nothing returns to. The first answer
// decides, and the rest stop.
func TestProbesLeaveFromEachAddress(t *testing.T) {
	remote, quiet := netip.MustParseAddrPort("192.0.2.1:1"), netip.MustParseAddrPort("192.0.2.2:2")
	old, current, v6 := netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("198.51.100.2"), netip.MustParseAddr("2001:db8::1")
	p := newTestPath(t, remote, quiet)
	p.t.locals.Store(&[]netip.Addr{old, current, v6})
	system := netip.Addr{}
	now := time.Now()
	if got := sources(p, now, remote); !slices.Equal(got, []netip.Addr{system, old, current}) {
		t.Fatalf("probing from %v", got)
	}
	// The answer to the probe that left from one of them proves the pair.
	p.mu.Lock()
	p.record(remote, current, time.Millisecond, true, now)
	p.mu.Unlock()
	if got := p.best.Load(); got.local != current {
		t.Fatalf("using %s", got)
	}
	if got := sources(p, now, remote); !slices.Equal(got, []netip.Addr{current}) {
		t.Fatalf("probing from %v once it answers", got)
	}
	// With an address in use, the others are left to the system: its pick
	// follows the routes, and the path does not depend on them.
	p.sweepUntil = now.Add(sweepTime)
	if got := sources(p, now, quiet); !slices.Equal(got, []netip.Addr{system}) {
		t.Fatalf("probing the other address from %v", got)
	}
	// Once the address in use stops answering, the path looks again.
	later := now.Add(staleLimit + time.Second)
	if got := sources(p, later, remote); !slices.Equal(got, []netip.Addr{system, old, current}) {
		t.Fatalf("probing from %v after it stopped answering", got)
	}
}

// The answers to the probes that left from other addresses of this machine
// than the one in use are no measures of the pair.
func TestAnswersAtOtherAddressesAreNoMeasures(t *testing.T) {
	remote := netip.MustParseAddrPort("192.0.2.1:1")
	first, second := netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("198.51.100.2")
	p := newTestPath(t, remote)
	now := time.Now()
	p.record(remote, first, time.Millisecond, true, now)
	p.record(remote, second, time.Second, true, now)
	p.record(remote, second, time.Second, true, now)
	if target := p.targets[remote]; target.samples != 1 || target.rtt != time.Millisecond {
		t.Fatalf("%d measures, of %s", target.samples, target.rtt)
	}
}

// A machine whose address the system picks returns nothing, as when the
// network gave up its prefix, finds the path all the same, from another of
// its addresses, without a probe of the other side to tell which.
func TestConnectWhenTheSystemPicksADeadAddress(t *testing.T) {
	several(t, func(t *testing.T, network string, locals []netip.Addr) {
		tr, err := Listen(0, Network{Addresses: func() []netip.Addr { return locals }}, never)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(tr.Close)
		good := locals[len(locals)-1]
		c, at := peer(t, network, locals[0])
		// The other side: it hears every probe, and its answers come back
		// only to the one address.
		go func() {
			buf := make([]byte, 2048)
			for {
				n, from, err := c.ReadFromUDPAddrPort(buf)
				if err != nil {
					return
				}
				from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
				if n == probeLen && buf[0] == kindProbe && from.Addr() == good {
					c.WriteToUDPAddrPort(answer([tokenLen]byte(buf[1:1+tokenLen]), buf[1+tokenLen+idLen:n], from), from)
				}
			}
		}()
		p, err := tr.Start(nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Close() })
		if err := p.Connect(within(t, 5*time.Second), wire.DirectOffer{Addresses: []string{at.String()}}); err != nil {
			t.Fatal(err)
		}
		if want := netip.AddrPortFrom(good, tr.socketFor(good).port).String(); p.Local() != want {
			t.Fatalf("local %s, not %s", p.Local(), want)
		}
	})
}

// A probe that tries an address of this machine and cannot leave fails with
// no word in the trace: the one from where the system says left with it.
func TestAddressTriedGone(t *testing.T) {
	remote, tried := netip.MustParseAddrPort("192.0.2.1:1"), netip.MustParseAddr("198.51.100.1")
	p := newTestPath(t, remote)
	p.trace = func(text string) { t.Errorf("traced %q", text) }
	if p.unpin(*newPair(tried, remote), syscall.EADDRNOTAVAIL) {
		t.Fatal("a second probe from where the system says")
	}
}

// other is the other side of a path, played by the test: a socket that
// sends what the test says, and tells what arrives.
type other struct {
	t  *testing.T
	c  *net.UDPConn
	p  *Path
	to netip.AddrPort // the path's socket
}

func newOther(t *testing.T, tr *Transport, p *Path) *other {
	c, _ := peer(t, "udp4", loopback)
	return &other{t, c, p, netip.AddrPortFrom(loopback, tr.v4.port)}
}

func (o *other) at() netip.AddrPort {
	return netip.AddrPortFrom(loopback, o.c.LocalAddr().(*net.UDPAddr).AddrPort().Port())
}

func (o *other) send(packet []byte) { o.c.WriteToUDPAddrPort(packet, o.to) }

// next returns the next packet of the kind that arrives within a second;
// nil when none does.
func (o *other) next(kind byte) []byte {
	o.t.Helper()
	buf := make([]byte, 2048)
	o.c.SetReadDeadline(time.Now().Add(time.Second))
	for {
		n, _, err := o.c.ReadFromUDPAddrPort(buf)
		if err != nil {
			return nil
		}
		if buf[0] == kind {
			return buf[:n]
		}
	}
}

// confirm runs the three steps from the other side: its probe, the path's
// answer, and its probe that says that the pair works.
func (o *other) confirm() {
	o.t.Helper()
	o.send(probeOf(o.p, o.to, false))
	if o.next(kindAnswer) == nil {
		o.t.Fatal("no answer")
	}
	o.send(probeOf(o.p, o.to, true))
}

// received makes p tell what it hands on.
func received(p *Path) <-chan []byte {
	got := make(chan []byte, 16)
	p.HandlePackets(func(packet []byte) { got <- slices.Clone(packet) })
	return got
}

// The side whose own probes all fail, as from an address that nothing
// returns to, has its path all the same: the other side's probe, this
// side's answer, and the other side's word that the answer arrived confirm
// a pair, which both then use.
func TestTheOtherSideConfirmsThePair(t *testing.T) {
	tr := transport(t)
	p, err := tr.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	got := received(p)
	o := newOther(t, tr, p)
	// The other side offers an address that never answers.
	_, silent := peer(t, "udp4", loopback)
	connected := make(chan error, 1)
	go func() {
		connected <- p.Connect(within(t, 5*time.Second), wire.DirectOffer{Addresses: []string{silent.String()}})
	}()
	o.confirm()
	if err := <-connected; err != nil {
		t.Fatal(err)
	}
	if want := netip.AddrPortFrom(loopback, tr.v4.port).String(); p.Local() != want || p.Remote() != o.at().String() {
		t.Fatalf("using %s to %s, not %s to %s", p.Local(), p.Remote(), want, o.at())
	}
	// The answer to the probe that says so repeats it: the other side then
	// knows that this side knows.
	if a := o.next(kindAnswer); a == nil || a[1+tokenLen+echoLen-1]&flagWorks == 0 {
		t.Fatalf("the answer %v does not repeat that the pair works", a)
	}
	if err := p.WritePacket(wg4()); err != nil {
		t.Fatal(err)
	}
	if o.next(wg4()[0]) == nil {
		t.Fatal("the packet did not arrive at the other side")
	}
	o.send(wg4())
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the packet did not arrive")
	}
}

// The side that offers has its path before it hears the answer to its
// offer over SSH, when the other side's probes were faster.
func TestConfirmedBeforeConnect(t *testing.T) {
	tr := transport(t)
	p, err := tr.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	o := newOther(t, tr, p)
	o.confirm()
	if o.next(kindAnswer) == nil {
		t.Fatal("no answer")
	}
	if err := p.Connect(within(t, 5*time.Second), wire.DirectOffer{}); err != nil {
		t.Fatal(err)
	}
	if p.Remote() != o.at().String() {
		t.Fatalf("using %s", p.Remote())
	}
	// It keeps the pair, saying that it works.
	if probe := o.next(kindProbe); probe == nil || probe[probeLen-1]&flagWorks == 0 {
		t.Fatalf("probe %v", probe)
	}
}

// A side sends nothing on a pair until the other side knows that it works:
// answers alone, which never repeat that this side said so, make no path.
func TestNoPathUntilBothSidesKnow(t *testing.T) {
	tr := transport(t)
	p, err := tr.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	o := newOther(t, tr, p)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, _, err := o.c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			if n == probeLen && buf[0] == kindProbe {
				echo := slices.Clone(buf[1+tokenLen+idLen : n])
				echo[echoLen-1] = 0
				o.send(answer(p.token, echo, o.to))
			}
		}
	}()
	if err := p.Connect(within(t, time.Second), wire.DirectOffer{Addresses: []string{o.at().String()}}); err == nil {
		t.Fatal("a path that the other side does not know of")
	}
	if !p.works.Load() {
		t.Fatal("the answers did not arrive")
	}
	if err := p.WritePacket(wg4()); err == nil {
		t.Fatal("sent a packet on it")
	}
}

// A side hands on packets once it knows a pair to work, not before: the
// other side sends only on such a pair, and a probe alone proves none.
func TestPacketsWaitForAPairThatWorks(t *testing.T) {
	tr := transport(t)
	p, err := tr.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	got := received(p)
	o := newOther(t, tr, p)
	o.send(probeOf(p, o.to, false))
	if o.next(kindAnswer) == nil {
		t.Fatal("no answer")
	}
	o.send(wg4())
	select {
	case <-got:
		t.Fatal("handed on a packet before any pair worked")
	case <-time.After(200 * time.Millisecond):
	}
	o.send(probeOf(p, o.to, true))
	if o.next(kindAnswer) == nil {
		t.Fatal("no answer")
	}
	o.send(wg4())
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the packet did not arrive")
	}
}

// A try that ends without a path tells the other side, wherever its probes
// came from, so that it stops trying too.
func TestClosingATryTellsTheOtherSide(t *testing.T) {
	tr := transport(t)
	p, err := tr.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	o := newOther(t, tr, p)
	o.send(probeOf(p, o.to, false))
	if o.next(kindAnswer) == nil {
		t.Fatal("no answer")
	}
	p.Close()
	if o.next(kindBye) == nil {
		t.Fatal("the other side was not told")
	}
}
