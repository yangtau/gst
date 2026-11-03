package mesh

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func newNode(name string) *Node {
	n := New("gst test", log.New(io.Discard, "", 0))
	if name != "" {
		n.Configure(&config.Mesh{ID: fmt.Sprintf("%032x", []byte(name)), Name: name})
	}
	return n
}

// connect links client to server as a tunnel with mesh on would, with the
// handshake that the server's daemon performs on its control socket, and
// returns the outgoing link's result once it ends.
func connect(ctx context.Context, client, server *Node) (*link, chan error) {
	a, b := net.Pipe()
	go func() {
		_, err := wire.Accept(b, wire.Hello{Role: wire.RoleControl}, server.CheckHello, func(wire.Hello) wire.Hello { return server.Hello() })
		if err != nil {
			b.Close()
			return
		}
		server.Accept(ctx, b)
	}()
	l := client.add(wire.LinkOut, "srv")
	done := make(chan error, 1)
	go func() {
		done <- client.linkOver(ctx, l, a, nil)
		client.remove(l)
	}()
	return l, done
}

func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLinkIntroducesPeers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m3, mini := newNode("m3"), newNode("mini")
	_, done := connect(ctx, m3, mini)

	waitFor(t, "links not up", func() bool {
		out, in := m3.Peers().Links, mini.Peers().Links
		return len(out) == 1 && out[0].State == wire.LinkUp && len(in) == 1 && in[0].State == wire.LinkUp
	})
	out, in := m3.Peers().Links[0], mini.Peers().Links[0]
	if out.Direction != wire.LinkOut || out.Tunnel != "srv" || out.Peer.Name != "mini" || out.Peer.ID != mini.self().ID {
		t.Fatalf("outgoing link %+v", out)
	}
	if in.Direction != wire.LinkIn || in.Peer.Name != "m3" || in.Peer.Software != "gst test" {
		t.Fatalf("incoming link %+v", in)
	}

	// Disabling the mesh on the server closes the link on both sides.
	mini.Configure(nil)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("link ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("link survived disabling the mesh")
	}
	waitFor(t, "incoming link not removed", func() bool { return len(mini.Peers().Links) == 0 })
}

func TestLinkRefusals(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, tt := range []struct {
		name           string
		client, server *Node
		want           string
	}{
		{"server disabled", newNode("m3"), newNode(""), "gst mesh enable"},
		{"self", newNode("m3"), newNode("m3"), "this machine"},
	} {
		_, done := connect(ctx, tt.client, tt.server)
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("%s: got %v, want %q", tt.name, err, tt.want)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: link did not fail", tt.name)
		}
	}
}

func TestUnknownPeerMethods(t *testing.T) {
	n := newNode("m3")
	if _, err := n.handle("topology.sync", nil); err == nil || err.Code != wire.CodeUnsupportedMethod {
		t.Fatalf("got %v", err)
	}
}
