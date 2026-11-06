package direct

import (
	"context"
	"fmt"
	"io"
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

// exchange checks that a stream carries data both ways and half-closes.
func exchange(t *testing.T, a, b *Conn) {
	t.Helper()
	ctx := within(t, 5*time.Second)
	out, err := a.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := out.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := out.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	in, err := b.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	got, err := io.ReadAll(in)
	if err != nil || string(got) != "ping" {
		t.Fatalf("read %q, %v", got, err)
	}
	if _, err := in.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	if err := in.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(out)
	if err != nil || string(got) != "pong" {
		t.Fatalf("read %q, %v", got, err)
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

// Packets of another protocol share the socket with QUIC, in bursts that
// no queue of quic-go's would hold, and leave its streams alone.
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
	exchange(t, c, s)
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

func TestWrongCertificate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wrong func(offer, answer *wire.DirectOffer)
	}{
		{"client", func(offer, _ *wire.DirectOffer) { offer.Fingerprint = "00" }},
		{"server", func(_, answer *wire.DirectOffer) { answer.Fingerprint = "00" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := attempt(t), attempt(t)
			offer, answer := client.Offer(), server.Offer()
			tc.wrong(&offer, &answer)
			if _, _, err := connect(within(t, 2*time.Second), t, client, server, offer, answer); err == nil {
				t.Fatal("connected with a wrong certificate")
			}
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

// When the connection ends, reading a stream fails rather than ending
// cleanly, and writing that was waiting ends.
func TestConnectionLost(t *testing.T) {
	client, server := attempt(t), attempt(t)
	c, s, err := connect(within(t, 10*time.Second), t, client, server, client.Offer(), server.Offer())
	if err != nil {
		t.Fatal(err)
	}
	ctx := within(t, 5*time.Second)
	out, err := c.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out.Write([]byte("x"))
	in, err := s.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in.Read(make([]byte, 1))
	c.Close()
	in.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := in.Read(make([]byte, 1)); err == nil || err == io.EOF {
		t.Fatalf("read after losing the connection: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the other side did not notice")
	}
}

// Close ends a write that waits for the other side, as a relay that
// breaks down needs.
func TestCloseDuringWrite(t *testing.T) {
	client, server := attempt(t), attempt(t)
	c, _, err := connect(within(t, 10*time.Second), t, client, server, client.Offer(), server.Offer())
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Open(within(t, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() {
		chunk := make([]byte, 1<<20)
		for {
			if _, err := out.Write(chunk); err != nil {
				wrote <- err
				return
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	closed := make(chan struct{})
	go func() {
		out.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not return")
	}
	select {
	case <-wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("the write did not end")
	}
}

var _ net.Conn = (*stream)(nil)

func TestAddresses(t *testing.T) {
	for _, addr := range Addresses() {
		if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsMulticast() || !addr.IsValid() {
			t.Errorf("unusable address %s", addr)
		}
	}
}
