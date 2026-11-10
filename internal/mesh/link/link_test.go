package link

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

const (
	idA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	idB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var fast = Timing{Alive: 20 * time.Millisecond, Dead: 100 * time.Millisecond}

func side(id, name string, gen int64) Handshake {
	return Handshake{
		Self:       wire.NodeInfo{ID: id, Name: name, Software: "gst test"},
		Membership: func() *wire.Membership { return &wire.Membership{Generation: gen} },
		Timing:     fast,
	}
}

// shake runs a handshake between a and b over a pipe.
func shake(t *testing.T, a, b Handshake) (*Link, error, *Link, error) {
	t.Helper()
	ca, cb := net.Pipe()
	var lb *Link
	var errB error
	done := make(chan struct{})
	go func() {
		defer close(done)
		theirs, err := wire.ReadHello(cb)
		if err != nil {
			errB = err
			return
		}
		lb, errB = b.Accept(cb, theirs)
	}()
	la, errA := a.Dial(ca)
	<-done
	return la, errA, lb, errB
}

type inbox struct {
	mu     sync.Mutex
	frames []string
}

func (i *inbox) handle(t Type, body []byte) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.frames = append(i.frames, string(rune('0'+t))+string(body))
}

func (i *inbox) list() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.frames...)
}

func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting: " + msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func run(l *Link, in *inbox) chan error {
	ended := make(chan error, 1)
	go func() { ended <- l.Run(in.handle) }()
	return ended
}

func TestHandshakeAndFrames(t *testing.T) {
	la, errA, lb, errB := shake(t, side(idA, "laptop", 0), side(idB, "devbox", 2))
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	if la.Peer.ID != idB || la.Peer.Name != "devbox" || la.Membership.Generation != 2 || !la.Opened {
		t.Fatalf("opener sees %+v %+v", la.Peer, la.Membership)
	}
	if lb.Peer.ID != idA || lb.Opened || lb.Peer.Software != "gst test" {
		t.Fatalf("accepter sees %+v", lb.Peer)
	}
	var ia, ib inbox
	endA, endB := run(la, &ia), run(lb, &ib)
	la.Send(Sync, []byte("s1"), true)
	la.Send(Msg, []byte("m1"), false)
	lb.Send(Msg, []byte("m2"), true)
	waitFor(t, "frames", func() bool { return len(ib.list()) == 2 && len(ia.list()) == 1 })
	if got := ib.list(); got[0] != "1s1" || got[1] != "2m1" {
		t.Fatalf("b got %q", got)
	}
	// Alive frames keep an idle link up.
	time.Sleep(3 * fast.Dead)
	select {
	case err := <-endA:
		t.Fatalf("idle link ended: %v", err)
	default:
	}
	la.Close(&wire.Bye{Error: wire.Errorf(wire.CodeRemoved, "bye now")})
	if err := <-endA; !errors.Is(err, ErrClosed) {
		t.Fatalf("closing side: %v", err)
	}
	var bye *ByeError
	if err := <-endB; !errors.As(err, &bye) || bye.Bye.Error.Code != wire.CodeRemoved {
		t.Fatalf("other side: %v", err)
	}
}

func TestUrgentFramesGoFirst(t *testing.T) {
	la, _, lb, _ := shake(t, side(idA, "laptop", 0), side(idB, "devbox", 0))
	for range 10 {
		la.Send(Msg, []byte("packet"), false)
	}
	la.Send(Sync, []byte("table"), true)
	var ia, ib inbox
	run(la, &ia)
	run(lb, &ib)
	waitFor(t, "frames", func() bool { return len(ib.list()) == 11 })
	if got := ib.list()[0]; got != "1table" {
		t.Fatalf("first frame %q", got)
	}
}

func TestBulkQueueDrops(t *testing.T) {
	la, _, _, _ := shake(t, side(idA, "laptop", 0), side(idB, "devbox", 0))
	sent := 0
	for range bulkQueue + 10 {
		if la.Send(Msg, []byte("p"), false) {
			sent++
		}
	}
	if sent != bulkQueue {
		t.Fatalf("queued %d packets", sent)
	}
	if !la.Send(Sync, []byte("table"), true) {
		t.Fatal("urgent frame dropped")
	}
	if la.Send(Msg, make([]byte, MaxBody+1), true) {
		t.Fatal("oversized frame queued")
	}
}

func TestSilentPeerEndsTheLink(t *testing.T) {
	la, _, lb, _ := shake(t, side(idA, "laptop", 0), side(idB, "devbox", 0))
	_ = lb // never runs: it reads nothing and sends nothing
	go func() {
		// Drain what la writes, as a stalled node's buffers would.
		buf := make([]byte, 1024)
		for {
			if _, err := lb.conn.Read(buf); err != nil {
				return
			}
		}
	}()
	var ia inbox
	select {
	case err := <-run(la, &ia):
		if err == nil || errors.Is(err, ErrClosed) {
			t.Fatalf("ended with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a silent peer kept the link")
	}
}

func TestRefusals(t *testing.T) {
	removed := &wire.Entry{Key: "removed/" + idA, Version: []int64{0}}
	refuse := func(h Handshake) Handshake {
		h.Check = func(peer wire.NodeInfo, m wire.Membership) *Refusal {
			return &Refusal{Err: wire.Errorf(wire.CodeRemoved, "%s was removed", peer.Name), Membership: &wire.Membership{Removed: removed}}
		}
		return h
	}

	// The accepting side refuses in its Hello.
	_, errA, _, errB := shake(t, side(idA, "laptop", 0), refuse(side(idB, "devbox", 0)))
	var r *RefusedError
	if !errors.As(errA, &r) || !r.ByPeer || r.Err.Code != wire.CodeRemoved || r.Membership == nil || r.Membership.Removed.Key != removed.Key || r.Peer.ID != idB {
		t.Fatalf("opener: %v %+v", errA, r)
	}
	if !errors.As(errB, &r) || r.ByPeer {
		t.Fatalf("accepter: %v", errB)
	}

	// The opening side refuses with a bye, and Dial returns once the bye
	// is on its way.
	ca, cb := net.Pipe()
	ended := make(chan error, 1)
	go func() {
		theirs, err := wire.ReadHello(cb)
		if err != nil {
			ended <- err
			return
		}
		lb, err := side(idB, "devbox", 0).Accept(cb, theirs)
		if err != nil {
			ended <- err
			return
		}
		var ib inbox
		ended <- lb.Run(ib.handle)
	}()
	_, errA = refuse(side(idA, "laptop", 0)).Dial(ca)
	if !errors.As(errA, &r) || r.ByPeer || r.Peer.ID != idB {
		t.Fatalf("opener: %v", errA)
	}
	var bye *ByeError
	if err := <-ended; !errors.As(err, &bye) || bye.Bye.Membership == nil || bye.Bye.Membership.Removed == nil {
		t.Fatalf("accepter: %v", err)
	}

	// A node that reaches itself.
	_, errA, _, _ = shake(t, side(idA, "laptop", 0), side(idA, "laptop", 0))
	if errA == nil || !errors.As(errA, &r) {
		t.Fatalf("self: %v", errA)
	}
	// An invalid id.
	_, errA, _, _ = shake(t, side(idA, "laptop", 0), side("nope", "devbox", 0))
	if errA == nil {
		t.Fatal("accepted an invalid id")
	}
}

func TestAcceptingSideAnswersAfterCheck(t *testing.T) {
	gen := int64(0)
	b := side(idB, "devbox", 0)
	b.Membership = func() *wire.Membership { return &wire.Membership{Generation: gen} }
	b.Check = func(_ wire.NodeInfo, m wire.Membership) *Refusal {
		if m.Admit {
			gen = 4
		}
		return nil
	}
	a := side(idA, "laptop", 0)
	a.Membership = func() *wire.Membership { return &wire.Membership{Admit: true} }
	la, err, _, _ := shake(t, a, b)
	if err != nil || la.Membership.Generation != 4 {
		t.Fatalf("%v %+v", err, la)
	}
}
