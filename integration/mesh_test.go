//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
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
