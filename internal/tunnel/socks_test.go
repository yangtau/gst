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

func TestDynamicForwarding(t *testing.T) {
	logs := captureLog(t)
	s := startServer(t, serverOptions{})
	proxy := freeAddr(t)
	s.opts.Forwards = []config.Forward{{Direction: config.Dynamic, Listen: proxy}}
	cancel, result := runAsync(t, s.opts)
	await(t, logs.ready)

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
	if _, code := socksDial(t, proxy, binary.BigEndian.AppendUint16([]byte{1, 127, 0, 0, 1}, refused.Port())); code != socksConnectionRefused {
		t.Fatalf("refused target: reply code %d", code)
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
	for _, secret := range []string{"SECRET", proxy, echo.String(), "127.0.0.1", "localhost"} {
		if strings.Contains(text, secret) {
			t.Fatalf("runtime log leaked %q", secret)
		}
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
