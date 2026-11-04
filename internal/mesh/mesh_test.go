package mesh

import (
	"context"
	"errors"
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
	n.SetRetry(20 * time.Millisecond)
	if name != "" {
		n.Configure(&config.Mesh{ID: fmt.Sprintf("%032x", []byte(name)), Name: name})
	}
	return n
}

// linkTunnel adds a running tunnel with mesh on to client, whose server
// is server, with host key fingerprint hostKey. Its peer links run over
// in-process pipes that server accepts as its daemon would.
func linkTunnel(ctx context.Context, client, server *Node, name, hostKey string) *Tunnel {
	t := client.AddTunnel(name, name, "", log.New(io.Discard, "", 0))
	t.SetState(wire.StateRunning)
	go t.connected(ctx, nil, hostKey, func() (io.ReadWriteCloser, error) {
		a, b := net.Pipe()
		go func() {
			theirs, err := wire.ReadHello(b)
			if err != nil {
				b.Close()
				return
			}
			server.Accept(ctx, b, theirs)
		}()
		return a, nil
	})
	return t
}

// outLink returns n's link over tunnel, if any.
// outLink returns the state of a tunnel's link, which Peers leaves out
// while the mesh is disabled.
func outLink(n *Node, tunnel string) (wire.PeerLink, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for l := range n.links {
		if s := l.state(); s.Direction == wire.LinkOut && s.Tunnel == tunnel {
			return s, true
		}
	}
	return wire.PeerLink{}, false
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
	linkTunnel(ctx, m3, mini, "srv", "")

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

	// Disabling the mesh on the server closes the link on both sides, and
	// the server refuses it from then on.
	mini.Configure(nil)
	waitFor(t, "link survived disabling the mesh", func() bool {
		l, _ := outLink(m3, "srv")
		return l.State != wire.LinkUp && strings.Contains(l.Error, "gst mesh enable")
	})
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
		{"client disabled", newNode(""), newNode("m3"), "not enabled on this machine"},
	} {
		linkTunnel(ctx, tt.client, tt.server, "srv", "")
		waitFor(t, tt.name+": link did not fail with "+tt.want, func() bool {
			l, _ := outLink(tt.client, "srv")
			return l.State == wire.LinkDown && strings.Contains(l.Error, tt.want)
		})
	}
	// A machine without the mesh tries no link and lists none.
	client, server := newNode(""), newNode("m3")
	linkTunnel(ctx, client, server, "srv", "")
	time.Sleep(100 * time.Millisecond)
	if len(client.Peers().Links) != 0 || len(server.Peers().Links) != 0 {
		t.Fatalf("links without the mesh: %+v, %+v", client.Peers().Links, server.Peers().Links)
	}
}

func TestUnknownPeerMethods(t *testing.T) {
	n := newNode("m3")
	a, b := net.Pipe()
	defer a.Close()
	go n.peer.Serve(context.Background(), b)
	var werr *wire.Error
	if err := wire.Call(a, "mesh.unknown", nil, nil); !errors.As(err, &werr) || werr.Code != wire.CodeUnsupportedMethod {
		t.Fatalf("got %v", err)
	}
}
