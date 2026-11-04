//go:build integration

package integration

import (
	"strings"
	"testing"
)

// A link goes through its whole life between two containers: add, change,
// stop, start, reconnect after the server's daemon dies, and remove. The
// server needs no setup: the link's peer-bridge starts its daemon.
func TestLinkLifecycle(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	client.mustGst("name", "cli")
	if server.daemonPID() != "" {
		t.Fatal("the server runs a daemon before the link")
	}

	out := client.mustGst(append([]string{"link", "add", "srv"}, p.flags()...)...)
	if !strings.Contains(out, "Added link srv") {
		t.Fatalf("unexpected output %q", out)
	}
	link := client.waitLink("srv", "running")

	// The peer-bridge started the server's daemon, which sees the client.
	if server.daemonPID() == "" {
		t.Fatal("the server has no daemon after the link came up")
	}
	cli, topo := server.waitReachable("cli")
	srv := self(topo)
	if link.Peer == nil || link.Peer.ID != srv.ID || link.Peer.Name != srv.Name || link.Peer.Software == "" {
		t.Fatalf("the client sees its peer as %+v; the server is %+v", link.Peer, srv)
	}
	if len(topo.Nodes) != 2 || len(topo.Edges) != 1 || topo.Edges[0].From != cli.ID || topo.Edges[0].Link != "srv" || path(topo, cli) != srv.Name+",cli" {
		t.Fatalf("the server's mesh: %+v", topo)
	}

	// The link log records the link without naming the server.
	log := client.mustGst("link", "logs", "srv")
	if !strings.Contains(log, "link up") || strings.Contains(log, server.ip) {
		t.Fatalf("unexpected link log:\n%s", log)
	}

	// A change restarts the link with it.
	out = client.mustGst("link", "set", "srv", "--keepalive-interval", "5s")
	if !strings.Contains(out, "Restarted link srv") {
		t.Fatalf("unexpected output %q", out)
	}
	link = client.waitLink("srv", "running")
	if link.KeepaliveInterval != 5 {
		t.Fatalf("keepalive interval %d, want 5", link.KeepaliveInterval)
	}

	// A stopped link stays configured but does not run.
	client.mustGst("link", "stop", "srv")
	if status := client.links()["srv"].Status; status != "stopped" {
		t.Fatalf("link is %s after stop", status)
	}
	client.mustGst("link", "start", "srv")
	client.waitLink("srv", "running")

	// The link reconnects after the server's daemon dies.
	client.mustGst("link", "set", "srv", "--keepalive-interval", "1s")
	client.waitLink("srv", "running")
	server.killDaemon()
	client.waitLink("srv", "down")
	client.waitLink("srv", "running")
	if server.daemonPID() == "" {
		t.Fatal("the server's daemon was not started again")
	}

	// It also reconnects after the SSH connection drops.
	server.signalSessions("TERM")
	client.waitLink("srv", "down")
	client.waitLink("srv", "running")

	client.mustGst("link", "remove", "srv")
	if _, ok := client.links()["srv"]; ok {
		t.Fatal("link remains after remove")
	}
	if _, err := client.sh(`test -e ~/.local/state/gst/logs/srv.log`); err == nil {
		t.Fatal("log remains after remove")
	}
}

// When a link ends, the machine it led to stops reaching the other end.
func TestLinkStopIsSeenByServer(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	client.mustGst("name", "cli")
	client.mustGst(append([]string{"link", "add", "srv"}, p.flags()...)...)
	client.waitLink("srv", "running")
	server.waitReachable("cli")
	client.mustGst("link", "stop", "srv")
	server.waitUnreachable("cli")
}

// A server without gst in PATH reports why the link fails: the link is
// down, link check fails at the remote gst step with a hint, and
// --remote-gst names the real command.
func TestLinkRemoteGstMissing(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	if _, err := server.exec("root", "mv", "/usr/local/bin/gst", "/opt/gst"); err != nil {
		t.Fatal(err)
	}

	client.mustGst(append([]string{"link", "add", "srv"}, append(p.flags(), "--remote-gst", "/opt/nonexistent/gst")...)...)
	link := client.waitLink("srv", "down")
	if !strings.Contains(link.Error, "gst") {
		t.Fatalf("the error does not mention gst: %q", link.Error)
	}

	code, out, _ := client.gst("link", "check", "srv")
	if code != 1 {
		t.Fatalf("link check exited %d:\n%s", code, out)
	}
	var failed, hint bool
	for line := range strings.Lines(out) {
		failed = failed || strings.HasPrefix(line, "FAIL") && strings.Contains(line, "remote gst")
		hint = hint || strings.Contains(line, "hint:") && strings.Contains(line, "--remote-gst")
	}
	if !failed || !hint {
		t.Fatalf("link check does not fail the remote gst step with a hint:\n%s", out)
	}
	if !strings.Contains(out, "ok") || strings.Contains(out, "FAIL  login") {
		t.Fatalf("the steps before remote gst should pass:\n%s", out)
	}

	client.mustGst("link", "set", "srv", "--remote-gst", "/opt/gst")
	client.waitLink("srv", "running")
	if code, out, _ := client.gst("link", "check", "srv"); code != 0 {
		t.Fatalf("link check exited %d:\n%s", code, out)
	}
}
