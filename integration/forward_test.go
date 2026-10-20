//go:build integration

package integration

import (
	"bufio"
	"fmt"
	"net"
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
