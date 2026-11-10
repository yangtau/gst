package route

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func id(c byte) string { return strings.Repeat(string(c), 32) }

// net is routers joined by in-memory links that deliver in the background.
type net struct {
	routers map[string]*Router
	mu      sync.Mutex
	hops    []string // "from>to" of every message on a link
}

func newNet(names ...byte) *net {
	n := &net{routers: map[string]*Router{}}
	for _, c := range names {
		r := New()
		r.SetSelf(id(c))
		Handle(r, "whoami", func(_ context.Context, from string, p struct{ Say string }) (any, error) {
			return map[string]string{"me": string(c), "from": from[:1], "say": p.Say}, nil
		})
		n.routers[id(c)] = r
	}
	return n
}

func (n *net) link(a, b byte) (cut func()) {
	send := func(from, to byte) Sender {
		return func(body []byte, _ bool) bool {
			n.mu.Lock()
			n.hops = append(n.hops, string(from)+">"+string(to))
			n.mu.Unlock()
			go n.routers[id(to)].Receive(append([]byte(nil), body...))
			return true
		}
	}
	ra := n.routers[id(a)].AddLink(id(b), send(a, b))
	rb := n.routers[id(b)].AddLink(id(a), send(b, a))
	return func() { ra(); rb() }
}

func (n *net) route(at byte, routes map[byte]byte) {
	next := map[string]string{}
	for to, hop := range routes {
		next[id(to)] = id(hop)
	}
	n.routers[id(at)].SetRoutes(next)
}

func TestCallAcrossTheMesh(t *testing.T) {
	n := newNet('a', 'b', 'c')
	n.link('a', 'b')
	n.link('b', 'c')
	n.route('a', map[byte]byte{'b': 'b', 'c': 'b'})
	n.route('b', map[byte]byte{'a': 'a', 'c': 'c'})
	n.route('c', map[byte]byte{'b': 'b', 'a': 'b'})
	var got map[string]string
	if err := n.routers[id('a')].Call(context.Background(), id('c'), "whoami", map[string]string{"say": "hi"}, &got); err != nil {
		t.Fatal(err)
	}
	if got["me"] != "c" || got["from"] != "a" || got["say"] != "hi" {
		t.Fatalf("got %v", got)
	}
	err := n.routers[id('a')].Call(context.Background(), id('c'), "nothing", nil, nil)
	var e *wire.Error
	if !errors.As(err, &e) || e.Code != wire.CodeUnsupportedMethod {
		t.Fatalf("unknown method: %v", err)
	}
	// A call to this node runs here.
	if err := n.routers[id('a')].Call(context.Background(), id('a'), "whoami", nil, &got); err != nil || got["me"] != "a" {
		t.Fatalf("self: %v %v", err, got)
	}
}

func TestUnreachableComesBackAtOnce(t *testing.T) {
	n := newNet('a', 'b', 'c')
	n.link('a', 'b')
	n.route('a', map[byte]byte{'b': 'b', 'c': 'b'})
	n.route('b', map[byte]byte{'a': 'a'}) // b has no way to c
	start := time.Now()
	err := n.routers[id('a')].Call(context.Background(), id('c'), "whoami", nil, nil)
	var e *wire.Error
	if !errors.As(err, &e) || e.Code != wire.CodeUnreachable || time.Since(start) > time.Second {
		t.Fatalf("got %v after %s", err, time.Since(start))
	}
	// No way from here either.
	err = n.routers[id('c')].Call(context.Background(), id('a'), "whoami", nil, nil)
	if !errors.As(err, &e) || e.Code != wire.CodeUnreachable {
		t.Fatalf("got %v", err)
	}
}

func TestLoopsEnd(t *testing.T) {
	n := newNet('a', 'b', 'c')
	n.link('a', 'b')
	// a and b each think that the other leads to c.
	n.route('a', map[byte]byte{'b': 'b', 'c': 'b'})
	n.route('b', map[byte]byte{'a': 'a', 'c': 'a'})
	err := n.routers[id('a')].Call(context.Background(), id('c'), "whoami", nil, nil)
	var e *wire.Error
	if !errors.As(err, &e) || e.Code != wire.CodeUnreachable {
		t.Fatalf("got %v", err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.hops) > 2*InitialTTL+2 {
		t.Fatalf("%d hops", len(n.hops))
	}
}

func TestNextMessageTakesTheNewRoute(t *testing.T) {
	n := newNet('a', 'b', 'c', 'd')
	cutAB := n.link('a', 'b')
	n.link('b', 'd')
	n.link('a', 'c')
	n.link('c', 'd')
	n.route('a', map[byte]byte{'d': 'b'})
	n.route('b', map[byte]byte{'a': 'a', 'd': 'd'})
	n.route('c', map[byte]byte{'a': 'a', 'd': 'd'})
	n.route('d', map[byte]byte{'a': 'b'})
	if err := n.routers[id('a')].Call(context.Background(), id('d'), "whoami", nil, nil); err != nil {
		t.Fatal(err)
	}
	cutAB()
	n.route('a', map[byte]byte{'d': 'c'})
	n.route('d', map[byte]byte{'a': 'c'})
	if err := n.routers[id('a')].Call(context.Background(), id('d'), "whoami", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPackets(t *testing.T) {
	n := newNet('a', 'b', 'c')
	n.link('a', 'b')
	n.link('b', 'c')
	n.route('a', map[byte]byte{'c': 'b'})
	n.route('b', map[byte]byte{'c': 'c', 'a': 'a'})
	got := make(chan string, 1)
	n.routers[id('c')].HandlePackets(func(from string, packet []byte) { got <- from[:1] + string(packet) })
	if !n.routers[id('a')].SendPacket(id('c'), []byte("wg")) {
		t.Fatal("not sent")
	}
	select {
	case p := <-got:
		if p != "awg" {
			t.Fatalf("got %q", p)
		}
	case <-time.After(time.Second):
		t.Fatal("no packet")
	}
	if n.routers[id('a')].SendPacket(id('d'), []byte("wg")) {
		t.Fatal("a packet to nowhere went")
	}
}
