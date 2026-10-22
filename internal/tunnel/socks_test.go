package tunnel

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

func TestSOCKSRequest(t *testing.T) {
	greeting := []byte{5, 2, 2, 0}
	accepted := []byte{5, 0}
	reply := func(code byte) []byte { return []byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0} }
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	tests := map[string]struct {
		input  []byte
		target string // empty when the request fails
		output []byte
	}{
		"ipv4":      {cat(greeting, []byte{5, 1, 0, 1, 192, 0, 2, 1, 0, 80}), "192.0.2.1:80", accepted},
		"ipv6":      {cat(greeting, []byte{5, 1, 0, 4}, netip.IPv6Loopback().AsSlice(), []byte{1, 187}), "[::1]:443", accepted},
		"domain":    {cat(greeting, []byte{5, 1, 0, 3, 15}, []byte("ssh.example.com"), []byte{0, 22}), "ssh.example.com:22", accepted},
		"auth only": {[]byte{5, 1, 2}, "", []byte{5, 0xff}},
		"socks4":    {[]byte{4, 1, 0, 80, 192, 0, 2, 1, 0}, "", nil},
		"bind":      {cat(greeting, []byte{5, 2, 0, 1, 192, 0, 2, 1, 0, 80}), "", cat(accepted, reply(7))},
		"udp":       {cat(greeting, []byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}), "", cat(accepted, reply(7))},
		"bad type":  {cat(greeting, []byte{5, 1, 0, 9}), "", cat(accepted, reply(8))},
		"truncated": {cat(greeting, []byte{5, 1, 0, 3, 15, 'a'}), "", accepted},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			client, server := tcpPair(t)
			client.Write(test.input)
			client.(*net.TCPConn).CloseWrite()
			target, err := socksRequest(server)
			server.Close()
			if target != test.target || (err == nil) != (test.target != "") {
				t.Fatalf("socksRequest = %q, %v; want %q", target, err, test.target)
			}
			if output, _ := io.ReadAll(client); !bytes.Equal(output, test.output) {
				t.Fatalf("replied %v, want %v", output, test.output)
			}
		})
	}
}

func TestSOCKSForwarding(t *testing.T) {
	for _, direction := range []string{config.Dynamic, config.Remote} {
		t.Run(direction, func(t *testing.T) {
			logs := captureLog(t)
			s := startServer(t, serverOptions{rejectDirect: direction == config.Remote})
			proxy := freeAddr(t)
			s.opts.Forwards = []config.Forward{{Direction: direction, Listen: proxy}}
			cancel, result := runAsync(t, s.opts)
			await(t, logs.ready)
			if direction == config.Remote {
				proxy = await(t, s.forwards).Addr().String()
			}

			echo := netip.MustParseAddrPort(startEchoTarget(t))
			port := binary.BigEndian.AppendUint16(nil, echo.Port())
			for name, address := range map[string][]byte{
				"ipv4":   append([]byte{1, 127, 0, 0, 1}, port...),
				"domain": append([]byte{3, 9, 'l', 'o', 'c', 'a', 'l', 'h', 'o', 's', 't'}, port...),
			} {
				t.Run(name, func(t *testing.T) {
					c, code := socksDial(t, proxy, address)
					if code != socksSucceeded {
						t.Fatalf("reply code %d", code)
					}
					message := strings.Repeat("payload/", 20000)
					io.WriteString(c, message)
					c.(*net.TCPConn).CloseWrite()
					if reply, err := io.ReadAll(c); err != nil || string(reply) != "reply:"+message {
						t.Fatal("relay through SOCKS failed", err)
					}
				})
			}

			refused := netip.MustParseAddrPort(freeAddr(t))
			failed, code := socksDial(t, proxy, binary.BigEndian.AppendUint16([]byte{1, 127, 0, 0, 1}, refused.Port()))
			if code != socksConnectionRefused {
				t.Fatalf("refused target: reply code %d", code)
			}
			if reply, err := io.ReadAll(failed); err != nil || len(reply) != 0 {
				t.Fatalf("failed connection not closed: %q, %v", reply, err)
			}
			eventually(t, "missing target failure log", func() bool {
				return strings.Contains(logs.String(), "target connection failed for forward 1")
			})

			c, err := net.DialTimeout("tcp", proxy, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			c.Write([]byte{4, 1})
			c.Close()
			eventually(t, "missing SOCKS failure log", func() bool {
				return strings.Contains(logs.String(), "SOCKS request failed for forward 1")
			})

			cancel()
			if err := await(t, result); err != nil {
				t.Fatal(err)
			}
			text := logs.String()
			for _, secret := range []string{"SECRET", proxy, echo.String(), "127.0.0.1", "localhost", s.opts.Server, s.opts.User, s.opts.IdentityFiles[0], s.opts.KnownHostsFiles[0], s.opts.Forwards[0].Listen} {
				if strings.Contains(text, secret) {
					t.Fatalf("runtime log leaked %q", secret)
				}
			}
		})
	}
}

func TestReverseSOCKSIPv6(t *testing.T) {
	target, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	serveEcho(t, target)
	logs := captureLog(t)
	s := startServer(t, serverOptions{rejectDirect: true})
	s.opts.Forwards[0].Target = ""
	cancel, result, remote := startTunnel(t, s, logs)
	address := append([]byte{socksIPv6}, netip.IPv6Loopback().AsSlice()...)
	address = binary.BigEndian.AppendUint16(address, uint16(target.Addr().(*net.TCPAddr).Port))
	c, code := socksDial(t, remote.Addr().String(), address)
	if code != socksSucceeded {
		t.Fatalf("reply code %d", code)
	}
	io.WriteString(c, "IPv6 payload")
	c.(*net.TCPConn).CloseWrite()
	if reply, err := io.ReadAll(c); err != nil || string(reply) != "reply:IPv6 payload" {
		t.Fatal("IPv6 relay failed", err)
	}
	cancel()
	if err := await(t, result); err != nil {
		t.Fatal(err)
	}
}

func TestReverseSOCKSInvalidRequests(t *testing.T) {
	logs := captureLog(t)
	s := startServer(t, serverOptions{})
	s.opts.Forwards[0].Target = ""
	cancel, result, remote := startTunnel(t, s, logs)
	for name, test := range map[string]struct {
		request []byte
		reply   []byte
	}{
		"version":   {[]byte{4, 1}, nil},
		"auth":      {[]byte{5, 1, 2}, []byte{5, 0xff}},
		"command":   {[]byte{5, 1, 0, 5, 2, 0, 1, 127, 0, 0, 1, 0, 80}, []byte{5, 0, 5, 7, 0, 1, 0, 0, 0, 0, 0, 0}},
		"address":   {[]byte{5, 1, 0, 5, 1, 0, 9}, []byte{5, 0, 5, 8, 0, 1, 0, 0, 0, 0, 0, 0}},
		"truncated": {[]byte{5, 1, 0, 5, 1, 0, 3, 15, 'a'}, []byte{5, 0}},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := net.DialTimeout("tcp", remote.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(time.Second))
			c.Write(test.request)
			c.(*net.TCPConn).CloseWrite()
			if reply, err := io.ReadAll(c); err != nil || !bytes.Equal(reply, test.reply) {
				t.Fatalf("reply %v, %v; want %v", reply, err, test.reply)
			}
		})
	}
	// A failed request must not stop the listener or the tunnel.
	echo := netip.MustParseAddrPort(startEchoTarget(t))
	if _, code := socksDial(t, remote.Addr().String(), binary.BigEndian.AppendUint16([]byte{1, 127, 0, 0, 1}, echo.Port())); code != socksSucceeded {
		t.Fatalf("reply code after invalid requests: %d", code)
	}
	cancel()
	if err := await(t, result); err != nil {
		t.Fatal(err)
	}
}

func TestReverseSOCKSHandshakeTimeout(t *testing.T) {
	logs := captureLog(t)
	s := startServer(t, serverOptions{})
	s.opts.ConnectTimeout = 250 * time.Millisecond
	s.opts.Forwards[0].Target = ""
	cancel, result, remote := startTunnel(t, s, logs)
	var stalled []net.Conn
	for _, request := range [][]byte{{}, {5, 1, 0, 5, 1, 0, 3, 15, 'a'}} {
		c, err := net.DialTimeout("tcp", remote.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(2 * time.Second))
		c.Write(request)
		stalled = append(stalled, c)
	}
	for _, c := range stalled {
		if _, err := io.ReadAll(c); err != nil {
			t.Fatalf("stalled handshake was not closed: %v", err)
		}
	}
	eventually(t, "missing handshake timeout log", func() bool {
		return strings.Contains(logs.String(), "SOCKS request failed for forward 1")
	})

	// The handshake timer must be stopped before relaying a successful request.
	echo := netip.MustParseAddrPort(startEchoTarget(t))
	c, code := socksDial(t, remote.Addr().String(), binary.BigEndian.AppendUint16([]byte{1, 127, 0, 0, 1}, echo.Port()))
	if code != socksSucceeded {
		t.Fatalf("reply code %d", code)
	}
	time.Sleep(2 * s.opts.ConnectTimeout)
	io.WriteString(c, "after handshake timeout")
	c.(*net.TCPConn).CloseWrite()
	if reply, err := io.ReadAll(c); err != nil || string(reply) != "reply:after handshake timeout" {
		t.Fatal("handshake timeout affected relay", err)
	}
	cancel()
	if err := await(t, result); err != nil {
		t.Fatal(err)
	}
}

func TestReverseSOCKSShutdown(t *testing.T) {
	for _, reason := range []string{"cancel", "disconnect"} {
		t.Run(reason, func(t *testing.T) {
			logs := captureLog(t)
			s := startServer(t, serverOptions{rejectDirect: true})
			s.opts.Forwards[0].Target = ""
			cancel, result, remote := startTunnel(t, s, logs)
			target, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			port := uint16(target.Addr().(*net.TCPAddr).Port)
			active, code := socksDial(t, remote.Addr().String(), binary.BigEndian.AppendUint16([]byte{1, 127, 0, 0, 1}, port))
			if code != socksSucceeded {
				t.Fatalf("reply code %d", code)
			}
			outgoing, err := target.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer outgoing.Close()
			stalled, err := net.DialTimeout("tcp", remote.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer stalled.Close()
			stalled.SetDeadline(time.Now().Add(time.Second))
			stalled.Write([]byte{5, 1, 0})
			var greeting [2]byte
			if _, err := io.ReadFull(stalled, greeting[:]); err != nil || greeting != [2]byte{5, 0} {
				t.Fatalf("greeting: %v, %v", greeting, err)
			}
			if reason == "cancel" {
				cancel()
			} else {
				s.cancel()
			}
			err = await(t, result)
			if reason == "cancel" && err != nil {
				t.Fatal(err)
			}
			if reason == "disconnect" && (err == nil || err.Error() != "SSH connection closed") {
				t.Fatalf("unexpected disconnect result: %v", err)
			}
			for _, c := range []net.Conn{active, outgoing, stalled} {
				c.SetReadDeadline(time.Now().Add(time.Second))
				if _, err := c.Read(make([]byte, 1)); err == nil {
					t.Fatal("connection survived shutdown")
				} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
					t.Fatal("connection close timed out")
				}
			}
			eventually(t, "remote listener survived shutdown", func() bool {
				c, err := net.DialTimeout("tcp", remote.Addr().String(), 100*time.Millisecond)
				if err != nil {
					return true
				}
				c.Close()
				return false
			})
		})
	}
}

// socksDial sends a CONNECT request for address (ATYP, address, port)
// to the SOCKS5 proxy and returns the connection and the reply code.
func socksDial(t *testing.T, proxy string, address []byte) (net.Conn, byte) {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxy, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write(append([]byte{5, 1, 0, 5, 1, 0}, address...))
	reply := make([]byte, 12)
	if _, err := io.ReadFull(c, reply); err != nil || reply[0] != 5 || reply[1] != 0 || reply[2] != 5 {
		t.Fatalf("bad SOCKS reply %v: %v", reply, err)
	}
	return c, reply[3]
}

// tcpPair returns both ends of a loopback TCP connection.
func tcpPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err = ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	for _, c := range []net.Conn{client, server} {
		c.SetDeadline(time.Now().Add(3 * time.Second))
	}
	return client, server
}

func TestReverseSOCKSListenerFailure(t *testing.T) {
	logs := captureLog(t)
	s := startServer(t, serverOptions{rejectForward: 1})
	s.opts.Forwards[0].Target = ""
	_, result := runAsync(t, s.opts)
	if err := await(t, result); err == nil || err.Error() != "listener start failed for forward 1" {
		t.Fatalf("unexpected startup error: %v", err)
	}
	if strings.Contains(logs.String(), "tunnel ready") {
		t.Fatal("tunnel ready despite listener failure")
	}
}
