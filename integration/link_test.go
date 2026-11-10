//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

// A link goes through its whole life between two containers: add, change
// its ssh options, reconnect after the server's daemon dies or the SSH
// connection drops, and unlink.
func TestLinkLifecycle(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	client.mustGst("name", "cli")

	out := client.mustGst("add", p.destination())
	if !strings.Contains(out, "to the network") {
		t.Fatalf("unexpected output %q", out)
	}
	link := client.waitLink(p.destination(), "running")

	cli, topo := server.waitReachable("cli")
	srv := self(topo)
	if link.Peer == nil || link.Peer.ID != srv.ID || link.Peer.Name != srv.Name || link.Peer.Software == "" || link.Destination != p.destination() {
		t.Fatalf("the client sees its peer as %+v; the server is %+v", link, srv)
	}
	if len(topo.Nodes) != 2 || len(topo.Edges) != 1 || topo.Edges[0].From != cli.ID || topo.Edges[0].To != srv.ID || path(topo, cli) != srv.Name+",cli" {
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

	// Adding the destination again changes its ssh options in place.
	if out := client.mustGst("add", p.destination(), "--", "-p", "22"); !strings.Contains(out, "Changed this machine's link to "+p.destination()) {
		t.Fatalf("adding again: %q", out)
	}
	if l := client.waitLink(p.destination(), "running"); len(client.status().Links) != 1 || l.Peer == nil || l.Peer.ID != srv.ID {
		t.Fatalf("the changed link: %+v", client.status().Links)
	}

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

	// Unlinking changes this machine only: both stay in the network,
	// offline to each other.
	client.mustGst("unlink", p.destination())
	if _, ok := client.links()[p.destination()]; ok {
		t.Fatal("link remains after unlink")
	}
	server.waitUnreachable("cli")
	if _, ok := find(server.nodes(), "cli"); !ok {
		t.Fatal("the server forgot the client")
	}
	if out := server.mustGst("status"); !strings.Contains(out, "offline, seen ") {
		t.Fatalf("gst status on the server:\n%s", out)
	}
}

// A link that cannot connect is listed as a problem by gst status.
func TestLinkDownIsAProblem(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client := p.client
	dead := p.deadEnd()
	client.mustGst("add", dead)
	link := client.waitLink(dead, "down")
	if link.Error == "" {
		t.Fatalf("a down link has no error: %+v", link)
	}
	out := client.mustGst("status")
	if !strings.Contains(out, "Problems:") || !strings.Contains(out, "link to "+dead+": down") {
		t.Fatalf("gst status does not list the link as a problem:\n%s", out)
	}
}

// gst remove takes a machine out of the network for every machine of it,
// itself included, and gst add brings it back.
func TestRemove(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	client.mustGst("name", "cli")
	server.mustGst("name", "srv")
	client.mustGst("add", p.destination())
	client.waitLink(p.destination(), "running")
	server.waitReachable("cli")

	if out := server.mustGst("remove", "cli"); !strings.Contains(out, "Removed cli from the network.") {
		t.Fatalf("gst remove cli: %q", out)
	}
	// The removed machine hears of it, drops its link, and says so.
	eventually(t, 30*time.Second, "the removed machine kept its link", func() bool {
		return len(client.status().Links) == 0
	})
	if _, ok := find(server.nodes(), "cli"); ok {
		t.Fatal("the server still lists the removed machine")
	}
	if out := client.mustGst("status"); !strings.Contains(out, "removed from the network by srv") || !strings.Contains(out, "gst add <destination>") {
		t.Fatalf("gst status on the removed machine:\n%s", out)
	}
	if _, ok := find(client.nodes(), "srv"); ok {
		t.Fatal("the removed machine still lists the server")
	}

	// gst add brings it back.
	client.mustGst("add", p.destination())
	client.waitLink(p.destination(), "running")
	server.waitReachable("cli")
	if out := client.mustGst("status"); strings.Contains(out, "removed from the network") {
		t.Fatalf("gst status after coming back:\n%s", out)
	}

	// A machine can leave the network itself.
	if out := client.mustGst("remove", "cli"); !strings.Contains(out, "This machine left the network.") {
		t.Fatalf("gst remove of itself: %q", out)
	}
	eventually(t, 30*time.Second, "the server kept the machine that left", func() bool {
		_, ok := find(server.nodes(), "cli")
		return !ok
	})
}

// Links run the gst that gst up put in place on the other machine, so
// they work wherever it is installed; a link to a machine without it says
// that the remote gst failed.
func TestLinkRunsTheGstOfUp(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	if _, err := server.exec("root", "mv", "/usr/local/bin/gst", "/opt/gst"); err != nil {
		t.Fatal(err)
	}

	client.mustGst("add", p.destination())
	link := client.waitLink(p.destination(), "down")
	if !strings.Contains(link.Error, "gst") {
		t.Fatalf("the error does not mention gst: %q", link.Error)
	}
	out := client.mustGst("status")
	if !strings.Contains(out, "Problems:") || !strings.Contains(out, "link to "+p.destination()+": down") || !strings.Contains(out, "gst") {
		t.Fatalf("gst status does not list the failed remote gst:\n%s", out)
	}

	server.mustSh(`/opt/gst up`)
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

	code, _, stderr := client.gst("add", p.destination())
	if code != 1 || !strings.Contains(stderr, "gst is not up there") || !strings.Contains(stderr, "`ssh -t "+p.destination()+" gst up`") {
		t.Fatalf("gst add: exit %d: %q", code, stderr)
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
