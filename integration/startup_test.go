//go:build integration

package integration

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

func TestRejectsUnknownHostKey(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	cfg := p.config(false, config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: refusedTarget})
	cfg.KnownHostsFile = nodeHome + "/.ssh/wrong_known_hosts"
	p.client.writeFile(cfg.KnownHostsFile, []byte(knownhosts.Line([]string{p.server.addr(22)}, key)+"\n"))

	tunnel := p.client.startTunnel(cfg)
	assertStartupFailure(t, tunnel, "SSH handshake failed")
}

func TestFailsWhenServerUnreachable(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	cfg := p.config(false, config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: refusedTarget})
	cfg.Destination = "ssh://tunnel@127.0.0.1:1"
	cfg.ConnectTimeoutSeconds = 2
	tunnel := p.client.startTunnel(cfg)
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
	p := startPair(t)
	tunnel := p.config(false)
	data, err := json.Marshal(config.File{Tunnels: map[string]*config.Tunnel{"it": &tunnel}})
	if err != nil {
		t.Fatal(err)
	}
	path := nodeHome + "/check.json"
	p.client.writeFile(path, data)

	code, out, stderr := p.client.gst("--config", path, "check", "it", "--json")
	var result struct{ OK bool }
	if code != 0 || json.Unmarshal([]byte(out), &result) != nil || !result.OK {
		t.Fatalf("check exited %d: %s%s", code, out, stderr)
	}
}
