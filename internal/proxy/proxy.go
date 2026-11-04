// Package proxy serves the gst proxy: SOCKS5 and HTTP proxy requests on
// one listener, told apart by their first byte, and the proxy
// auto-configuration (PAC) file that tells browsers which hosts to send
// through it.
package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
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
	// PAC returns the proxy auto-configuration file.
	PAC func() []byte
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
	case req.Method == http.MethodGet && req.URL.Path == "/proxy.pac":
		respond(c, http.StatusOK, "application/x-ns-proxy-autoconfig", s.PAC())
	default:
		respond(c, http.StatusBadRequest, "text/plain", []byte("This is the gst proxy. Send it proxy requests, or get its configuration at /proxy.pac.\n"))
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

// PAC returns a proxy auto-configuration file that sends the hosts under
// .gst and those that routes match to the proxy at listen, an HTTP proxy
// to browsers, and the others directly. IPv4 prefixes match IP addresses
// only; IPv6 prefixes are left out.
func PAC(listen string, routes config.Routes) []byte {
	if ap, err := netip.ParseAddrPort(listen); err == nil && ap.Addr().IsUnspecified() {
		listen = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), ap.Port()).String()
	}
	q := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "// gst proxy auto-configuration\nfunction FindProxyForURL(url, host) {\n")
	fmt.Fprintf(&b, "  var proxy = %s;\n  host = host.toLowerCase();\n", q("PROXY "+listen))
	fmt.Fprintf(&b, "  var ipv4 = /^\\d+\\.\\d+\\.\\d+\\.\\d+$/.test(host);\n")
	fmt.Fprintf(&b, "  if (dnsDomainIs(host, \".gst\")) return proxy;\n")
	fallback := `"DIRECT"`
	for _, r := range routes {
		prefix, err := netip.ParsePrefix(r.Match)
		if err != nil {
			if addr, err := netip.ParseAddr(r.Match); err == nil {
				prefix = netip.PrefixFrom(addr, addr.BitLen())
			}
		}
		switch {
		case r.Match == config.DefaultRoute:
			fallback = "proxy"
		case prefix.IsValid() && prefix.Addr().Is4():
			mask := net.CIDRMask(prefix.Bits(), 32)
			fmt.Fprintf(&b, "  if (ipv4 && isInNet(host, %s, %s)) return proxy;\n", q(prefix.Addr().String()), q(net.IP(mask).String()))
		case !prefix.IsValid():
			fmt.Fprintf(&b, "  if (host == %s || dnsDomainIs(host, %s)) return proxy;\n", q(r.Match), q("."+r.Match))
		}
	}
	fmt.Fprintf(&b, "  return %s;\n}\n", fallback)
	return []byte(b.String())
}
