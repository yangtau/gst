package relay

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// tcpPair returns the two ends of a loopback TCP connection.
func tcpPair(t *testing.T) (client, server *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan *net.TCPConn, 1)
	go func() {
		c, _ := ln.Accept()
		tc, _ := c.(*net.TCPConn)
		accepted <- tc
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s := <-accepted
	for _, c := range []*net.TCPConn{c.(*net.TCPConn), s} {
		c.SetDeadline(time.Now().Add(5 * time.Second))
		t.Cleanup(func() { c.Close() })
	}
	return c.(*net.TCPConn), s
}

func TestPipeHalfClose(t *testing.T) {
	// a1 <-> a2 ~ Pipe ~ b1 <-> b2: the test talks at a1 and b2.
	a1, a2 := tcpPair(t)
	b1, b2 := tcpPair(t)
	done := make(chan struct{})
	go func() {
		Pipe(a2, b1)
		close(done)
	}()

	// a1 sends a request and half-closes; b2 sees it and the end of it,
	// and can still answer.
	io.WriteString(a1, "request")
	a1.CloseWrite()
	if got, err := io.ReadAll(b2); err != nil || string(got) != "request" {
		t.Fatalf("b2 read %q, %v", got, err)
	}
	select {
	case <-done:
		t.Fatal("Pipe returned after one direction ended")
	case <-time.After(50 * time.Millisecond):
	}
	io.WriteString(b2, "response")
	b2.CloseWrite()
	if got, err := io.ReadAll(a1); err != nil || string(got) != "response" {
		t.Fatalf("a1 read %q, %v", got, err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Pipe did not return after both directions ended")
	}
	// Both connections are closed.
	for name, c := range map[string]*net.TCPConn{"a2": a2, "b1": b1} {
		if _, err := c.Write([]byte("x")); err == nil || !errors.Is(err, net.ErrClosed) {
			t.Errorf("%s still open after Pipe: write error %v", name, err)
		}
	}
}

func TestPipeCopiesBothWaysConcurrently(t *testing.T) {
	a1, a2 := tcpPair(t)
	b1, b2 := tcpPair(t)
	go Pipe(a2, b1)
	for _, step := range []struct {
		from, to net.Conn
		text     string
	}{{a1, b2, "ping"}, {b2, a1, "pong"}, {a1, b2, "again"}} {
		io.WriteString(step.from, step.text)
		got := make([]byte, len(step.text))
		if _, err := io.ReadFull(step.to, got); err != nil || string(got) != step.text {
			t.Fatalf("got %q, %v; want %q", got, err, step.text)
		}
	}
}

func TestPipeErrorClosesBoth(t *testing.T) {
	a1, a2 := tcpPair(t)
	b1, b2 := tcpPair(t)
	done := make(chan struct{})
	go func() {
		Pipe(a2, b1)
		close(done)
	}()
	// Resetting a1 makes the read from a2 fail, which must also end the
	// other direction even though b2 never closes.
	a1.SetLinger(0)
	a1.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Pipe did not return after a read error")
	}
	if _, err := io.ReadAll(b2); err != nil && !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("b2 read error %v", err)
	}
}

// noHalfClose is a connection whose write side cannot be closed alone.
type noHalfClose struct{ net.Conn }

func TestPipeWithoutHalfClose(t *testing.T) {
	// Without CloseWrite, the end of one direction closes the destination.
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	defer a1.Close()
	defer b2.Close()
	done := make(chan struct{})
	go func() {
		Pipe(noHalfClose{a2}, noHalfClose{b1})
		close(done)
	}()
	a1.Close()
	if _, err := io.ReadAll(b2); err != nil {
		t.Fatalf("b2 read: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Pipe did not return")
	}
}

func TestDialError(t *testing.T) {
	refused := os.NewSyscallError("connect", syscall.ECONNREFUSED)
	op := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 80}, Err: refused}
	plain := errors.New("plain")
	noCause := &net.OpError{Op: "dial"}
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"nil", nil, nil},
		{"OpError", op, refused},
		{"wrapped OpError", fmt.Errorf("dial: %w", op), refused},
		{"OpError without cause", noCause, noCause},
		{"other", plain, plain},
	}
	for _, tt := range tests {
		if got := DialError(tt.err); got != tt.want {
			t.Errorf("%s: DialError(%v) = %v, want %v", tt.name, tt.err, got, tt.want)
		}
	}
	if msg := DialError(op).Error(); msg != "connect: connection refused" {
		t.Errorf("message = %q", msg)
	}
}
