package direct

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// Conn is a direct connection: streams in both directions over a QUIC
// connection. The stream interface of net.Conn applies, as well as
// CloseWrite, which ends writing while Close also ends reading. When the
// connection is lost, reading fails rather than ending cleanly.
type Conn struct {
	conn   *quic.Conn
	socket *socket
}

// Open opens a stream to the other side.
func (c *Conn) Open(ctx context.Context) (net.Conn, error) {
	s, err := c.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return &stream{Stream: s, conn: c.conn}, nil
}

// Accept waits for a stream that the other side opens.
func (c *Conn) Accept(ctx context.Context) (net.Conn, error) {
	s, err := c.conn.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return &stream{Stream: s, conn: c.conn}, nil
}

// Local returns the IP:port of this end.
func (c *Conn) Local() string { return c.conn.LocalAddr().String() }

// Remote returns the IP:port of the other end, as this one sees it.
func (c *Conn) Remote() string { return c.conn.RemoteAddr().String() }

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.conn.Context().Done() }

// WritePacket sends a datagram of another protocol to the other side, on
// the socket of the connection, which already has a way through the
// firewalls and NATs between the two. Its first byte must have the top
// two bits clear, which keeps it out of QUIC on the other side, and it
// must fit a UDP payload of 1200 bytes, which is all that a path is sure
// to carry: the socket does not fragment.
func (c *Conn) WritePacket(p []byte) error {
	_, err := c.socket.transport.WriteTo(p, c.conn.RemoteAddr())
	return err
}

// HandlePackets makes handle receive the datagrams that the other side
// sends with WritePacket, on the goroutine that reads the socket: it must
// not block or keep the packet. Packets are not authenticated, and probes
// of the attempt may trail in.
func (c *Conn) HandlePackets(handle func(packet []byte)) {
	c.socket.demux.handle.Store(&handle)
}

// Close ends the connection, telling the other side, and frees its socket.
func (c *Conn) Close() error {
	c.conn.CloseWithError(0, "")
	c.socket.close()
	return nil
}

// stream is a QUIC stream as a net.Conn. quic-go does not allow closing
// a stream while writing to it, which Close does when a relay breaks down
// or a context ends, so writing holds mu for reading, and closing fails
// the write in progress first.
type stream struct {
	*quic.Stream
	conn *quic.Conn
	mu   sync.RWMutex
}

func (s *stream) LocalAddr() net.Addr  { return s.conn.LocalAddr() }
func (s *stream) RemoteAddr() net.Addr { return s.conn.RemoteAddr() }

func (s *stream) Write(p []byte) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Stream.Write(p)
}

func (s *stream) CloseWrite() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Stream.Close()
}

func (s *stream) Close() error {
	s.Stream.CancelRead(0)
	s.Stream.SetWriteDeadline(time.Now())
	return s.CloseWrite()
}
