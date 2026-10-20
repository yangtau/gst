//go:build integration

package integration

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/sshconfig"
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
	cfg := config.Tunnel{
		Destination:              "ssh://tunnel@" + unusedAddr(t),
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

func TestCheckPassesAgainstServer(t *testing.T) {
	t.Parallel()
	s := startSSHServer(t)
	tunnel := s.config(false)
	data, err := json.Marshal(config.File{Tunnels: map[string]*config.Tunnel{"it": &tunnel}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, path, data)

	cmd := exec.Command(tunnelBinary, "--config", path, "tunnel", "check", "it", "--json")
	cmd.Env = append(os.Environ(), sshconfig.ConfigFileEnv+"=/dev/null")
	out, err := cmd.Output()
	var result struct{ OK bool }
	if err != nil || json.Unmarshal(out, &result) != nil || !result.OK {
		t.Fatalf("check failed: %v\n%s", err, out)
	}
}
