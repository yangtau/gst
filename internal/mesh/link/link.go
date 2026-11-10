// Package link runs the links of the mesh. A link is a connection to
// another machine's daemon, which one side opens over SSH by running
// "gst peer-bridge" there. After a handshake, an exchange of Hello frames
// in which each side says who it is, a link is an ordered, reliable pipe of
// typed frames in both directions: sync frames for the replicated table
// (internal/mesh/state), msg frames for routed messages
// (internal/mesh/route), and its own alive and bye frames. The package
// hands the bodies of sync and msg frames on without reading them.
package link

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Type is the type of a frame.
type Type byte

// Frame types.
const (
	Sync  Type = 1 // a batch of table entries, for the state layer
	Msg   Type = 2 // a routed message, for the routing layer
	Alive Type = 3 // nothing: it says that the other side is there
	Bye   Type = 4 // wire.Bye: the sender ends the link
)

const (
	// HandshakeTimeout bounds the start of the remote gst and its Hello.
	HandshakeTimeout = 30 * time.Second
	// MaxBody is the largest body of a frame.
	MaxBody = wire.MaxFrame

	headerSize = 5    // length (uint32) and type
	bulkQueue  = 1024 // frames that are not urgent, such as packets; more are dropped
	byeWait    = 3 * time.Second
)

// Timing is how a link finds that the other side is gone.
type Timing struct {
	Alive time.Duration // a side that sent nothing for this long sends an alive frame
	Dead  time.Duration // a link that received nothing for this long ends
}

// DefaultTiming is the timing of real links.
var DefaultTiming = Timing{Alive: 15 * time.Second, Dead: 45 * time.Second}

// Refusal is how a side refuses the other in the handshake.
type Refusal struct {
	Err    *wire.Error
	Member *wire.Member // sent along, such as the other's removal record
}

// RefusedError is a handshake that a side refused.
type RefusedError struct {
	Peer   *wire.NodeInfo // the other node, when it said who it is
	Err    *wire.Error
	Member *wire.Member // what the refusal carried
	ByPeer bool         // the other side refused; otherwise this one did
}

func (e *RefusedError) Error() string {
	if e.ByPeer {
		return "the other machine refused the link: " + e.Err.Message
	}
	return e.Err.Message
}

// Handshake is this node's side of handshakes.
type Handshake struct {
	Self wire.NodeInfo // ID, Name, and Software
	// Member returns this node's member field for its Hello. On the
	// accepting side it is asked after Check, which may change it.
	Member func() *wire.Member
	// Check judges the other node once its protocol and identity passed;
	// it returns nil to accept it.
	Check  func(peer wire.NodeInfo, m wire.Member) *Refusal
	Timing Timing
}

func (h Handshake) hello() wire.Hello {
	self := h.Self
	mine := wire.Hello{Software: self.Software, Role: wire.RolePeer, Node: &wire.NodeInfo{ID: self.ID, Name: self.Name}}
	if h.Member != nil {
		mine.Member = h.Member()
	}
	return mine
}

// judge checks the other side's Hello.
func (h Handshake) judge(theirs wire.Hello) (wire.NodeInfo, wire.Member, *Refusal) {
	var peer wire.NodeInfo
	var member wire.Member
	if theirs.Node != nil {
		peer = wire.NodeInfo{ID: theirs.Node.ID, Name: theirs.Node.Name, Software: theirs.Software}
	}
	if theirs.Member != nil {
		member = *theirs.Member
	}
	switch {
	case !config.ValidNodeID(peer.ID) || config.ValidateName(peer.Name) != nil:
		return peer, member, &Refusal{Err: wire.Errorf(wire.CodeInvalidParams, "the other machine sent no valid node id or name")}
	case peer.ID == h.Self.ID:
		return peer, member, &Refusal{Err: wire.Errorf(wire.CodeInvalidParams, "the other machine is this machine")}
	}
	if h.Check != nil {
		if r := h.Check(peer, member); r != nil {
			return peer, member, r
		}
	}
	return peer, member, nil
}

// Dial shakes hands on conn as the side that opened it, and returns the
// link, which Run then runs. It closes conn when it fails.
func (h Handshake) Dial(conn io.ReadWriteCloser) (*Link, error) {
	timeout := time.AfterFunc(HandshakeTimeout, func() { conn.Close() })
	theirs, err := wire.Dial(conn, h.hello())
	if !timeout.Stop() && err == nil {
		err = errors.New("the peer handshake timed out")
	}
	if err != nil {
		conn.Close()
		var refused *wire.Error
		if errors.As(err, &refused) {
			return nil, &RefusedError{Peer: theirs.Node, Err: refused, Member: theirs.Member, ByPeer: true}
		}
		return nil, err
	}
	peer, member, refusal := h.judge(theirs)
	if refusal != nil {
		// The other side already took this link; a bye tells it why not.
		// Dial returns once the bye is on its way, so that the caller
		// may close what carries conn.
		stop := time.AfterFunc(byeWait, func() { conn.Close() })
		writeBye(conn, wire.Bye{Error: refusal.Err, Member: refusal.Member})
		stop.Stop()
		conn.Close()
		return nil, &RefusedError{Peer: &peer, Err: refusal.Err, Member: refusal.Member}
	}
	return newLink(conn, peer, member, true, h.Timing), nil
}

// Accept answers theirs, the Hello that opened conn, as the side that
// accepted it, and returns the link, which Run then runs. It closes conn
// when it fails.
func (h Handshake) Accept(conn io.ReadWriteCloser, theirs wire.Hello) (*Link, error) {
	var peer wire.NodeInfo
	var member wire.Member
	var refusal *Refusal
	if theirs.Protocol >= wire.Protocol {
		peer, member, refusal = h.judge(theirs)
	}
	mine := h.hello()
	var refused *wire.Error
	if refusal != nil {
		mine.Member, refused = refusal.Member, refusal.Err
	}
	if err := wire.Answer(conn, theirs, mine, refused); err != nil {
		conn.Close()
		var e *wire.Error
		if errors.As(err, &e) {
			r := &RefusedError{Err: e}
			if peer.ID != "" {
				r.Peer = &peer
			}
			if refusal != nil {
				r.Member = refusal.Member
			}
			return nil, r
		}
		return nil, err
	}
	return newLink(conn, peer, member, false, h.Timing), nil
}

func writeBye(w io.Writer, bye wire.Bye) error {
	body, err := json.Marshal(bye)
	if err != nil {
		return err
	}
	_, err = w.Write(frame(Bye, body))
	return err
}

func frame(t Type, body []byte) []byte {
	f := make([]byte, headerSize+len(body))
	binary.BigEndian.PutUint32(f, uint32(len(body)))
	f[4] = byte(t)
	copy(f[headerSize:], body)
	return f
}

// ByeError is a link that the other side ended with a bye frame.
type ByeError struct{ Bye wire.Bye }

func (e *ByeError) Error() string {
	if e.Bye.Error == nil {
		return "the other machine ended the link"
	}
	return "the other machine ended the link: " + e.Bye.Error.Message
}

// ErrClosed is a link that this side closed.
var ErrClosed = errors.New("the link was closed")

// Link is a link after the handshake.
type Link struct {
	Peer   wire.NodeInfo
	Member wire.Member // the peer's, from its Hello
	Opened bool        // this side opened it

	conn   io.ReadWriteCloser
	timing Timing

	mu      sync.Mutex
	urgent  [][]byte // frames to send before any other
	closing bool
	reason  string // of this side's bye
	bye     []byte
	wake    chan struct{}
	bulk    chan []byte

	done     chan struct{}
	endOnce  sync.Once
	err      error
	lastRecv atomic.Int64
}

func newLink(conn io.ReadWriteCloser, peer wire.NodeInfo, member wire.Member, opened bool, t Timing) *Link {
	if t.Alive <= 0 {
		t.Alive = DefaultTiming.Alive
	}
	if t.Dead <= 0 {
		t.Dead = DefaultTiming.Dead
	}
	l := &Link{
		Peer: peer, Member: member, Opened: opened,
		conn: conn, timing: t,
		wake: make(chan struct{}, 1), bulk: make(chan []byte, bulkQueue), done: make(chan struct{}),
	}
	l.lastRecv.Store(time.Now().UnixNano())
	return l
}

// Send queues a frame and reports whether it did: it does not when the
// link ended, the body is too large, or, for a frame that is not urgent,
// the queue is full. Urgent frames go before the others, in order.
func (l *Link) Send(t Type, body []byte, urgent bool) bool {
	if len(body) > MaxBody {
		return false
	}
	select {
	case <-l.done:
		return false
	default:
	}
	f := frame(t, body)
	if !urgent {
		select {
		case l.bulk <- f:
			return true
		default:
			return false
		}
	}
	l.mu.Lock()
	if l.closing {
		l.mu.Unlock()
		return false
	}
	l.urgent = append(l.urgent, f)
	l.mu.Unlock()
	l.signal()
	return true
}

func (l *Link) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Close ends the link after the frames already queued as urgent, and
// bye, unless it is nil. It does not wait.
func (l *Link) Close(bye *wire.Bye) {
	l.mu.Lock()
	if l.closing {
		l.mu.Unlock()
		return
	}
	l.closing = true
	if bye != nil && bye.Error != nil {
		l.reason = bye.Error.Message
	}
	if bye != nil {
		if body, err := json.Marshal(bye); err == nil {
			l.bye = frame(Bye, body)
		}
	}
	l.mu.Unlock()
	l.signal()
	time.AfterFunc(byeWait, func() { l.end(l.closedErr()) })
}

func (l *Link) end(err error) {
	l.endOnce.Do(func() {
		l.mu.Lock()
		l.err = err
		l.mu.Unlock()
		close(l.done)
		l.conn.Close()
	})
}

// broken ends the link after its connection failed: as closed, when this
// side was closing it and the other side was quicker to hang up.
func (l *Link) broken() {
	l.mu.Lock()
	closing := l.closing
	l.mu.Unlock()
	if closing {
		l.end(l.closedErr())
		return
	}
	l.end(errors.New("the link closed"))
}

// closedErr is ErrClosed, with the reason that this side's bye gave.
func (l *Link) closedErr() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reason != "" {
		return fmt.Errorf("%w: %s", ErrClosed, l.reason)
	}
	return ErrClosed
}

// Err returns why the link ended, or nil while it runs.
func (l *Link) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// liveReader reads the link's connection, and counts any bytes that come
// as a sign of life, so that a large frame over a slow link keeps it.
type liveReader struct{ l *Link }

func (r liveReader) Read(p []byte) (int, error) {
	n, err := r.l.conn.Read(p)
	if n > 0 {
		r.l.lastRecv.Store(time.Now().UnixNano())
	}
	return n, err
}

// Run carries the link's frames until it ends, handing the bodies of the
// sync and msg frames that arrive to handle, which must not block for
// long. It closes the connection, and returns why the link ended:
// ErrClosed after Close, a *ByeError after the other side's bye, and
// another error after a failure.
func (l *Link) Run(handle func(t Type, body []byte)) error {
	go l.write()
	go l.watch()
	r := bufio.NewReaderSize(liveReader{l}, 64<<10)
	var header [headerSize]byte
	for {
		if _, err := io.ReadFull(r, header[:]); err != nil {
			l.broken()
			break
		}
		n := binary.BigEndian.Uint32(header[:4])
		if n > MaxBody {
			l.end(fmt.Errorf("a frame of %d bytes exceeds the limit", n))
			break
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			l.broken()
			break
		}
		switch t := Type(header[4]); t {
		case Sync, Msg:
			handle(t, body)
		case Bye:
			var bye wire.Bye
			json.Unmarshal(body, &bye)
			l.end(&ByeError{bye})
		}
		// Alive frames only count as received, and unknown types are
		// for later versions.
	}
	<-l.done
	return l.Err()
}

// next returns the next frame to send without waiting, and whether it is
// the last.
func (l *Link) next() ([]byte, bool) {
	l.mu.Lock()
	if len(l.urgent) > 0 {
		f := l.urgent[0]
		l.urgent[0] = nil
		l.urgent = l.urgent[1:]
		l.mu.Unlock()
		return f, false
	}
	if l.closing {
		bye := l.bye
		l.mu.Unlock()
		return bye, true
	}
	l.mu.Unlock()
	select {
	case f := <-l.bulk:
		return f, false
	default:
		return nil, false
	}
}

func (l *Link) write() {
	w := bufio.NewWriterSize(l.conn, 64<<10)
	alive := time.NewTimer(l.timing.Alive)
	defer alive.Stop()
	for {
		f, last := l.next()
		if f == nil && !last {
			if err := w.Flush(); err != nil {
				l.conn.Close() // the reader says why, as a bye may have come first
				return
			}
			select {
			case <-l.done:
				return
			case <-l.wake:
				continue
			case f = <-l.bulk:
			case <-alive.C:
				f = frame(Alive, nil)
			}
		}
		if f != nil {
			if _, err := w.Write(f); err != nil {
				l.conn.Close() // the reader says why, as a bye may have come first
				return
			}
			alive.Reset(l.timing.Alive)
		}
		if last {
			w.Flush()
			l.end(l.closedErr())
			return
		}
	}
}

// watch ends the link when nothing arrived for too long: the other side
// sends at least an alive frame in that time.
func (l *Link) watch() {
	tick := time.NewTicker(l.timing.Dead / 5)
	defer tick.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-tick.C:
			if time.Since(time.Unix(0, l.lastRecv.Load())) > l.timing.Dead {
				l.end(fmt.Errorf("nothing came from the other machine for %s", l.timing.Dead))
				return
			}
		}
	}
}
