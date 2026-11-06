package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func runningNet() *wire.NetState {
	return &wire.NetState{
		Subnet: "10.99.0.0/16", Address: "10.99.1.2", Interface: "gst0",
		Peers: []wire.NetPeer{
			{Node: peerOf("devbox").ID, Name: "devbox", Address: "10.99.3.4", Path: wire.PathDirect, Handshake: time.Now().Add(-time.Minute), Received: 2048, Sent: 5},
			{Node: peerOf("homelab").ID, Name: "homelab", Address: "10.99.5.6", Path: wire.PathSSH},
		},
	}
}

func TestSubnetShows(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	e.back.net = runningNet()
	if out := e.mustGst("subnet"); out != "10.99.0.0/16\n" {
		t.Fatalf("output %q", out)
	}
	e.back.net = nil
	e.expectError(exitFailure, []string{"subnet"}, "no subnet yet", "gst status")
	e.mustGst("down")
	e.expectError(exitFailure, []string{"subnet"}, "does not answer", "gst up")
}

func TestSubnetChanges(t *testing.T) {
	e := newEnv(t)
	e.back.net = runningNet()
	contains(t, e.mustGst("subnet", "10.99.7.0/16"), "10.99.1.2", "gst0", "10.99.0.0/16")
	n := e.load().Net
	if n == nil || n.Subnet != "10.99.0.0/16" || n.Since == 0 {
		t.Fatalf("config: %+v", n)
	}
	if !e.back.installed {
		t.Fatal("gst subnet did not start the daemon")
	}
}

func TestSubnetRejects(t *testing.T) {
	e := newEnv(t)
	for _, subnet := range []string{"8.8.8.0/24", "10.0.0.0/7", "nonsense"} {
		e.expectError(exitUsage, []string{"subnet", subnet}, "error:", "usage:")
	}
	if e.load().Net != nil {
		t.Fatal("a rejected subnet was saved")
	}
}

func TestSubnetReportsWhatToDo(t *testing.T) {
	e := newEnv(t)
	e.back.net = &wire.NetState{Error: "the helper of the virtual network does not run", Hint: "install it with `sudo gst install`"}
	e.expectError(exitFailure, []string{"subnet", "10.99.0.0/16"}, "error: the helper", "hint: install it with `sudo gst install`")
	if e.load().Net == nil {
		t.Fatal("the choice should apply once the helper is there")
	}
}

func TestStatusShowsTheNetwork(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	e.back.net = runningNet()
	self, peer := peerOf("laptop"), peerOf("devbox")
	e.back.topology = wire.Topology{Self: self.ID, Nodes: []wire.TopoNode{
		{ID: self.ID, Name: "laptop", Reachable: true, Net: &wire.NetInfo{Address: "10.99.1.2"}},
		{ID: peer.ID, Name: "devbox", Reachable: true, Path: []string{self.ID, peer.ID}},
	}}
	out := e.mustGst("status")
	contains(t, out, "10.99.1.2")
	if lines := strings.Split(out, "\n"); !strings.Contains(lines[1], " - ") {
		t.Fatalf("no placeholder for the machine off the network:\n%s", out)
	}

	all := e.mustGst("status", "--all")
	contains(t, all, "MACHINE", "1m0s ago", "2.0 KiB", "5 B", "never")
	found := false
	for _, row := range rows(all) {
		if len(row) > 2 && row[0] == "devbox" && row[1] == "10.99.3.4" {
			found = row[2] == "direct"
		}
	}
	if !found {
		t.Fatalf("no line of devbox:\n%s", all)
	}
	v := decode[meshStatus](t, e.mustGst("status", "--json"))
	if v.Self.Net == nil || v.Self.Net.Address != "10.99.1.2" || len(v.Self.Net.Peers) != 2 {
		t.Fatalf("view: %+v", v.Self.Net)
	}

	e.back.net = &wire.NetState{Error: "the helper does not run", Hint: "install it with `sudo gst install`"}
	contains(t, e.mustGst("status"), "# Problems:", "virtual network: the helper does not run; install it with `sudo gst install`")

	e.back.net = runningNet()
	contains(t, e.mustGst("status", "devbox"), "Address: 10.99.1.2 (subnet 10.99.0.0/16)")
}

// A path that outlives the SSH links still counts as online.
func TestStatusOfAMachineWithOnlyAUDPPath(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	a, b := nodeID("aaaa1111"), nodeID("bbbb2222")
	e.back.topology = wire.Topology{Self: a, Nodes: []wire.TopoNode{
		{ID: a, Name: "a", Reachable: true, Path: []string{a}},
		{ID: b, Name: "b", Direct: true},
	}}
	contains(t, e.mustGst("status"), "online; direct")
}

func TestInstallNeedsRoot(t *testing.T) {
	e := newEnv(t)
	e.expectError(exitFailure, []string{"install"}, "needs root", "sudo gst install")
	e.expectError(exitFailure, []string{"uninstall"}, "needs root", "sudo gst uninstall")
}
