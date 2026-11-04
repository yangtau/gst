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
	m3.mustGst(append([]string{"add", "mini"}, toMini.flags()...)...)
	m3.mustGst(append([]string{"add", "devcn"}, toDevcn.flags()...)...)

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
// names it, and --no-mesh turns the link off.
func TestMeshRemoteGst(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	p.client.mustGst("mesh", "enable", "--name", "client")
	p.server.mustGst("mesh", "enable", "--name", "server")
	if _, err := p.server.exec("root", "mv", "/usr/local/bin/gst", "/opt/gst"); err != nil {
		t.Fatal(err)
	}
	p.client.mustGst(append([]string{"add", "srv"}, p.flags()...)...)
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

	// --no-mesh keeps a tunnel out of the mesh.
	p.client.mustGst("set", "srv", "--no-mesh")
	eventually(t, 20*time.Second, "link kept with --no-mesh", func() bool { return len(p.client.peers().Links) == 0 })
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

// uiServer is gst ui running on a node.
type uiServer struct {
	n           *node
	base, token string
}

// startUI starts gst ui on n and reads its address.
func (n *node) startUI() *uiServer {
	n.t.Helper()
	n.mustSh(`setsid gst ui --no-open --port 8370 </dev/null >~/ui.out 2>&1 &`)
	var url string
	eventually(n.t, 10*time.Second, "gst ui printed no address", func() bool {
		out, _ := n.sh(`cat ~/ui.out`)
		_, rest, ok := strings.Cut(out, "gst ui: ")
		url, _, _ = strings.Cut(rest, "\n")
		return ok && strings.Contains(url, "#")
	})
	base, token, _ := strings.Cut(url, "#")
	return &uiServer{n, base, token}
}

// request sends an HTTP request to gst ui, with its token or without, and
// returns the status code and body.
func (u *uiServer) request(method, path string, withToken bool) (string, string) {
	u.n.t.Helper()
	args := []string{"http", method, u.base + path}
	if withToken {
		args = append(args, u.token)
	}
	out, err := u.n.helper(args...)
	if err != nil {
		u.n.t.Fatalf("gst ui %s %s: %v", method, path, err)
	}
	code, body, _ := strings.Cut(out, "\n")
	return code, body
}

// m3 has mesh tunnels to mini and devcn. mini sees devcn through m3: first
// as an SSH server, then, once devcn joins the mesh, as a gst node.
func TestMeshTopology(t *testing.T) {
	t.Parallel()
	m3, mini, devcn := startNode(t), startNode(t), startNode(t)
	toMini, toDevcn := trust(t, m3, mini), trust(t, m3, devcn)
	m3.mustGst("mesh", "enable", "--name", "m3")
	mini.mustGst("mesh", "enable", "--name", "mini")
	m3.mustGst(append([]string{"add", "mini"}, toMini.flags()...)...)
	m3.mustGst(append([]string{"add", "devcn"}, toDevcn.flags()...)...)

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

	// gst ui on m3 serves the same view, to holders of the token it prints.
	ui := m3.startUI()
	if code, _ := ui.request("GET", "api/state", false); code != "401" {
		t.Fatalf("state without the token: %s", code)
	}
	code, body := ui.request("GET", "api/state", true)
	var state struct {
		Topology wire.Topology `json:"topology"`
	}
	if code != "200" || json.Unmarshal([]byte(body), &state) != nil {
		t.Fatalf("state: %s %q", code, body)
	}
	if got := summary(state.Topology); !slices.Contains(got, "node devcn ssh reachable") || !slices.Contains(got, "edge m3 mini mini running linked") {
		t.Fatalf("gst ui shows:\n%s", strings.Join(got, "\n"))
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
	// Stopped from the page, as gst stop would.
	if code, _ := ui.request("POST", "api/tunnels/devcn/stop", false); code != "401" {
		t.Fatalf("stop without the token: %s", code)
	}
	if code, body := ui.request("POST", "api/tunnels/devcn/stop", true); code != "200" || !strings.Contains(body, "Stopped tunnel devcn.") {
		t.Fatalf("stop from gst ui: %s %q", code, body)
	}
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

// mini reaches devcn through m3: first as an SSH server without gst, over
// m3's tunnel, and then, once devcn joins the mesh, through devcn's gst.
// mini needs no credentials for devcn, except to log in to it with ssh.
func TestMeshRelay(t *testing.T) {
	t.Parallel()
	m3, mini, devcn := startNode(t), startNode(t), startNode(t)
	toMini, toDevcn := trust(t, m3, mini), trust(t, m3, devcn)
	m3.mustGst("mesh", "enable", "--name", "m3")
	mini.mustGst("mesh", "enable", "--name", "mini")
	m3.mustGst(append([]string{"add", "mini"}, toMini.flags()...)...)
	m3.mustGst(append([]string{"add", "devcn"}, toDevcn.flags()...)...)
	devcn.serve(7100, "hello from devcn")
	mini.waitNodes(
		"node mini gst reachable",
		"node m3 gst reachable",
		"node devcn ssh reachable",
		"edge m3 mini mini running linked",
		"edge m3 devcn devcn running",
	)

	mini.mustGst("add", "devcn-app", "mesh:devcn", "-L", "17100:localhost:7100", "-D", "11080")
	mini.waitStatus("devcn-app", "running")
	mini.eventuallyFetches(17100, "hello from devcn")
	if out := mini.mustGst("check", "devcn-app"); !strings.Contains(out, "mini → m3 → devcn") {
		t.Fatalf("unexpected check:\n%s", out)
	}
	if out, err := mini.helper("socks", "127.0.0.1:11080", "localhost", "7100"); err != nil || strings.TrimSpace(out) != "hello from devcn" {
		t.Fatalf("SOCKS through the mesh: %q, %v", out, err)
	}
	if out := mini.mustSh(`gst connect devcn 7100 </dev/null`); out != "hello from devcn" {
		t.Fatalf("gst connect: %q", out)
	}

	// As an ssh ProxyCommand, with mini's own login to devcn.
	login := trust(t, mini, devcn)
	out := mini.mustSh(`ssh -i "$1" -o UserKnownHostsFile="$2" -o BatchMode=yes -o ProxyCommand='gst connect devcn 22' "tunnel@$3" echo logged in`,
		login.identity, login.knownHosts, devcn.ip)
	if strings.TrimSpace(out) != "logged in" {
		t.Fatalf("ssh through the mesh: %q", out)
	}

	devcn.mustGst("mesh", "enable", "--name", "devcn")
	mini.waitNodes(
		"node mini gst reachable",
		"node m3 gst reachable",
		"node devcn gst reachable",
		"edge m3 mini mini running linked",
		"edge m3 devcn devcn running linked",
	)
	mini.eventuallyFetches(17100, "hello from devcn")

	// An unreachable machine fails each connection, not the tunnel.
	m3.mustGst("stop", "devcn")
	eventually(t, 20*time.Second, "route to devcn survived", func() bool {
		code, _, _ := mini.gst("check", "devcn-app")
		return code == 1
	})
	if got, _ := mini.fetch(17100); got != "" {
		t.Fatalf("fetched %q without a route", got)
	}
	if status := mini.tunnels()["devcn-app"].Status; status != "running" {
		t.Fatalf("the tunnel is %s without a route", status)
	}
	log := mini.mustSh(`cat ~/.local/state/gst/logs/devcn-app.log`)
	if !strings.Contains(log, "target connection failed for forward 1") || strings.Contains(log, devcn.ip) {
		t.Fatalf("unexpected tunnel log:\n%s", log)
	}
}
