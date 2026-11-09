package expose

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestStreams(t *testing.T) {
	for _, network := range []string{"tcp", "unix"} {
		t.Run(network, func(t *testing.T) {
			address := "127.0.0.1:0"
			if network == "unix" {
				dir, err := os.MkdirTemp("/tmp", "gst-expose-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.RemoveAll(dir) })
				address = filepath.Join(dir, "socket")
			}
			backend, err := net.Listen(network, address)
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			go func() {
				for {
					c, err := backend.Accept()
					if err != nil {
						return
					}
					go func() { defer c.Close(); p, _ := io.ReadAll(c); c.Write(append([]byte("reply:"), p...)) }()
				}
			}()
			rule, err := config.ParseExpose(network+"://"+backend.Addr().String(), freePort(t), false)
			if err != nil {
				t.Fatal(err)
			}
			var m Manager
			defer m.Close()
			m.Apply(t.Context(), "", []config.Expose{rule})
			if m.Status()[0].State != "waiting" {
				t.Fatal(m.Status())
			}
			m.Apply(t.Context(), "127.0.0.1", []config.Expose{rule})
			state := m.Status()[0]
			if state.State != "running" {
				t.Fatal(state)
			}
			c, err := net.Dial("tcp", state.Address)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(3 * time.Second))
			io.WriteString(c, "hello")
			// An identical reload must preserve existing connections.
			m.Apply(t.Context(), "127.0.0.1", []config.Expose{rule})
			c.(*net.TCPConn).CloseWrite()
			p, err := io.ReadAll(c)
			if err != nil || string(p) != "reply:hello" {
				t.Fatalf("half-close reply %q: %v", p, err)
			}
			active, err := net.Dial("tcp", state.Address)
			if err != nil {
				t.Fatal(err)
			}
			defer active.Close()
			active.SetDeadline(time.Now().Add(3 * time.Second))
			active.Write([]byte("pending"))
			m.Apply(t.Context(), "", []config.Expose{rule})
			var b [1]byte
			if _, err := active.Read(b[:]); err == nil {
				t.Fatal("network down kept connection open")
			} else if e, ok := err.(net.Error); ok && e.Timeout() {
				t.Fatal("connection was not closed")
			}
			if c, err := net.DialTimeout("tcp", state.Address, 100*time.Millisecond); err == nil {
				c.Close()
				t.Fatal("listener survived network down")
			}
			m.Apply(t.Context(), "127.0.0.1", []config.Expose{rule})
			if m.Status()[0].State != "running" {
				t.Fatal(m.Status())
			}
			m.Apply(t.Context(), "127.0.0.1", nil)
			if len(m.Status()) != 0 {
				t.Fatal(m.Status())
			}
		})
	}
}

func TestBindFailureAndRecovery(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	rule, _ := config.ParseExpose("8080", occupied.Addr().(*net.TCPAddr).Port, false)
	var m Manager
	defer m.Close()
	for _, address := range []string{"0.0.0.0", "::", "127.0.0.1"} {
		m.Apply(t.Context(), address, []config.Expose{rule})
		if s := m.Status()[0]; s.State != "failed" || s.Error == "" {
			t.Fatal(s)
		}
	}
	occupied.Close()
	m.Apply(t.Context(), "127.0.0.1", []config.Expose{rule})
	if m.Status()[0].State != "running" {
		t.Fatal(m.Status())
	}
	// An unassigned new address closes the old listener even if rebinding fails.
	old := m.Status()[0].Address
	m.Apply(t.Context(), "192.0.2.123", []config.Expose{rule})
	if c, err := net.DialTimeout("tcp", old, 100*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("old listener survived address change")
	}
}

func TestUDPClients(t *testing.T) {
	backend, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		b := make([]byte, 65535)
		for {
			n, from, err := backend.ReadFrom(b)
			if err != nil {
				return
			}
			backend.WriteTo([]byte(from.String()+"|"+string(b[:n])), from)
		}
	}()
	rule, _ := config.ParseExpose("udp://"+backend.LocalAddr().String(), freePort(t), false)
	tcpRule, _ := config.ParseExpose("8080", rule.Port, false)
	var m Manager
	defer m.Close()
	m.Apply(t.Context(), "127.0.0.1", []config.Expose{rule, tcpRule})
	for _, s := range m.Status() {
		if s.State != "running" {
			t.Fatal(s)
		}
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(rule.Port))
	clients := make([]net.Conn, 2)
	for i := range clients {
		clients[i], err = net.Dial("udp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer clients[i].Close()
		clients[i].SetDeadline(time.Now().Add(3 * time.Second))
	}
	sources := map[string]bool{}
	for i, c := range clients {
		for _, payload := range []string{fmt.Sprint(i), "", strings.Repeat("x", 8192)} {
			c.Write([]byte(payload))
			b := make([]byte, 65535)
			n, err := c.Read(b)
			if err != nil {
				t.Fatal(err)
			}
			source, got, _ := strings.Cut(string(b[:n]), "|")
			if got != payload {
				t.Fatal("datagram changed")
			}
			sources[source] = true
		}
	}
	if len(sources) != 2 {
		t.Fatalf("clients shared a socket: %v", sources)
	}
	m.Apply(t.Context(), "127.0.0.1", []config.Expose{tcpRule})
	// UDP can be rebound immediately after its rule is removed; TCP remains.
	replacement, err := net.ListenPacket("udp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	replacement.Close()
	if m.Status()[0].State != "running" {
		t.Fatal(m.Status())
	}
}

func TestUDPIdle(t *testing.T) {
	backend, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	rule, _ := config.ParseExpose("udp://"+backend.LocalAddr().String(), 5999, false)
	queue := make(chan []byte, 1)
	queue <- []byte("hello")
	done := make(chan struct{})
	go func() {
		defer close(done)
		udpSession(context.Background(), socket, socket.LocalAddr(), rule, queue, 50*time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("idle session did not expire")
	}
}
