//go:build integration

package integration

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"reverse-ssh-tunnel/internal/config"
)

func TestRejectsUnknownHostKey(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.config(false, config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: unusedAddr(t)})
	cfg.KnownHostsFile = filepath.Join(t.TempDir(), "known_hosts")
	writeFile(t, cfg.KnownHostsFile, []byte(knownhosts.Line([]string{s.addr}, key)+"\n"))

	tunnel := startTunnel(t, cfg)
	assertStartupFailure(t, tunnel, "SSH handshake failed")
}

func TestFailsWhenServerUnreachable(t *testing.T) {
	t.Parallel()
	identity, _ := newIdentity(t)
	cfg := config.Config{
		Server:                   unusedAddr(t),
		User:                     "tunnel",
		IdentityFile:             identity,
		KnownHostsFile:           "/dev/null",
		ConnectTimeoutSeconds:    2,
		KeepaliveIntervalSeconds: 1,
		KeepaliveCountMax:        1,
		Forwards:                 []config.Forward{{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: "127.0.0.1:1"}},
	}
	tunnel := startTunnel(t, cfg)
	assertStartupFailure(t, tunnel, "TCP connection failed")
}

func assertStartupFailure(t *testing.T, tunnel *tunnelProcess, reason string) {
	t.Helper()
	if code, _ := tunnel.waitExit(t, 10*time.Second); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if !strings.Contains(tunnel.logs(), reason) {
		t.Fatalf("log does not contain %q", reason)
	}
}
