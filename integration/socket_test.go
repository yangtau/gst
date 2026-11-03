//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

// The remote socket is reached with nc on the server, the remote socket
// target is a one-shot nc listener there, and the local socket targets a
// service on the server's loopback, so each direction proves that the
// right side dialed.
func TestUnixSocketForwards(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	p.client.startEcho(echoPort)
	p.server.serve(4000, "from-server")
	localSocket := nodeHome + "/local.sock"
	p.server.startSocketListener("tunnel", "/home/tunnel/greeting.sock", "greeting")

	tunnel := p.client.startTunnel(p.config(false,
		config.Forward{Direction: config.Remote, Listen: "/home/tunnel/remote.sock", Target: echoTarget},
		config.Forward{Direction: config.Local, Listen: localSocket, Target: "127.0.0.1:4000"},
		config.Forward{Direction: config.Local, Listen: "127.0.0.1:15000", Target: "/home/tunnel/greeting.sock"},
	))
	tunnel.waitReady(t)

	if out, err := p.server.exec("tunnel", "sh", "-c", `printf hello | nc -N -U /home/tunnel/remote.sock`); err != nil || out != "reply:hello" {
		t.Fatalf("remote socket forward: %q, %v", out, err)
	}
	if out, err := p.client.helper("line", "unix", localSocket); err != nil || out != "from-server\n" {
		t.Fatalf("local socket forward: %q, %v", out, err)
	}
	if out, err := p.client.helper("line", "tcp", "127.0.0.1:15000"); err != nil || out != "greeting\n" {
		t.Fatalf("remote socket target: %q, %v", out, err)
	}
	if mode := p.client.mustSh(`stat -c %a "$1"`, localSocket); mode != "600\n" {
		t.Fatalf("local socket mode %q", mode)
	}

	tunnel.signal(t, "TERM")
	if code, _ := tunnel.waitExit(t, 5*time.Second); code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	if _, err := p.client.sh(`test ! -e "$1"`, localSocket); err != nil {
		t.Fatal("local socket survived shutdown")
	}
	tunnel.assertPrivateLog(t, ".sock")
}

// sshd leaves a forward's socket file behind, even when the tunnel exits
// cleanly, and does not replace it unless StreamLocalBindUnlink is set, so
// the file blocks the next session until cleanup removes it.
func TestCleanupRemovesStaleRemoteSockets(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	s := p.server
	p.client.startEcho(echoPort)
	stale, live := "/home/tunnel/stale.sock", "/home/tunnel/live.sock"
	s.leaveStaleSocket("tunnel", stale)
	if _, err := s.exec("tunnel", "sh", "-c", `nc -lkU "$1" </dev/null >/dev/null 2>&1 &`, "sh", live); err != nil {
		t.Fatal(err)
	}
	s.waitForSocket(live)

	blocked := p.client.startTunnel(p.config(false, config.Forward{Direction: config.Remote, Listen: stale, Target: echoTarget}))
	if code, _ := blocked.waitExit(t, 10*time.Second); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if !strings.Contains(blocked.logs(), "listener start failed for forward 1") {
		t.Fatal("missing listener failure log")
	}

	tunnel := p.client.startTunnel(p.config(true,
		config.Forward{Direction: config.Remote, Listen: stale, Target: echoTarget},
		config.Forward{Direction: config.Remote, Listen: live, Target: echoTarget},
	))
	tunnel.waitReady(t)
	if s.hasProcess("tunnel", "nc") {
		t.Fatal("socket listener survived cleanup")
	}
	for _, path := range []string{stale, live} {
		if out, err := s.exec("tunnel", "sh", "-c", `printf hello | nc -N -U "$1"`, "sh", path); err != nil || out != "reply:hello" {
			t.Fatalf("forward on %s after cleanup: %q, %v", path, out, err)
		}
	}
}

func TestCleanupSparesOtherUsersSocket(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	path := "/tmp/other.sock"
	p.server.leaveStaleSocket("other", path)

	tunnel := p.client.startTunnel(p.config(true, config.Forward{Direction: config.Remote, Listen: path, Target: refusedTarget}))
	if code, _ := tunnel.waitExit(t, 20*time.Second); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if !strings.Contains(tunnel.logs(), "remote listener cleanup failed") {
		t.Fatal("missing cleanup failure log")
	}
	if out, err := p.server.exec("root", "stat", "-c", "%U %F", path); err != nil || out != "other socket\n" {
		t.Fatalf("cleanup removed another user's socket: %q, %v", out, err)
	}
}

// startSocketListener serves one connection on a container socket owned by
// user, answering with line.
func (s *node) startSocketListener(user, path, line string) {
	s.t.Helper()
	if _, err := s.exec(user, "sh", "-c", `printf '%s\n' "$2" | nc -N -lU "$1" >/dev/null 2>&1 &`, "sh", path, line); err != nil {
		s.t.Fatal(err)
	}
	s.waitForSocket(path)
}

// leaveStaleSocket leaves a socket file owned by user that nothing listens on.
func (s *node) leaveStaleSocket(user, path string) {
	s.t.Helper()
	if _, err := s.exec(user, "sh", "-c", `nc -lU "$1" </dev/null >/dev/null 2>&1 & while [ ! -S "$1" ]; do sleep 0.1; done; kill $!`, "sh", path); err != nil {
		s.t.Fatal(err)
	}
	eventually(s.t, 5*time.Second, "listener of the stale socket did not exit", func() bool {
		return !s.hasProcess(user, "nc")
	})
	s.waitForSocket(path)
}

func (s *node) waitForSocket(path string) {
	s.t.Helper()
	eventually(s.t, 5*time.Second, "socket did not appear", func() bool {
		_, err := s.exec("root", "test", "-S", path)
		return err == nil
	})
}
