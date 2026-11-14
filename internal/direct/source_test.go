package direct

import (
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
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

func probeOf(p *Path, to netip.AddrPort) []byte {
	packet := make([]byte, 0, probeLen)
	packet = append(packet, kindProbe)
	packet = append(packet, p.token[:]...)
	packet = append(packet, make([]byte, idLen+8)...)
	return appendAddr(packet, to)
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
			other.WriteToUDPAddrPort(probeOf(p, to), to)
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
			p.record(at, locals[len(locals)-1-i], time.Millisecond, time.Now())
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
	p.record(remote, first, time.Millisecond, now)
	p.record(remote, second, time.Millisecond, now.Add(time.Millisecond))
	if got := p.best.Load(); got.local != first || p.targets[remote].local != first {
		t.Fatalf("using %s", got)
	}
	if got := p.due(now); len(got) != 1 || got[0].local != first {
		t.Fatalf("probing %v", got)
	}
	later := now.Add(staleLimit + time.Second)
	if got := p.due(later); len(got) != 1 || got[0].local.IsValid() {
		t.Fatalf("probing %v after it stopped answering", got)
	}
	p.record(remote, second, time.Millisecond, later)
	if got := p.best.Load(); got.local != second {
		t.Fatalf("using %s", got)
	}
}

// When this side changes network, the probes leave from where the system
// says, and the packets keep their address until an answer tells another.
func TestRemeasureAsksTheSystem(t *testing.T) {
	remote := netip.MustParseAddrPort("192.0.2.1:1")
	first, second := netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("198.51.100.2")
	p := newTestPath(t, remote)
	now := time.Now()
	p.record(remote, first, time.Millisecond, now)
	p.Remeasure()
	if got := p.due(now); len(got) != 1 || got[0].local.IsValid() {
		t.Fatalf("probing %v", got)
	}
	if got := p.best.Load(); got.local != first {
		t.Fatalf("using %s before an answer", got)
	}
	p.mu.Lock()
	p.record(remote, second, time.Millisecond, now.Add(time.Millisecond))
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
			p.record(at, c.gone, time.Millisecond, time.Now())
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
			p.record(at, c.at, time.Millisecond, time.Now())
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
		tr, err := Listen(0, func() []netip.Addr { return []netip.Addr{netip.IPv6Loopback()} }, never)
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
