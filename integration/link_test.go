//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

// A link goes through its whole life between two containers: link, change
// by unlinking and linking again, reconnect after the server's daemon dies,
// and unlink.
func TestLinkLifecycle(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	client.mustGst("name", "cli")

	out := client.mustGst("link", p.destination())
	if !strings.Contains(out, "Linked to "+p.destination()) {
		t.Fatalf("unexpected output %q", out)
	}
	link := client.waitLink(p.destination(), "running")

	cli, topo := server.waitReachable("cli")
	srv := self(topo)
	if link.Peer == nil || link.Peer.ID != srv.ID || link.Peer.Name != srv.Name || link.Peer.Software == "" || link.Destination != p.destination() {
		t.Fatalf("the client sees its peer as %+v; the server is %+v", link, srv)
	}
	if len(topo.Nodes) != 2 || len(topo.Edges) != 1 || topo.Edges[0].From != cli.ID || topo.Edges[0].Link != config.LinkID(p.destination()) || path(topo, cli) != srv.Name+",cli" {
		t.Fatalf("the server's mesh: %+v", topo)
	}

	// gst status lists both machines, each reached by the other.
	if out := client.mustGst("status"); !strings.Contains(out, srv.Name) || !strings.Contains(out, "relay") || strings.Contains(out, "link to") {
		t.Fatalf("gst status on the client:\n%s", out)
	}
	if out := server.mustGst("status"); !strings.Contains(out, "cli") || !strings.Contains(out, "relay") {
		t.Fatalf("gst status on the server:\n%s", out)
	}

	// The daemon's log records the link without naming the server.
	log := client.mustSh(`cat ~/.systemctl-shim/*.log`)
	if !strings.Contains(log, "link "+config.LinkID(p.destination())+": up") || strings.Contains(log, server.ip) || strings.Contains(log, p.destination()) {
		t.Fatalf("unexpected daemon log:\n%s", log)
	}

	// A link is not changed in place: linking again fails with a hint,
	// and unlinking first makes room.
	code, _, stderr := client.gst("link", p.destination())
	if code != 1 || !strings.Contains(stderr, "already links to") || !strings.Contains(stderr, "gst unlink "+p.destination()) {
		t.Fatalf("linking again: exit %d, stderr %q", code, stderr)
	}
	client.mustGst("unlink", p.destination())
	if _, ok := client.links()[p.destination()]; ok {
		t.Fatal("link remains after unlink")
	}
	client.mustGst("link", p.destination(), "--remote-gst", "gst")
	client.waitLink(p.destination(), "running")

	// The link reconnects after the server's daemon dies.
	server.killDaemon()
	client.waitLink(p.destination(), "down")
	client.waitLink(p.destination(), "running")
	if server.daemonPID() == "" {
		t.Fatal("the server's daemon was not started again")
	}

	// It also reconnects after the SSH connection drops.
	server.signalSessions("TERM")
	client.waitLink(p.destination(), "down")
	client.waitLink(p.destination(), "running")

	// The machine's name unlinks it too.
	client.mustGst("unlink", srv.Name)
	if _, ok := client.links()[p.destination()]; ok {
		t.Fatal("link remains after unlink")
	}
}

// A link that cannot connect is listed as a problem by gst status.
func TestLinkDownIsAProblem(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client := p.client
	dead := p.deadEnd()
	client.mustGst("link", dead)
	link := client.waitLink(dead, "down")
	if link.Error == "" {
		t.Fatalf("a down link has no error: %+v", link)
	}
	out := client.mustGst("status")
	if !strings.Contains(out, "Problems:") || !strings.Contains(out, "link to "+dead+": down") {
		t.Fatalf("gst status does not list the link as a problem:\n%s", out)
	}
}

// When a link is unlinked, the machine it led to stops reaching the other
// end.
func TestLinkRemoveIsSeenByServer(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	client.mustGst("name", "cli")
	client.mustGst("link", p.destination())
	client.waitLink(p.destination(), "running")
	server.waitReachable("cli")
	client.mustGst("unlink", p.destination())
	server.waitUnreachable("cli")
}

// A server without gst in PATH reports why the link fails: the link is
// down with an error that names gst, gst status lists it as a problem, and
// linking again with --remote-gst names the real command.
func TestLinkRemoteGstMissing(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	if _, err := server.exec("root", "mv", "/usr/local/bin/gst", "/opt/gst"); err != nil {
		t.Fatal(err)
	}

	client.mustGst("link", p.destination(), "--remote-gst", "/opt/nonexistent/gst")
	link := client.waitLink(p.destination(), "down")
	if !strings.Contains(link.Error, "gst") {
		t.Fatalf("the error does not mention gst: %q", link.Error)
	}
	out := client.mustGst("status")
	if !strings.Contains(out, "Problems:") || !strings.Contains(out, "link to "+p.destination()+": down") || !strings.Contains(out, "gst") {
		t.Fatalf("gst status does not list the failed remote gst:\n%s", out)
	}

	client.mustGst("unlink", p.destination())
	client.mustGst("link", p.destination(), "--remote-gst", "/opt/gst")
	client.waitLink(p.destination(), "running")
	if out := client.mustGst("status"); strings.Contains(out, "link to") {
		t.Fatalf("gst status lists the link after the fix:\n%s", out)
	}
}

// gst never sets up another machine: a link to one where nobody ran gst up
// says what to run there, and connects by itself once someone has.
func TestLinkToAMachineThatIsNotUp(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	server.mustGst("down")
	server.mustSh(`rm -rf ~/.config/gst`)

	code, _, stderr := client.gst("link", p.destination())
	if code != 1 || !strings.Contains(stderr, "gst is not up there") || !strings.Contains(stderr, "`ssh -t "+p.destination()+" gst up`") {
		t.Fatalf("gst link: exit %d: %q", code, stderr)
	}
	if server.daemonPID() != "" {
		t.Fatal("the link started a daemon on the server")
	}
	if _, err := server.sh(`test -e ~/.config/gst/config.json`); err == nil {
		t.Fatal("the link gave the server a configuration")
	}

	server.mustGst("up")
	client.waitLink(p.destination(), "running")
}
