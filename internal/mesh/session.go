package mesh

import (
	"context"
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
type session interface {
	Open(ctx context.Context) (net.Conn, error)
	Accept(ctx context.Context) (net.Conn, error)
	Close() error
	Done() <-chan struct{} // closed when the session ends
}

// closed reports whether s has ended.
func closed(s session) bool {
	select {
	case <-s.Done():
		return true
	default:
		return false
	}
}

// yamuxSession is the session of a link over SSH.
type yamuxSession struct{ *yamux.Session }

func (s yamuxSession) Open(context.Context) (net.Conn, error) {
	stream, err := s.Session.OpenStream()
	if err != nil {
		return nil, err
	}
	return streamConn{stream, s.Session}, nil
}

func (s yamuxSession) Accept(context.Context) (net.Conn, error) {
	stream, err := s.Session.AcceptStream()
	if err != nil {
		return nil, err
	}
	return streamConn{stream, s.Session}, nil
}

func (s yamuxSession) Done() <-chan struct{} { return s.Session.CloseChan() }

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
