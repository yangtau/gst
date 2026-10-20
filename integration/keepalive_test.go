//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

// The server configuration uses a 1 s keepalive interval and a 2 s timeout.

func TestHealthyTunnelSurvivesManyKeepalives(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	target := startEchoTarget(t)
	tunnel := startTunnel(t, s.config(false, config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: target}))
	tunnel.waitReady(t)

	time.Sleep(5 * time.Second)
	if !tunnel.running() {
		t.Fatal("healthy tunnel exited")
	}
	if err := echoRoundTrip(s.hostAddr(2222), "after keepalives"); err != nil {
		t.Fatal(err)
	}
}

func TestExitsWhenServerFreezes(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	tunnel := startTunnel(t, s.config(false, config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: unusedAddr(t)}))
	tunnel.waitReady(t)

	// SIGSTOP keeps the TCP connection open in the kernel, so only the SSH
	// keepalive can notice that sshd stopped answering.
	s.signalSessions("STOP")
	code, elapsed := tunnel.waitExit(t, 10*time.Second)
	if code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if !strings.Contains(tunnel.logs(), "SSH keepalive timed out") {
		t.Fatal("exit was not caused by the keepalive timeout")
	}
	// The first unanswered probe is sent after the freeze and needs the full
	// 2 s timeout; the next tick adds at most 1 s on top.
	if elapsed < 2*time.Second || elapsed > 5*time.Second {
		t.Fatalf("exited after %v, want 2-5 s", elapsed)
	}
}

func TestExitsWhenServerSessionIsKilled(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	tunnel := startTunnel(t, s.config(false, config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: unusedAddr(t)}))
	tunnel.waitReady(t)

	s.signalSessions("KILL")
	assertConnectionLost(t, tunnel, 3*time.Second)
}

func TestExitsWhenServerContainerDies(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	tunnel := startTunnel(t, s.config(false, config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: unusedAddr(t)}))
	tunnel.waitReady(t)

	s.kill()
	// Whether the host sees the reset depends on the container network
	// proxy, so allow for detection by keepalive instead.
	assertConnectionLost(t, tunnel, 5*time.Second)
}

func assertConnectionLost(t *testing.T, tunnel *tunnelProcess, limit time.Duration) {
	t.Helper()
	code, elapsed := tunnel.waitExit(t, limit)
	if code != 1 {
		t.Fatalf("exit code %d after %v, want 1", code, elapsed)
	}
	logs := tunnel.logs()
	for _, reason := range []string{"SSH connection closed", "SSH keepalive failed", "SSH keepalive timed out"} {
		if strings.Contains(logs, reason) {
			return
		}
	}
	t.Fatal("exit reason missing from log")
}
