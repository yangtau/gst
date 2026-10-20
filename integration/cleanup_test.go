//go:build integration

package integration

import (
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"reverse-ssh-tunnel/internal/config"
)

func TestCleanupKillsStaleListenerOfLoginUser(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	s.startListener("tunnel", 2222)
	target := startEchoTarget(t)

	tunnel := startTunnel(t, s.config(true, config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: target}))
	tunnel.waitReady(t)

	if s.hasProcess("tunnel", "nc") {
		t.Fatal("stale listener survived cleanup")
	}
	if err := echoRoundTrip(s.hostAddr(2222), "after cleanup"); err != nil {
		t.Fatal(err)
	}
}

// A tunnel whose client stopped answering keeps its remote port until sshd
// reaps the session. Cleanup cannot kill that session on Linux, because sshd
// sessions are not dumpable, so it must fail clearly instead of hanging; the
// server's ClientAliveInterval (1 s × 2 in the test image) frees the port.
func TestStaleSessionIsReapedByServerNotCleanup(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	target := startEchoTarget(t)
	forward := config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: target}

	stale := startTunnel(t, s.config(false, forward))
	stale.waitReady(t)
	stale.signal(t, syscall.SIGSTOP)

	blocked := startTunnel(t, s.config(true, forward))
	if code, _ := blocked.waitExit(t, 20*time.Second); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if !strings.Contains(blocked.logs(), "remote listener cleanup failed") {
		t.Fatal("missing cleanup failure log")
	}

	eventually(t, 10*time.Second, "sshd did not reap the stale session", func() bool {
		return len(s.listenerOwners(2222)) == 0
	})
	current := startTunnel(t, s.config(true, forward))
	current.waitReady(t)
	if err := echoRoundTrip(s.hostAddr(2222), "served by the new tunnel"); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupSparesOtherUsersListener(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	s.startListener("other", 2222)

	tunnel := startTunnel(t, s.config(true, config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: unusedAddr(t)}))
	if code, _ := tunnel.waitExit(t, 20*time.Second); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if !strings.Contains(tunnel.logs(), "remote listener cleanup failed") {
		t.Fatal("missing cleanup failure log")
	}
	if !slices.Contains(s.listenerOwners(2222), "other") {
		t.Fatal("cleanup killed another user's listener")
	}
}

func TestBusyPortWithoutCleanupFails(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	s.startListener("tunnel", 2222)

	tunnel := startTunnel(t, s.config(false, config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: unusedAddr(t)}))
	if code, _ := tunnel.waitExit(t, 10*time.Second); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if !strings.Contains(tunnel.logs(), "listener start failed for mapping 1") {
		t.Fatal("missing listener failure log")
	}
	if !s.hasProcess("tunnel", "nc") {
		t.Fatal("listener was killed although cleanup is disabled")
	}
}
