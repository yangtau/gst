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
			{Node: peerOf("homelab").ID, Name: "homelab", Address: "10.99.5.6", Path: wire.PathMesh},
		},
	}
}

func TestNetOn(t *testing.T) {
	e := newEnv(t)
	e.back.net = runningNet()
	out := e.mustGst("net", "on")
	contains(t, out, "10.99.1.2", "gst0", "10.99.0.0/16")
	if n := e.load().Net; n == nil || n.Subnet != "" || n.Since != 0 {
		t.Fatalf("config: %+v", n)
	}
	if !e.back.installed {
		t.Fatal("gst net on did not start the daemon")
	}
}

func TestNetOnWithSubnet(t *testing.T) {
	e := newEnv(t)
	e.back.net = runningNet()
	e.mustGst("net", "on", "--subnet", "10.99.7.0/16")
	n := e.load().Net
	if n == nil || n.Subnet != "10.99.0.0/16" || n.Since == 0 {
		t.Fatalf("config: %+v", n)
	}

	// Joining again keeps the choice, and its time.
	e.mustGst("net", "on")
	if again := e.load().Net; *again != *n {
		t.Fatalf("config: %+v, was %+v", again, n)
	}
}

func TestNetOnRejectsSubnets(t *testing.T) {
	e := newEnv(t)
	for _, subnet := range []string{"8.8.8.0/24", "10.0.0.0/7", "nonsense"} {
		e.expectError(exitUsage, []string{"net", "on", "--subnet", subnet}, "error:", "usage:")
	}
	if e.load().Net != nil {
		t.Fatal("a rejected subnet turned the network on")
	}
}

func TestNetOnReportsWhatToDo(t *testing.T) {
	e := newEnv(t)
	e.back.net = &wire.NetState{Error: "the helper of the virtual network does not run", Hint: "install it with `sudo gst net install`"}
	e.expectError(exitFailure, []string{"net", "on"}, "error: the helper", "hint: install it with `sudo gst net install`")
	if e.load().Net == nil {
		t.Fatal("the network should start once the helper is there")
	}
}

func TestNetOff(t *testing.T) {
	e := newEnv(t)
	e.back.net = runningNet()
	e.mustGst("net", "on")
	e.mustGst("net", "off")
	if e.load().Net != nil {
		t.Fatal("the network stayed on")
	}
	contains(t, e.mustGst("net", "status"), "The virtual network is off", "gst net on")
	if v := decode[netView](t, e.mustGst("net", "status", "--json")); v.On {
		t.Fatalf("view: %+v", v)
	}
}

func TestNetStatus(t *testing.T) {
	e := newEnv(t)
	e.back.net = runningNet()
	e.mustGst("net", "on")
	out := e.mustGst("net", "status")
	contains(t, out, "Address: 10.99.1.2", "Subnet: 10.99.0.0/16", "Interface: gst0", "1m0s ago", "2.0 KiB", "5 B", "never")
	found := false
	for _, row := range rows(out) {
		if len(row) > 2 && row[0] == "devbox" {
			found = row[1] == "10.99.3.4" && row[2] == "direct"
		}
	}
	if !found {
		t.Fatalf("no line of devbox:\n%s", out)
	}
	v := decode[netView](t, e.mustGst("net", "status", "--json"))
	if !v.On || v.NetState == nil || v.Address != "10.99.1.2" || len(v.Peers) != 2 {
		t.Fatalf("view: %+v", v)
	}

	e.back.net.Error, e.back.net.Hint = "the subnet overlaps", "choose another"
	contains(t, e.mustGst("net", "status"), "# Problem: the subnet overlaps", "# Hint: choose another")
}

func TestStatusShowsTheNetwork(t *testing.T) {
	e := newEnv(t)
	e.back.net = runningNet()
	e.mustGst("net", "on")
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

	e.back.net = &wire.NetState{Error: "the subnet overlaps"}
	contains(t, e.mustGst("status"), "# Problems:", "virtual network: the subnet overlaps")

	e.back.net = runningNet()
	contains(t, e.mustGst("status", "devbox"), "Net: 10.99.1.2 (subnet 10.99.0.0/16)")
}

func TestNetInstallNeedsRoot(t *testing.T) {
	e := newEnv(t)
	e.expectError(exitFailure, []string{"net", "install"}, "needs root", "sudo gst net install")
	e.expectError(exitFailure, []string{"net", "uninstall"}, "needs root", "sudo gst net uninstall")
}
