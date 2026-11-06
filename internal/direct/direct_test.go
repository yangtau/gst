package direct

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

var loopback = []netip.Addr{netip.MustParseAddr("127.0.0.1")}

func attempt(t *testing.T) *Attempt {
	t.Helper()
	a, err := New(loopback)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

// connect runs a try between two attempts. offer and answer are what
// each side gets from the other.
func connect(ctx context.Context, t *testing.T, client, server *Attempt, offer, answer wire.DirectOffer) (*Conn, *Conn, error) {
	t.Helper()
	type result struct {
		conn *Conn
		err  error
	}
	accepted := make(chan result, 1)
	go func() {
		conn, err := server.Accept(ctx, offer)
		accepted <- result{conn, err}
	}()
	c, err := client.Dial(ctx, answer)
	if err != nil {
		return nil, nil, err
	}
	t.Cleanup(func() { c.Close() })
	s := <-accepted
	if s.err != nil {
		return nil, nil, s.err
	}
	t.Cleanup(func() { s.conn.Close() })
	return c, s.conn, nil
}

func within(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)
	return ctx
}

// exchange checks that packets go both ways.
func exchange(t *testing.T, a, b *Conn) {
	t.Helper()
	for _, ends := range [][2]*Conn{{a, b}, {b, a}} {
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
	client, server := attempt(t), attempt(t)
	start := time.Now()
	c, s, err := connect(within(t, 10*time.Second), t, client, server, client.Offer(), server.Offer())
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("connecting took %v, as if it waited for the context", took)
	}
	// The sockets that the attempts did not use go; the connection stays.
	client.Close()
	server.Close()
	exchange(t, c, s)
	exchange(t, s, c)
}

// Bursts of packets arrive, as the frames of a video do.
func TestPackets(t *testing.T) {
	client, server := attempt(t), attempt(t)
	c, s, err := connect(within(t, 10*time.Second), t, client, server, client.Offer(), server.Offer())
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	server.Close()
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

// What is not WireGuard's stays out, and so does what comes from another
// address.
func TestStrayPackets(t *testing.T) {
	client, server := attempt(t), attempt(t)
	c, s, err := connect(within(t, 10*time.Second), t, client, server, client.Offer(), server.Offer())
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 16)
	s.HandlePackets(func(p []byte) { got <- string(p) })
	stranger, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(loopback[0], 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer stranger.Close()
	to, err := netip.ParseAddrPort(s.Local())
	if err != nil {
		t.Fatal(err)
	}
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
			client, server := attempt(t), attempt(t)
			offer, answer := client.Offer(), server.Offer()
			tc.hidden(&offer, &answer)
			c, s, err := connect(within(t, 10*time.Second), t, client, server, offer, answer)
			if err != nil {
				t.Fatal(err)
			}
			exchange(t, c, s)
		})
	}
}

// Probes with another token prove nothing: the other side stays silent.
func TestWrongToken(t *testing.T) {
	client, server := attempt(t), attempt(t)
	offer := client.Offer()
	offer.Token = "00000000000000000000000000000000"
	if _, _, err := connect(within(t, time.Second), t, client, server, offer, server.Offer()); err == nil {
		t.Fatal("connected with a wrong token")
	}
}

func TestInvalidOffers(t *testing.T) {
	a := attempt(t)
	good := a.Offer()
	for name, offer := range map[string]wire.DirectOffer{
		"token":     {Token: "xx", Addresses: good.Addresses},
		"address":   {Token: good.Token, Addresses: []string{"nonsense"}},
		"addresses": {Token: good.Token, Addresses: make([]string, maxAddresses+1)},
	} {
		if _, err := a.Accept(t.Context(), offer); err == nil {
			t.Errorf("accepted an offer with an invalid %s", name)
		}
	}
}

func TestNoAddress(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("started without addresses")
	}
}

// Closing a path tells the other side at once.
func TestClose(t *testing.T) {
	client, server := attempt(t), attempt(t)
	c, s, err := connect(within(t, 10*time.Second), t, client, server, client.Offer(), server.Offer())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	select {
	case <-s.Done():
	case <-time.After(silenceLimit / 2):
		t.Fatal("the other side did not notice")
	}
}

// A path whose other end stopped answering ends by itself.
func TestSilence(t *testing.T) {
	client, server := attempt(t), attempt(t)
	c, s, err := connect(within(t, 10*time.Second), t, client, server, client.Offer(), server.Offer())
	if err != nil {
		t.Fatal(err)
	}
	s.end() // Without a word to the other side.
	select {
	case <-c.Done():
	case <-time.After(2 * silenceLimit):
		t.Fatal("the path did not end")
	}
}

// A path keeps running while the two ends hear each other.
func TestBeats(t *testing.T) {
	client, server := attempt(t), attempt(t)
	c, s, err := connect(within(t, 10*time.Second), t, client, server, client.Offer(), server.Offer())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
		t.Fatal("the path ended")
	case <-s.Done():
		t.Fatal("the path ended")
	case <-time.After(silenceLimit + beatInterval):
	}
	exchange(t, c, s)
}

func TestAddresses(t *testing.T) {
	for _, addr := range Addresses() {
		if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsMulticast() || !addr.IsValid() {
			t.Errorf("unusable address %s", addr)
		}
	}
}
