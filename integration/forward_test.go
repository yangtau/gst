//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

// echoPort is where tests run an echo server on the client node.
const echoPort = 7000

var echoTarget = "127.0.0.1:7000"

// refusedTarget is a client address where nothing listens.
const refusedTarget = "127.0.0.1:1"

func TestForwardsConcurrentTrafficOnAllMappings(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	p.client.startEcho(echoPort)
	tunnel := p.client.startTunnel(p.config(false,
		config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: echoTarget},
		config.Forward{Direction: config.Remote, Listen: "0.0.0.0:3000", Target: echoTarget},
	))
	tunnel.waitReady(t)

	if err := p.client.roundtrip("-conns", "16", "-repeat", "50000", p.server.addr(2222), p.server.addr(3000)); err != nil {
		t.Fatal(err)
	}
	tunnel.assertPrivateLog(t, append(p.secrets(), echoTarget)...)
}

// The local forward targets a service on the server's loopback, so its
// answer proves that the server dialed the target.
func TestForwardsBothDirections(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	p.client.startEcho(echoPort)
	p.server.serve(4000, "from-server")
	tunnel := p.client.startTunnel(p.config(false,
		config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: echoTarget},
		config.Forward{Direction: config.Local, Listen: "127.0.0.1:15000", Target: "127.0.0.1:4000"},
	))
	tunnel.waitReady(t)

	if err := p.client.roundtrip(p.server.addr(2222)); err != nil {
		t.Fatal(err)
	}
	if got, err := p.client.fetch(15000); err != nil || got != "from-server" {
		t.Fatalf("local forward answered %q: %v", got, err)
	}

	tunnel.signal(t, "TERM")
	if code, _ := tunnel.waitExit(t, 5*time.Second); code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	if _, err := p.client.fetch(15000); err == nil {
		t.Fatal("local listener survived shutdown")
	}
}

// The dynamic forward requests "localhost" by name, and only the server
// listens on the port, so the answer proves that the server resolved and
// dialed the target.
func TestDynamicForward(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	p.server.serve(4000, "from-server")
	tunnel := p.client.startTunnel(p.config(false, config.Forward{Direction: config.Dynamic, Listen: "127.0.0.1:1080"}))
	tunnel.waitReady(t)

	if got, err := p.client.helper("socks", "127.0.0.1:1080", "localhost", "4000"); err != nil || got != "from-server\n" {
		t.Fatalf("dynamic forward answered %q: %v", got, err)
	}
	tunnel.assertPrivateLog(t, "127.0.0.1:1080", "localhost")
}

// The target only listens on the client's loopback, so a reply proves that
// gst, not sshd on the server, resolved and dialed localhost.
func TestReverseDynamicForward(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	p.client.startEcho(echoPort)
	tunnel := p.client.startTunnel(p.config(false, config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222"}))
	tunnel.waitReady(t)

	if got, err := p.server.helper("socks", "127.0.0.1:2222", "localhost", "7000", "reverse SOCKS"); err != nil || got != "reply:reverse SOCKS" {
		t.Fatalf("reverse SOCKS answered %q: %v", got, err)
	}
	tunnel.assertPrivateLog(t, ":7000", "localhost")
	tunnel.signal(t, "TERM")
	if code, _ := tunnel.waitExit(t, 5*time.Second); code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	eventually(t, 5*time.Second, "reverse SOCKS listener survived shutdown", func() bool {
		return len(p.server.listenerOwners(2222)) == 0
	})
}

func TestRefusedTargetClosesOnlyThatConnection(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	p.client.startEcho(echoPort)
	tunnel := p.client.startTunnel(p.config(false,
		config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: refusedTarget},
		config.Forward{Direction: config.Remote, Listen: "0.0.0.0:3000", Target: echoTarget},
	))
	tunnel.waitReady(t)

	if err := p.client.roundtrip(p.server.addr(2222)); err == nil {
		t.Fatal("refused target produced a reply")
	}
	eventually(t, 5*time.Second, "missing target failure log", func() bool {
		return strings.Contains(tunnel.logs(), "target connection failed for forward 1")
	})
	if err := p.client.roundtrip(p.server.addr(3000)); err != nil {
		t.Fatalf("healthy forward broken: %v", err)
	}
	if !tunnel.running() {
		t.Fatal("tunnel exited after a target failure")
	}
}

func TestSIGTERMExitsCleanlyAndReleasesRemotePorts(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	p.client.startEcho(echoPort)
	tunnel := p.client.startTunnel(p.config(false,
		config.Forward{Direction: config.Remote, Listen: "0.0.0.0:2222", Target: echoTarget},
		config.Forward{Direction: config.Remote, Listen: "0.0.0.0:3000", Target: echoTarget},
	))
	tunnel.waitReady(t)
	ports := []int{2222, 3000}
	for _, port := range ports {
		if len(p.server.listenerOwners(port)) == 0 {
			t.Fatalf("port %d not listening while tunnel is ready", port)
		}
	}

	tunnel.signal(t, "TERM")
	if code, elapsed := tunnel.waitExit(t, 5*time.Second); code != 0 {
		t.Fatalf("exit code %d after %v, want 0", code, elapsed)
	}
	for _, port := range ports {
		eventually(t, 5*time.Second, "remote port still listening", func() bool {
			return len(p.server.listenerOwners(port)) == 0
		})
	}
}
