//go:build integration

package integration

import (
	"strings"
	"testing"
)

// A link goes through its whole life between two containers: add, change
// by removing and adding again, reconnect after the server's daemon dies,
// and remove. The server needs no setup: the link's peer-bridge starts its
// daemon.
func TestLinkLifecycle(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	client.mustGst("name", "cli")
	if server.daemonPID() != "" {
		t.Fatal("the server runs a daemon before the link")
	}

	out := client.mustGst("add", "srv", p.destination())
	if !strings.Contains(out, "Added link srv") || !strings.Contains(out, "gst status") {
		t.Fatalf("unexpected output %q", out)
	}
	link := client.waitLink("srv", "running")

	// The peer-bridge started the server's daemon, which sees the client.
	if server.daemonPID() == "" {
		t.Fatal("the server has no daemon after the link came up")
	}
	cli, topo := server.waitReachable("cli")
	srv := self(topo)
	if link.Peer == nil || link.Peer.ID != srv.ID || link.Peer.Name != srv.Name || link.Peer.Software == "" || link.Destination != p.destination() {
		t.Fatalf("the client sees its peer as %+v; the server is %+v", link, srv)
	}
	if len(topo.Nodes) != 2 || len(topo.Edges) != 1 || topo.Edges[0].From != cli.ID || topo.Edges[0].Link != "srv" || path(topo, cli) != srv.Name+",cli" {
		t.Fatalf("the server's mesh: %+v", topo)
	}

	// gst status lists both machines, each online for the other.
	if out := client.mustGst("status"); !strings.Contains(out, srv.Name) || !strings.Contains(out, "online") || strings.Contains(out, "# Problems") {
		t.Fatalf("gst status on the client:\n%s", out)
	}
	if out := server.mustGst("status"); !strings.Contains(out, "cli") || !strings.Contains(out, "online") {
		t.Fatalf("gst status on the server:\n%s", out)
	}

	// The daemon's log records the link without naming the server.
	log := client.mustSh(`cat ~/.systemctl-shim/*.log`)
	if !strings.Contains(log, "link srv: up") || strings.Contains(log, server.ip) {
		t.Fatalf("unexpected daemon log:\n%s", log)
	}

	// A link is not changed in place: adding it again fails with a hint,
	// and removing it first makes room.
	code, _, stderr := client.gst("add", "srv", p.destination())
	if code != 1 || !strings.Contains(stderr, "already exists") || !strings.Contains(stderr, "gst remove srv") {
		t.Fatalf("adding an existing link: exit %d, stderr %q", code, stderr)
	}
	client.mustGst("remove", "srv")
	if _, ok := client.links()["srv"]; ok {
		t.Fatal("link remains after remove")
	}
	client.mustGst("add", "srv", p.destination(), "--remote-gst", "gst")
	client.waitLink("srv", "running")

	// The link reconnects after the server's daemon dies.
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

	client.mustGst("remove", "srv")
	if _, ok := client.links()["srv"]; ok {
		t.Fatal("link remains after remove")
	}
}

// A link that cannot connect is listed as a problem by gst status.
func TestLinkDownIsAProblem(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client := p.client
	client.mustGst("add", "dead", p.deadEnd())
	link := client.waitLink("dead", "down")
	if link.Error == "" {
		t.Fatalf("a down link has no error: %+v", link)
	}
	out := client.mustGst("status")
	if !strings.Contains(out, "# Problems:") || !strings.Contains(out, "link dead is down") {
		t.Fatalf("gst status does not list the link as a problem:\n%s", out)
	}
}

// When a link is removed, the machine it led to stops reaching the other
// end.
func TestLinkRemoveIsSeenByServer(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	client.mustGst("name", "cli")
	client.mustGst("add", "srv", p.destination())
	client.waitLink("srv", "running")
	server.waitReachable("cli")
	client.mustGst("remove", "srv")
	server.waitUnreachable("cli")
}

// A server without gst in PATH reports why the link fails: the link is
// down with an error that names gst, gst status lists it as a problem, and
// adding the link again with --remote-gst names the real command.
func TestLinkRemoteGstMissing(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	if _, err := server.exec("root", "mv", "/usr/local/bin/gst", "/opt/gst"); err != nil {
		t.Fatal(err)
	}

	client.mustGst("add", "srv", p.destination(), "--remote-gst", "/opt/nonexistent/gst")
	link := client.waitLink("srv", "down")
	if !strings.Contains(link.Error, "gst") {
		t.Fatalf("the error does not mention gst: %q", link.Error)
	}
	out := client.mustGst("status")
	if !strings.Contains(out, "# Problems:") || !strings.Contains(out, "link srv is down") || !strings.Contains(out, "gst") {
		t.Fatalf("gst status does not list the failed remote gst:\n%s", out)
	}

	client.mustGst("remove", "srv")
	client.mustGst("add", "srv", p.destination(), "--remote-gst", "/opt/gst")
	client.waitLink("srv", "running")
	if out := client.mustGst("status"); strings.Contains(out, "# Problems") {
		t.Fatalf("gst status lists problems after the fix:\n%s", out)
	}
}
