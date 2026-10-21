//go:build integration

package integration

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

// The remote socket is reached with nc inside the container, the remote
// socket target is a one-shot nc listener there, and the local socket
// targets sshd, so each direction proves that the right side dialed.
func TestUnixSocketForwards(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	target := startEchoTarget(t)
	localSocket := filepath.Join(socketDir(t), "sshd.sock")
	localTCP := unusedAddr(t)
	s.startSocketListener("tunnel", "/home/tunnel/greeting.sock", "greeting")

	tunnel := startTunnel(t, s.config(false,
		config.Forward{Direction: config.Remote, Listen: "/home/tunnel/remote.sock", Target: target},
		config.Forward{Direction: config.Local, Listen: localSocket, Target: "127.0.0.1:22"},
		config.Forward{Direction: config.Local, Listen: localTCP, Target: "/home/tunnel/greeting.sock"},
	))
	tunnel.waitReady(t)

	if out, err := s.exec("tunnel", "sh", "-c", `printf hello | nc -N -U /home/tunnel/remote.sock`); err != nil || out != "reply:hello" {
		t.Fatalf("remote socket forward: %q, %v", out, err)
	}
	if line := readLine(t, "unix", localSocket); !strings.HasPrefix(line, "SSH-2.0-") {
		t.Fatalf("no sshd banner through the local socket: %q", line)
	}
	if line := readLine(t, "tcp", localTCP); line != "greeting" {
		t.Fatalf("remote socket target: %q", line)
	}
	if info, err := os.Stat(localSocket); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("local socket mode %v: %v", info.Mode(), err)
	}

	tunnel.signal(t, syscall.SIGTERM)
	if code, _ := tunnel.waitExit(t, 5*time.Second); code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	if _, err := os.Lstat(localSocket); !os.IsNotExist(err) {
		t.Fatal("local socket survived shutdown")
	}
	if logs := tunnel.logs(); strings.Contains(logs, ".sock") {
		t.Fatalf("log reveals socket paths:\n%s", logs)
	}
}

// sshd leaves a forward's socket file behind, even when the tunnel exits
// cleanly, and does not replace it unless StreamLocalBindUnlink is set, so
// the file blocks the next session until cleanup removes it.
func TestCleanupRemovesStaleRemoteSockets(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	target := startEchoTarget(t)
	stale, live := "/home/tunnel/stale.sock", "/home/tunnel/live.sock"
	s.leaveStaleSocket("tunnel", stale)
	if _, err := s.exec("tunnel", "sh", "-c", `nc -lkU "$1" </dev/null >/dev/null 2>&1 &`, "sh", live); err != nil {
		t.Fatal(err)
	}
	s.waitForSocket(live)

	blocked := startTunnel(t, s.config(false, config.Forward{Direction: config.Remote, Listen: stale, Target: target}))
	if code, _ := blocked.waitExit(t, 10*time.Second); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if !strings.Contains(blocked.logs(), "listener start failed for forward 1") {
		t.Fatal("missing listener failure log")
	}

	tunnel := startTunnel(t, s.config(true,
		config.Forward{Direction: config.Remote, Listen: stale, Target: target},
		config.Forward{Direction: config.Remote, Listen: live, Target: target},
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
	s := startSSHServer(t)
	path := "/tmp/other.sock"
	s.leaveStaleSocket("other", path)

	tunnel := startTunnel(t, s.config(true, config.Forward{Direction: config.Remote, Listen: path, Target: unusedAddr(t)}))
	if code, _ := tunnel.waitExit(t, 20*time.Second); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if !strings.Contains(tunnel.logs(), "remote listener cleanup failed") {
		t.Fatal("missing cleanup failure log")
	}
	if out, err := s.exec("root", "stat", "-c", "%U %F", path); err != nil || out != "other socket\n" {
		t.Fatalf("cleanup removed another user's socket: %q, %v", out, err)
	}
}

// startSocketListener serves one connection on a container socket owned by
// user, answering with line.
func (s *sshServer) startSocketListener(user, path, line string) {
	s.t.Helper()
	if _, err := s.exec(user, "sh", "-c", `printf '%s\n' "$2" | nc -N -lU "$1" >/dev/null 2>&1 &`, "sh", path, line); err != nil {
		s.t.Fatal(err)
	}
	s.waitForSocket(path)
}

// leaveStaleSocket leaves a socket file owned by user that nothing listens on.
func (s *sshServer) leaveStaleSocket(user, path string) {
	s.t.Helper()
	if _, err := s.exec(user, "sh", "-c", `nc -lU "$1" </dev/null >/dev/null 2>&1 & while [ ! -S "$1" ]; do sleep 0.1; done; kill $!`, "sh", path); err != nil {
		s.t.Fatal(err)
	}
	eventually(s.t, 5*time.Second, "listener of the stale socket did not exit", func() bool {
		return !s.hasProcess(user, "nc")
	})
	s.waitForSocket(path)
}

func (s *sshServer) waitForSocket(path string) {
	s.t.Helper()
	eventually(s.t, 5*time.Second, "socket did not appear", func() bool {
		_, err := s.exec("root", "test", "-S", path)
		return err == nil
	})
}

// socketDir returns a directory for local sockets. It lives in /tmp because
// the run directory may be too long for a socket address on macOS.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gst-it-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func readLine(t *testing.T, network, addr string) string {
	t.Helper()
	conn, err := net.DialTimeout(network, addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, _ := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSuffix(line, "\n")
}
