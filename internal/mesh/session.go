package mesh

import (
	"errors"
	"io"
	"net"
	"time"

	"github.com/hashicorp/yamux"
)

// session is what a link runs on: streams in both directions to the
// other node, each carrying one call. Streams end writing with CloseWrite,
// while Close also ends reading. When the session is lost, reading a
// stream fails rather than ending cleanly, so that the relay at each end
// closes the connection instead of waiting for the other direction, which
// would never end.
type session struct{ *yamux.Session }

func (s session) open() (net.Conn, error) {
	stream, err := s.OpenStream()
	if err != nil {
		return nil, err
	}
	return streamConn{stream, s.Session}, nil
}

func (s session) accept() (net.Conn, error) {
	stream, err := s.AcceptStream()
	if err != nil {
		return nil, err
	}
	return streamConn{stream, s.Session}, nil
}

// done is closed when the session ends.
func (s session) done() <-chan struct{} { return s.CloseChan() }

func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.MaxStreamWindowSize = 2 * 1024 * 1024
	cfg.LogOutput = io.Discard
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.ConnectionWriteTimeout = 30 * time.Second
	return cfg
}

// streamConn is a connection over a yamux stream.
type streamConn struct {
	*yamux.Stream
	session *yamux.Session
}

var errLinkLost = errors.New("the link closed")

func (c streamConn) Read(p []byte) (int, error) {
	n, err := c.Stream.Read(p)
	if err == io.EOF && c.session.IsClosed() {
		err = errLinkLost
	}
	return n, err
}

func (c streamConn) CloseWrite() error { return c.Stream.Close() }

func (c streamConn) Close() error {
	err := c.Stream.Close()
	c.Stream.SetReadDeadline(time.Now())
	return err
}
