package vnet

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// startInside starts a node that answers inside the interface, as on a
// system that keeps its sockets off it.
func startInside(t *testing.T, w *world, id, name string) *testNode {
	t.Helper()
	n := &testNode{mesh: w.add(id, name), dir: t.TempDir(), tune: func(cfg *Config) {
		cfg.Inside, cfg.DNS, cfg.Echo = true, "198.18.0.53:53", 15354
	}}
	n.run(t)
	return n
}

func TestInsideAnswersDNS(t *testing.T) {
	var w world
	a := startInside(t, &w, "a0", "phone")
	b := start(t, &w, "b0", "devbox")
	a.manager.Configure(t.Context())
	b.manager.Configure(t.Context())
	waitFor(t, "a is up", up(a, 1))
	waitFor(t, "b is up", up(b, 1))

	var pk inside
	own := netip.AddrPortFrom(netip.MustParseAddr(a.manager.Status().Address), 50000)
	dns := netip.MustParseAddrPort("198.18.0.53:53")
	a.device().in <- pk.appendUDP(nil, own, dns, query(t, "devbox.gst.", dnsmessage.TypeA))
	var reply udpPacket
	select {
	case p := <-a.device().out:
		var ok bool
		if reply, ok = parseUDP(p); !ok || reply.from != dns || reply.to != own {
			t.Fatalf("the reply is %x", p)
		}
		if checksum(0, p[:20]) != 0xffff || checksum(uint32(17+len(p)-20), p[12:]) != 0xffff {
			t.Fatalf("the reply's checksums are wrong: %x", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no reply to the query")
	}
	var m dnsmessage.Message
	if err := m.Unpack(reply.payload); err != nil || len(m.Answers) != 1 {
		t.Fatalf("the answer is %+v, %v", m, err)
	}
	got := netip.AddrFrom4(m.Answers[0].Body.(*dnsmessage.AResource).A)
	if want := b.manager.Status().Address; got.String() != want {
		t.Fatalf("devbox.gst is %s, want %s", got, want)
	}

	// Other packets, to that port too, go on to the other node.
	wantPacket(t, a, b, "after")
	to := netip.AddrPortFrom(netip.MustParseAddr(b.manager.Status().Address), 53)
	a.device().in <- pk.appendUDP(nil, own, to, []byte("not for here"))
	select {
	case p := <-b.device().out:
		if u, ok := parseUDP(p); !ok || string(u.payload) != "not for here" {
			t.Fatalf("b got %x", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the packet did not reach b")
	}
}

func TestInsideForwardsDNS(t *testing.T) {
	var w world
	asked := make(chan string, 4)
	a := &testNode{mesh: w.add("a0", "phone"), dir: t.TempDir(), tune: func(cfg *Config) {
		cfg.Inside, cfg.DNS, cfg.Echo = true, "198.18.0.53:53", 15354
		cfg.Forward = func(query []byte) ([]byte, error) {
			var m dnsmessage.Message
			if err := m.Unpack(query); err != nil {
				return nil, err
			}
			asked <- m.Questions[0].Name.String()
			m.Response = true
			m.Answers = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
				Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}},
			}}
			return m.Pack()
		}
	}}
	a.run(t)
	a.manager.Configure(t.Context())
	waitFor(t, "a is up", up(a, 0))

	var pk inside
	own := netip.AddrPortFrom(netip.MustParseAddr(a.manager.Status().Address), 50000)
	dns := netip.MustParseAddrPort("198.18.0.53:53")
	ask := func(name string) dnsmessage.Message {
		t.Helper()
		a.device().in <- pk.appendUDP(nil, own, dns, query(t, name, dnsmessage.TypeA))
		select {
		case p := <-a.device().out:
			reply, ok := parseUDP(p)
			var m dnsmessage.Message
			if !ok || reply.from != dns || m.Unpack(reply.payload) != nil {
				t.Fatalf("the reply is %x", p)
			}
			return m
		case <-time.After(10 * time.Second):
			t.Fatalf("no reply for %s", name)
		}
		return dnsmessage.Message{}
	}

	// A name outside .gst goes to the system's resolver.
	if m := ask("www.example.com."); len(m.Answers) != 1 || m.Answers[0].Body.(*dnsmessage.AResource).A != [4]byte{192, 0, 2, 1} {
		t.Fatalf("the answer for www.example.com is %+v", m)
	}
	if got := <-asked; got != "www.example.com." {
		t.Fatalf("the resolver was asked for %s", got)
	}
	// A name under .gst stays with gst.
	if m := ask("phone.gst."); len(m.Answers) != 1 || netip.AddrFrom4(m.Answers[0].Body.(*dnsmessage.AResource).A) != own.Addr() {
		t.Fatalf("the answer for phone.gst is %+v", m)
	}
	select {
	case got := <-asked:
		t.Fatalf("the resolver was asked for %s", got)
	default:
	}
}

func TestInsidePing(t *testing.T) {
	var w world
	a := startInside(t, &w, "a0", "phone")
	b := startInside(t, &w, "b0", "tablet")
	a.manager.Configure(t.Context())
	b.manager.Configure(t.Context())
	waitFor(t, "a is up", up(a, 1))
	waitFor(t, "b is up", up(b, 1))
	self, other := a.manager.Status().Address, b.manager.Status().Address

	// The first packets may be lost to WireGuard's handshake.
	var err error
	for range 5 {
		if _, err = a.manager.Ping(t.Context(), other, 0); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, err := a.manager.Ping(t.Context(), other, maxInsidePing); err != nil {
		t.Fatalf("a ping as large as the interface takes: %v", err)
	}
	if _, err := a.manager.Ping(t.Context(), other, maxInsidePing+1); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("a ping too large: %v", err)
	}
	if _, err := a.manager.Ping(t.Context(), self, 0); err != nil {
		t.Fatalf("a ping of this node: %v", err)
	}
	// Neither system saw any of it.
	for _, n := range []*testNode{a, b} {
		select {
		case p := <-n.device().out:
			t.Fatalf("a system got %x", p)
		default:
		}
	}

	// A packet to the port from elsewhere than a ping's answer is the
	// system's, and so is everything else.
	var pk inside
	stray := pk.appendUDP(nil, netip.AddrPortFrom(netip.MustParseAddr(self), 15354), netip.AddrPortFrom(netip.MustParseAddr(other), 40000), []byte("stray"))
	a.device().in <- stray
	select {
	case p := <-b.device().out:
		if !bytes.Equal(p, stray) {
			t.Fatalf("b got %x", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the packet did not reach b's system")
	}
	wantPacket(t, a, b, "after")
}

func TestParseUDP(t *testing.T) {
	var pk inside
	from, to := netip.MustParseAddrPort("10.1.0.1:5000"), netip.MustParseAddrPort("10.1.0.2:53")
	p := pk.appendUDP(nil, from, to, []byte("body"))
	if u, ok := parseUDP(p); !ok || u.from != from || u.to != to || string(u.payload) != "body" {
		t.Fatalf("parsed %+v, %v", u, ok)
	}
	fragment := bytes.Clone(p)
	fragment[6] |= 0x20 // more fragments follow
	tcp := bytes.Clone(p)
	tcp[9] = 6
	long := bytes.Clone(p)
	long[3]++ // longer than the packet
	for name, bad := range map[string][]byte{"a fragment": fragment, "TCP": tcp, "a short packet": p[:27], "a wrong length": long, "IPv6": append([]byte{0x60}, p[1:]...)} {
		if u, ok := parseUDP(bad); ok {
			t.Errorf("%s parsed as %+v", name, u)
		}
	}
}
