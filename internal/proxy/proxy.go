// Package proxy serves the gst proxy: SOCKS5 and HTTP proxy requests on
// one listener, told apart by their first byte.
package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/relay"
)

const (
	requestTimeout = 30 * time.Second // to read a client's request
	dialTimeout    = 45 * time.Second // to connect to its target
)

// Server answers proxy requests.
type Server struct {
	// Dial connects to address, a host:port that a client requested.
	Dial func(ctx context.Context, address string) (net.Conn, error)
}

// Serve answers the connections that listener accepts until ctx is done
// or listener fails.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	defer context.AfterFunc(ctx, func() { listener.Close() })()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.serve(ctx, conn)
	}
}

func (s *Server) serve(ctx context.Context, c net.Conn) {
	defer c.Close()
	defer context.AfterFunc(ctx, func() { c.Close() })()
	c.SetDeadline(time.Now().Add(requestTimeout))
	client := &bufferedConn{Conn: c, r: bufio.NewReader(c)}
	first, err := client.r.Peek(1)
	if err != nil {
		return
	}
	if first[0] == socksVersion {
		s.serveSOCKS(ctx, client)
	} else {
		s.serveHTTP(ctx, client)
	}
}

func (s *Server) dial(ctx context.Context, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	return s.Dial(ctx, address)
}

func (s *Server) serveSOCKS(ctx context.Context, c *bufferedConn) {
	address, err := socksRequest(c)
	if err != nil {
		return
	}
	c.SetDeadline(time.Time{})
	target, err := s.dial(ctx, address)
	if socksReply(c, socksReplyCode(err)) != nil || err != nil {
		if target != nil {
			target.Close()
		}
		return
	}
	relay.Pipe(c, target)
}

func (s *Server) serveHTTP(ctx context.Context, c *bufferedConn) {
	req, err := http.ReadRequest(c.r)
	if err != nil {
		return
	}
	switch {
	case req.Method == http.MethodConnect:
		target, err := s.dial(ctx, req.Host)
		if err != nil {
			respond(c, http.StatusBadGateway, "text/plain", []byte(err.Error()+"\n"))
			return
		}
		c.SetDeadline(time.Time{})
		if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			target.Close()
			return
		}
		relay.Pipe(c, target)
	case req.URL.IsAbs():
		s.forward(ctx, c, req)
	default:
		respond(c, http.StatusBadRequest, "text/plain", []byte("This is the gst proxy. Send it SOCKS5 or HTTP proxy requests.\n"))
	}
}

// forward passes on a plain HTTP request for an absolute URL and then
// the response, closing the connection afterwards.
func (s *Server) forward(ctx context.Context, c *bufferedConn, req *http.Request) {
	if req.URL.Scheme != "http" {
		respond(c, http.StatusBadRequest, "text/plain", []byte("only http:// URLs can be proxied without CONNECT\n"))
		return
	}
	address := req.URL.Host
	if req.URL.Port() == "" {
		address = net.JoinHostPort(req.URL.Hostname(), "80")
	}
	target, err := s.dial(ctx, address)
	if err != nil {
		respond(c, http.StatusBadGateway, "text/plain", []byte(err.Error()+"\n"))
		return
	}
	defer target.Close()
	c.SetDeadline(time.Time{})
	for _, h := range []string{"Proxy-Connection", "Proxy-Authorization", "Connection", "Keep-Alive"} {
		req.Header.Del(h)
	}
	req.Close = true // one request per connection
	go func() {
		if req.Write(target) != nil {
			target.Close()
		}
	}()
	io.Copy(c, target)
}

func respond(w io.Writer, code int, contentType string, body []byte) {
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n",
		code, http.StatusText(code), contentType, len(body))
	w.Write(body)
}

// bufferedConn is a client connection whose first bytes were read ahead.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}
