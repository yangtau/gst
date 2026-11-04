package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// dialer is a fake Dial that records the addresses it is asked for and
// connects them to handle, or fails with err.
type dialer struct {
	mu     sync.Mutex
	dialed []string
	err    error
	handle func(net.Conn)
}

func (d *dialer) dial(_ context.Context, address string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, address)
	d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	// A TCP pair rather than net.Pipe, which cannot half-close.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		server, err := ln.Accept()
		ln.Close()
		if err != nil {
			return
		}
		defer server.Close()
		d.handle(server)
	}()
	return net.Dial("tcp", ln.Addr().String())
}

func (d *dialer) addresses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dialed...)
}

func echo(c net.Conn) { io.Copy(c, c) }

// start runs a proxy on a loopback listener and returns its address.
func start(t *testing.T, d *dialer) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	s := &Server{Dial: d.dial}
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v after cancellation", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	return ln.Addr().String()
}

func connect(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { c.Close() })
	return c
}

// socks greets the proxy, sends a CONNECT request for the address bytes
// (ATYP onward), and returns the connection and the reply code.
func socks(t *testing.T, addr string, address ...byte) (net.Conn, byte) {
	t.Helper()
	c := connect(t, addr)
	c.Write([]byte{5, 1, 0})
	var method [2]byte
	if _, err := io.ReadFull(c, method[:]); err != nil || method != [2]byte{5, 0} {
		t.Fatalf("method selection = %v, %v", method, err)
	}
	c.Write(append([]byte{5, 1, 0}, address...))
	return c, socksReplyOf(t, c)
}

func socksReplyOf(t *testing.T, c net.Conn) byte {
	t.Helper()
	var reply [10]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil || reply[0] != 5 || reply[3] != 1 {
		t.Fatalf("reply = %v, %v", reply, err)
	}
	return reply[1]
}

func exchange(t *testing.T, c net.Conn, text string) {
	t.Helper()
	if _, err := io.WriteString(c, text); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(text))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != text {
		t.Fatalf("echo = %q, %v; want %q", got, err, text)
	}
}

func TestSOCKSConnect(t *testing.T) {
	tests := []struct {
		name    string
		address []byte
		want    string
	}{
		{"domain", append([]byte{3, 15}, append([]byte("web.example.com"), 0, 80)...), "web.example.com:80"},
		{"IPv4", []byte{1, 192, 0, 2, 1, 0x1f, 0x90}, "192.0.2.1:8080"},
		{"IPv6", append(append([]byte{4, 0x20, 0x01, 0x0d, 0xb8}, make([]byte, 11)...), 1, 0x01, 0xbb), "[2001:db8::1]:443"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &dialer{handle: echo}
			c, code := socks(t, start(t, d), tt.address...)
			if code != 0 {
				t.Fatalf("reply code = %d", code)
			}
			exchange(t, c, "hello")
			if got := d.addresses(); len(got) != 1 || got[0] != tt.want {
				t.Fatalf("dialed %v, want %s", got, tt.want)
			}
		})
	}
}

func TestSOCKSHalfClose(t *testing.T) {
	d := &dialer{handle: func(c net.Conn) {
		data, _ := io.ReadAll(c)
		c.Write([]byte("reply:" + string(data)))
	}}
	c, code := socks(t, start(t, d), 1, 192, 0, 2, 1, 0, 80)
	if code != 0 {
		t.Fatalf("reply code = %d", code)
	}
	io.WriteString(c, "request")
	c.(*net.TCPConn).CloseWrite()
	if got, err := io.ReadAll(c); err != nil || string(got) != "reply:request" {
		t.Fatalf("read %q, %v", got, err)
	}
}

func TestSOCKSReplyCodes(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	tests := []struct {
		name string
		err  error
		want byte
	}{
		{"forbidden", wire.Errorf(wire.CodeForbidden, "no"), 2},
		{"wrapped forbidden", fmt.Errorf("relay: %w", wire.Errorf(wire.CodeForbidden, "no")), 2},
		{"not found", wire.Errorf(wire.CodeNotFound, "no machine"), 4},
		{"refused", refused, 5},
		{"wrapped refused", fmt.Errorf("dial: %w", refused), 5},
		{"failed call", wire.Errorf(wire.CodeFailed, "broke"), 1},
		{"generic", errors.New("boom"), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &dialer{err: tt.err}
			c, code := socks(t, start(t, d), 1, 192, 0, 2, 1, 0, 80)
			if code != tt.want {
				t.Fatalf("reply code = %d, want %d", code, tt.want)
			}
			if _, err := c.Read(make([]byte, 1)); err == nil {
				t.Error("connection stayed open after a failed reply")
			}
		})
	}
}

func TestSOCKSRefusals(t *testing.T) {
	tests := []struct {
		name    string
		hello   []byte
		request []byte // after the method selection; nil when the greeting is refused
		want    byte   // reply code
	}{
		{"no acceptable method", []byte{5, 1, 2}, nil, 0},
		{"BIND", []byte{5, 1, 0}, []byte{5, 2, 0, 1, 192, 0, 2, 1, 0, 80}, 7},
		{"UDP ASSOCIATE", []byte{5, 1, 0}, []byte{5, 3, 0, 1, 192, 0, 2, 1, 0, 80}, 7},
		{"unknown address type", []byte{5, 1, 0}, []byte{5, 1, 0, 9, 1, 2, 3, 4, 0, 80}, 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &dialer{handle: echo}
			c := connect(t, start(t, d))
			c.Write(tt.hello)
			var method [2]byte
			if _, err := io.ReadFull(c, method[:]); err != nil {
				t.Fatal(err)
			}
			if tt.request == nil {
				if method != [2]byte{5, 0xff} {
					t.Fatalf("method = %v, want no acceptable method", method)
				}
			} else {
				c.Write(tt.request)
				if code := socksReplyOf(t, c); code != tt.want {
					t.Fatalf("reply code = %d, want %d", code, tt.want)
				}
			}
			if _, err := c.Read(make([]byte, 1)); err == nil {
				t.Error("connection stayed open")
			}
			if got := d.addresses(); len(got) != 0 {
				t.Errorf("dialed %v for a refused request", got)
			}
		})
	}
}

func TestHTTPConnect(t *testing.T) {
	d := &dialer{handle: echo}
	c := connect(t, start(t, d))
	// Bytes that follow the request in the same write belong to the tunnel.
	io.WriteString(c, "CONNECT web.example.com:443 HTTP/1.1\r\nHost: web.example.com:443\r\n\r\nearly")
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("response = %v, %v", resp, err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(r, got); err != nil || string(got) != "early" {
		t.Fatalf("early bytes echoed as %q, %v", got, err)
	}
	io.WriteString(c, "later")
	if _, err := io.ReadFull(r, got); err != nil || string(got) != "later" {
		t.Fatalf("later bytes echoed as %q, %v", got, err)
	}
	if addrs := d.addresses(); len(addrs) != 1 || addrs[0] != "web.example.com:443" {
		t.Fatalf("dialed %v", addrs)
	}
}

func TestHTTPConnectDialError(t *testing.T) {
	d := &dialer{err: errors.New("no route to machine")}
	c := connect(t, start(t, d))
	io.WriteString(c, "CONNECT web.example.com:443 HTTP/1.1\r\nHost: web.example.com:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("response = %v, %v", resp, err)
	}
	if body, _ := io.ReadAll(resp.Body); !strings.Contains(string(body), "no route to machine") {
		t.Errorf("body = %q", body)
	}
}

func TestHTTPForward(t *testing.T) {
	requests := make(chan *http.Request, 1)
	d := &dialer{handle: func(c net.Conn) {
		req, err := http.ReadRequest(bufio.NewReader(c))
		if err != nil {
			return
		}
		requests <- req
		io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	}}
	addr := start(t, d)

	c := connect(t, addr)
	io.WriteString(c, "GET http://web.example.com/path?q=1 HTTP/1.1\r\nHost: web.example.com\r\n"+
		"Proxy-Connection: keep-alive\r\nProxy-Authorization: Basic eDp5\r\nKeep-Alive: 5\r\nConnection: keep-alive\r\n"+
		"X-Custom: yes\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("response = %v, %v", resp, err)
	}
	if body, _ := io.ReadAll(resp.Body); string(body) != "ok" {
		t.Errorf("body = %q", body)
	}
	req := <-requests
	if req.RequestURI != "/path?q=1" || req.Host != "web.example.com" || req.Header.Get("X-Custom") != "yes" {
		t.Errorf("forwarded %s %s Host=%s X-Custom=%q", req.Method, req.RequestURI, req.Host, req.Header.Get("X-Custom"))
	}
	for _, h := range []string{"Proxy-Connection", "Proxy-Authorization", "Keep-Alive"} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("hop-by-hop header %s = %q was forwarded", h, v)
		}
	}
	if !req.Close {
		t.Errorf("forwarded request lacks Connection: close (Connection = %q)", req.Header.Get("Connection"))
	}
	if got := d.addresses(); len(got) != 1 || got[0] != "web.example.com:80" {
		t.Errorf("dialed %v, want the default port 80", got)
	}

	c = connect(t, addr)
	io.WriteString(c, "GET http://web.example.com:8080/ HTTP/1.1\r\nHost: web.example.com:8080\r\n\r\n")
	io.Copy(io.Discard, c)
	if got := d.addresses(); len(got) != 2 || got[1] != "web.example.com:8080" {
		t.Errorf("dialed %v, want the URL's port", got)
	}
}

func TestHTTPForwardRefusals(t *testing.T) {
	tests := []struct {
		name    string
		request string
		status  int
	}{
		{"https URL", "GET https://web.example.com/ HTTP/1.1\r\nHost: web.example.com\r\n\r\n", 400},
		{"dial error", "GET http://web.example.com/ HTTP/1.1\r\nHost: web.example.com\r\n\r\n", 502},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &dialer{err: errors.New("unreachable")}
			c := connect(t, start(t, d))
			io.WriteString(c, tt.request)
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil || resp.StatusCode != tt.status {
				t.Fatalf("response = %v, %v; want %d", resp, err, tt.status)
			}
			if tt.status == 400 && len(d.addresses()) != 0 {
				t.Errorf("dialed %v", d.addresses())
			}
		})
	}
}

func TestOtherRequests(t *testing.T) {
	addr := start(t, &dialer{err: errors.New("unused")})
	tests := []struct {
		name, request string
		status        int
		contentType   string
		body          string
	}{
		{"root", "GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n", 400, "text/plain", "This is the gst proxy"},
		{"post", "POST /x HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: 0\r\n\r\n", 400, "text/plain", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := connect(t, addr)
			io.WriteString(c, tt.request)
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil || resp.StatusCode != tt.status || resp.Header.Get("Content-Type") != tt.contentType {
				t.Fatalf("response = %v, %v", resp, err)
			}
			if body, _ := io.ReadAll(resp.Body); !strings.Contains(string(body), tt.body) {
				t.Errorf("body = %q, want %q", body, tt.body)
			}
		})
	}

	// Something that is neither SOCKS nor HTTP is dropped.
	c := connect(t, addr)
	io.WriteString(c, "garbage\r\n\r\n")
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("garbage request: read error = %v, want EOF", err)
	}
}

func TestServeReturnsListenerError(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ln.Close()
	s := &Server{}
	if err := s.Serve(context.Background(), ln); err == nil {
		t.Fatal("Serve on a closed listener returned nil")
	}
}
