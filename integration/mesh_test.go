//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

type meshPeers struct {
	Enabled bool `json:"enabled"`
	Node    struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"node"`
	Links []struct {
		Direction string `json:"direction"`
		Tunnel    string `json:"tunnel"`
		State     string `json:"state"`
		Error     string `json:"error"`
		Peer      struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"peer"`
	} `json:"links"`
}

func (n *node) peers() meshPeers {
	n.t.Helper()
	var p meshPeers
	if err := json.Unmarshal([]byte(n.mustGst("mesh", "peers", "--json")), &p); err != nil {
		n.t.Fatal(err)
	}
	return p
}

// links summarizes the peer links as "direction tunnel state peer".
func (p meshPeers) links() []string {
	var out []string
	for _, l := range p.Links {
		out = append(out, strings.TrimSpace(fmt.Sprintf("%s %s %s %s", l.Direction, l.Tunnel, l.State, l.Peer.Name)))
	}
	return out
}

// waitLinks waits until n's links are exactly want, in order.
func (n *node) waitLinks(want ...string) meshPeers {
	n.t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for {
		p := n.peers()
		if strings.Join(p.links(), "\n") == strings.Join(want, "\n") {
			return p
		}
		if time.Now().After(deadline) {
			n.t.Fatalf("links of %s: %q; want %q (%+v)", n.container, p.links(), want, p.Links)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// m3 has mesh tunnels to mini and devcn; each machine sees its peers.
func TestMeshLinksThreeNodes(t *testing.T) {
	t.Parallel()
	m3, mini, devcn := startNode(t), startNode(t), startNode(t)
	toMini, toDevcn := trust(t, m3, mini), trust(t, m3, devcn)
	for n, name := range map[*node]string{m3: "m3", mini: "mini", devcn: "devcn"} {
		n.mustGst("mesh", "enable", "--name", name)
	}
	m3.mustGst(append([]string{"add", "mini"}, append(toMini.flags(), "--mesh")...)...)
	m3.mustGst(append([]string{"add", "devcn"}, append(toDevcn.flags(), "--mesh")...)...)

	p := m3.waitLinks("out devcn up devcn", "out mini up mini")
	mini.waitLinks("in  up m3")
	devcn.waitLinks("in  up m3")
	if in := mini.peers().Links[0]; in.Peer.ID != p.Node.ID {
		t.Fatalf("mini sees m3 as %+v; m3 is %+v", in.Peer, p.Node)
	}

	// The tunnel log records the link without naming the server.
	log := m3.mustSh(`cat ~/.local/state/gst/logs/mini.log`)
	if !strings.Contains(log, "peer link up") || strings.Contains(log, mini.ip) {
		t.Fatalf("unexpected tunnel log:\n%s", log)
	}

	// A server that leaves the mesh refuses the link until it rejoins.
	mini.mustGst("mesh", "disable")
	eventually(t, 20*time.Second, "link to mini not down", func() bool {
		for _, l := range m3.peers().Links {
			if l.Tunnel == "mini" {
				return l.State == "down" && strings.Contains(l.Error, "gst mesh enable")
			}
		}
		return false
	})
	if status := m3.tunnels()["mini"].Status; status != "running" {
		t.Fatalf("the tunnel is %s without its peer link", status)
	}
	mini.mustGst("mesh", "enable", "--name", "mini")
	m3.waitLinks("out devcn up devcn", "out mini up mini")
}

// A server without gst in PATH reports why the link fails; --remote-gst
// names it.
func TestMeshRemoteGst(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	p.client.mustGst("mesh", "enable", "--name", "client")
	p.server.mustGst("mesh", "enable", "--name", "server")
	if _, err := p.server.exec("root", "mv", "/usr/local/bin/gst", "/opt/gst"); err != nil {
		t.Fatal(err)
	}
	p.client.mustGst(append([]string{"add", "srv"}, append(p.flags(), "--mesh")...)...)
	p.client.waitStatus("srv", "running")
	eventually(t, 20*time.Second, "missing link error", func() bool {
		links := p.client.peers().Links
		return len(links) == 1 && links[0].State == "down" && strings.Contains(links[0].Error, "not found")
	})
	if status := p.client.tunnels()["srv"].Status; status != "running" {
		t.Fatalf("a failed peer link took the tunnel %s", status)
	}

	p.client.mustGst("set", "srv", "--remote-gst", "/opt/gst")
	p.client.waitLinks("out srv up server")
}

func (n *node) nodes() wire.Topology {
	n.t.Helper()
	var topo wire.Topology
	if err := json.Unmarshal([]byte(n.mustGst("mesh", "nodes", "--json")), &topo); err != nil {
		n.t.Fatal(err)
	}
	return topo
}

// summary describes a topology as sorted lines: "node <name> gst|ssh
// reachable|unreachable" and "edge <from> <to> <tunnel> <state> [linked]".
func summary(topo wire.Topology) []string {
	names := map[string]string{}
	var lines []string
	for _, n := range topo.Nodes {
		names[n.ID] = n.Name
		kind, reach := "ssh", "unreachable"
		if n.Gst {
			kind = "gst"
		}
		if n.Reachable {
			reach = "reachable"
		}
		lines = append(lines, fmt.Sprintf("node %s %s %s", n.Name, kind, reach))
	}
	for _, e := range topo.Edges {
		line := fmt.Sprintf("edge %s %s %s %s", names[e.From], names[e.To], e.Tunnel, e.State)
		if e.Linked {
			line += " linked"
		}
		lines = append(lines, line)
	}
	slices.Sort(lines)
	return lines
}

// waitNodes waits until n's summary of the mesh is want.
func (n *node) waitNodes(want ...string) wire.Topology {
	n.t.Helper()
	slices.Sort(want)
	deadline := time.Now().Add(40 * time.Second)
	for {
		topo := n.nodes()
		got := summary(topo)
		if slices.Equal(got, want) {
			return topo
		}
		if time.Now().After(deadline) {
			n.t.Fatalf("mesh seen from %s:\n%s\nwant:\n%s", n.container, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// m3 has mesh tunnels to mini and devcn. mini sees devcn through m3: first
// as an SSH server, then, once devcn joins the mesh, as a gst node.
func TestMeshTopology(t *testing.T) {
	t.Parallel()
	m3, mini, devcn := startNode(t), startNode(t), startNode(t)
	toMini, toDevcn := trust(t, m3, mini), trust(t, m3, devcn)
	m3.mustGst("mesh", "enable", "--name", "m3")
	mini.mustGst("mesh", "enable", "--name", "mini")
	m3.mustGst(append([]string{"add", "mini"}, append(toMini.flags(), "--mesh")...)...)
	m3.mustGst(append([]string{"add", "devcn"}, append(toDevcn.flags(), "--mesh")...)...)

	topo := mini.waitNodes(
		"node mini gst reachable",
		"node m3 gst reachable",
		"node devcn ssh reachable",
		"edge m3 mini mini running linked",
		"edge m3 devcn devcn running",
	)
	for _, n := range topo.Nodes {
		if n.Name == "devcn" && n.ID != "ssh:"+ssh.FingerprintSHA256(devcn.hostKey) {
			t.Fatalf("devcn is %s; its host key is %s", n.ID, ssh.FingerprintSHA256(devcn.hostKey))
		}
		if n.Name == "mini" && !slices.Contains(n.HostKeys, ssh.FingerprintSHA256(mini.hostKey)) {
			t.Fatalf("mini announces host keys %q", n.HostKeys)
		}
	}
	if out := mini.mustGst("mesh", "nodes"); !strings.Contains(out, "this machine") || !strings.Contains(out, "peer link up") {
		t.Fatalf("unexpected gst mesh nodes:\n%s", out)
	}

	devcn.mustGst("mesh", "enable", "--name", "devcn")
	mini.waitNodes(
		"node mini gst reachable",
		"node m3 gst reachable",
		"node devcn gst reachable",
		"edge m3 mini mini running linked",
		"edge m3 devcn devcn running linked",
	)

	// Without the tunnel, devcn's record stays until it expires, but mini
	// can no longer reach it.
	m3.mustGst("stop", "devcn")
	mini.waitNodes(
		"node mini gst reachable",
		"node m3 gst reachable",
		"node devcn gst unreachable",
		"edge m3 mini mini running linked",
	)

	// The background logs mention neither the mesh's machines nor addresses.
	// The systemctl shim keeps the daemon's own log.
	var logs string
	for _, n := range []*node{m3, mini, devcn} {
		logs += n.mustSh(`cat ~/.systemctl-shim/gst.service.log ~/.local/state/gst/logs/*.log 2>/dev/null; true`)
	}
	for _, secret := range []string{mini.ip, devcn.ip, "SHA256:"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs mention %q:\n%s", secret, logs)
		}
	}
}
