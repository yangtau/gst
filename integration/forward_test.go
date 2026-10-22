//go:build integration

package integration

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

func TestForwardsConcurrentTrafficOnAllMappings(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	target := startEchoTarget(t)
	tunnel := startTunnel(t, s.config(false,
		config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: target},
		config.Forward{Direction: config.Remote, Listen: "0.0.0.0:3000", Target: target},
	))
	tunnel.waitReady(t)

	remote := []string{s.hostAddr(2222), s.hostAddr(3000)}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			payload := strings.Repeat(fmt.Sprintf("payload-%d/", i), 50_000)
			if err := echoRoundTrip(remote[i%2], payload); err != nil {
				t.Errorf("connection %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	logs := tunnel.logs()
	for _, secret := range []string{s.addr, target, s.identity, s.knownHosts} {
		if strings.Contains(logs, secret) {
			t.Errorf("log reveals %q", secret)
		}
	}
}

// The local forward targets sshd inside the container, so its banner proves
// that the server dialed the target.
func TestForwardsBothDirections(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	target := startEchoTarget(t)
	local := unusedAddr(t)
	tunnel := startTunnel(t, s.config(false,
		config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: target},
		config.Forward{Direction: config.Local, Listen: local, Target: "127.0.0.1:22"},
	))
	tunnel.waitReady(t)

	if err := echoRoundTrip(s.hostAddr(2222), "remote"); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", local, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	banner, err := bufio.NewReader(conn).ReadString('\n')
	conn.Close()
	if !strings.HasPrefix(banner, "SSH-2.0-") {
		t.Fatalf("no sshd banner through the local forward: %v", err)
	}

	tunnel.signal(t, syscall.SIGTERM)
	if code, _ := tunnel.waitExit(t, 5*time.Second); code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	if conn, err := net.DialTimeout("tcp", local, time.Second); err == nil {
		conn.Close()
		t.Fatal("local listener survived shutdown")
	}
}

// The dynamic forward requests "localhost" by name, so its sshd banner
// proves that the server resolved and dialed the target.
func TestDynamicForward(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	proxy := unusedAddr(t)
	tunnel := startTunnel(t, s.config(false, config.Forward{Direction: config.Dynamic, Listen: proxy}))
	tunnel.waitReady(t)

	conn := dialSOCKSLocalhost(t, proxy, 22)
	if banner, err := bufio.NewReader(conn).ReadString('\n'); !strings.HasPrefix(banner, "SSH-2.0-") {
		t.Fatalf("no sshd banner through the dynamic forward: %v", err)
	}
	if logs := tunnel.logs(); strings.Contains(logs, proxy) || strings.Contains(logs, "localhost") {
		t.Fatalf("log reveals forward addresses:\n%s", logs)
	}
}

// The target only listens on the host's loopback, so a reply proves that
// gst, not sshd in the container, resolved and dialed localhost.
func TestReverseDynamicForward(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	target := startEchoTarget(t)
	tunnel := startTunnel(t, s.config(false, config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222"}))
	tunnel.waitReady(t)

	conn := dialSOCKSLocalhost(t, s.hostAddr(2222), netip.MustParseAddrPort(target).Port())
	if _, err := io.WriteString(conn, "reverse SOCKS"); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(conn); err != nil || string(got) != "reply:reverse SOCKS" {
		t.Fatalf("reply %q: %v", got, err)
	}
	if logs := tunnel.logs(); strings.Contains(logs, target) || strings.Contains(logs, "localhost") {
		t.Fatalf("log reveals forward addresses:\n%s", logs)
	}
	tunnel.signal(t, syscall.SIGTERM)
	if code, _ := tunnel.waitExit(t, 5*time.Second); code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	eventually(t, 5*time.Second, "reverse SOCKS listener survived shutdown", func() bool {
		return len(s.listenerOwners(2222)) == 0
	})
}

func dialSOCKSLocalhost(t *testing.T, proxy string, port uint16) *net.TCPConn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxy, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	request := append([]byte{5, 1, 0, 5, 1, 0, 3, 9}, "localhost"...)
	request = binary.BigEndian.AppendUint16(request, port)
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 12)
	if _, err := io.ReadFull(conn, reply); err != nil || reply[1] != 0 || reply[3] != 0 {
		t.Fatalf("SOCKS reply %v: %v", reply, err)
	}
	return conn.(*net.TCPConn)
}

func TestRefusedTargetClosesOnlyThatConnection(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	target := startEchoTarget(t)
	tunnel := startTunnel(t, s.config(false,
		config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: unusedAddr(t)},
		config.Forward{Direction: config.Remote, Listen: "0.0.0.0:3000", Target: target},
	))
	tunnel.waitReady(t)

	if err := echoRoundTrip(s.hostAddr(2222), "hello"); err == nil {
		t.Fatal("refused target produced a reply")
	}
	eventually(t, 5*time.Second, "missing target failure log", func() bool {
		return strings.Contains(tunnel.logs(), "target connection failed for forward 1")
	})
	if err := echoRoundTrip(s.hostAddr(3000), "still working"); err != nil {
		t.Fatalf("healthy forward broken: %v", err)
	}
	if !tunnel.running() {
		t.Fatal("tunnel exited after a target failure")
	}
}

func TestSIGTERMExitsCleanlyAndReleasesRemotePorts(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	target := startEchoTarget(t)
	tunnel := startTunnel(t, s.config(false,
		config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: target},
		config.Forward{Direction: config.Remote, Listen: "0.0.0.0:3000", Target: target},
	))
	tunnel.waitReady(t)
	for _, port := range forwardPorts {
		if len(s.listenerOwners(port)) == 0 {
			t.Fatalf("port %d not listening while tunnel is ready", port)
		}
	}

	tunnel.signal(t, syscall.SIGTERM)
	if code, elapsed := tunnel.waitExit(t, 5*time.Second); code != 0 {
		t.Fatalf("exit code %d after %v, want 0", code, elapsed)
	}
	for _, port := range forwardPorts {
		eventually(t, 5*time.Second, fmt.Sprintf("port %d still listening", port), func() bool {
			return len(s.listenerOwners(port)) == 0
		})
	}
}
