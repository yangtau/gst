package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func id(name string) string { return fmt.Sprintf("%032x", []byte(name)) }

// describe summarizes a topology as "name[gst][reach]" nodes and
// "from>to:tunnel:state[:linked]" edges, with names for ids.
func describe(t wire.Topology) string {
	names := map[string]string{}
	var nodes, edges []string
	for _, n := range t.Nodes {
		names[n.ID] = n.Name
		s := n.Name
		if n.Gst {
			s += "+gst"
		}
		if n.Reachable {
			s += "+reach"
		}
		nodes = append(nodes, s)
	}
	for _, e := range t.Edges {
		s := names[e.From] + ">" + names[e.To] + ":" + e.Tunnel + ":" + e.State
		if e.Linked {
			s += ":linked"
		}
		edges = append(edges, s)
	}
	return strings.Join(nodes, " ") + " | " + strings.Join(edges, " ")
}

// m3 links to mini and devcn; mini learns about devcn through m3.
func TestTopologySpreads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m3, mini, devcn := newNode("m3"), newNode("mini"), newNode("devcn")
	m3.SetEdges(func() []wire.Edge {
		return []wire.Edge{
			{Tunnel: "devcn", Destination: "devcn.example.com", State: wire.StateRunning, HostKey: "SHA256:devcn"},
			{Tunnel: "mini", Destination: "mini", State: wire.StateRunning, HostKey: "SHA256:mini"},
		}
	})
	for _, n := range []*Node{m3, mini, devcn} {
		go n.Run(ctx)
		n.Update()
	}
	connectOver(ctx, m3, mini, "mini")
	connectOver(ctx, m3, devcn, "devcn")

	want := "mini+gst+reach devcn+gst+reach m3+gst+reach | m3>devcn:devcn:running:linked m3>mini:mini:running:linked"
	waitFor(t, "mini does not see the mesh", func() bool { return describe(mini.Topology()) == want })
	if got := describe(devcn.Topology()); got != "devcn+gst+reach m3+gst+reach mini+gst+reach | m3>devcn:devcn:running:linked m3>mini:mini:running:linked" {
		t.Fatalf("devcn sees %s", got)
	}
	topo := mini.Topology()
	if topo.Self != mini.self().ID || !topo.Enabled {
		t.Fatalf("topology %+v", topo)
	}

	// Disabling the mesh forgets the others' records.
	mini.Configure(nil)
	if topo := mini.Topology(); topo.Enabled || len(topo.Nodes) != 0 {
		t.Fatalf("disabled node reports %+v", topo)
	}
}

func TestBuildTopology(t *testing.T) {
	now := time.Now()
	live := now.Add(time.Hour)
	self := wire.NodeInfo{ID: id("mini"), Name: "mini", HostKeys: []string{"SHA256:mini"}}
	recs := []wire.Record{
		{Node: self, Seq: 1, Expires: live, Edges: []wire.Edge{
			// A tunnel to a server that has never answered.
			{Tunnel: "new", State: wire.StateConnecting},
		}},
		{Node: wire.NodeInfo{ID: id("m3"), Name: "m3"}, Seq: 1, Expires: live, Edges: []wire.Edge{
			// Linked: leads to the peer.
			{Tunnel: "mini", State: wire.StateRunning, HostKey: "SHA256:other", Peer: id("mini"), Linked: true},
			// Not linked, but the host key is devcn's: leads to devcn,
			// which mini reaches but cannot relay through.
			{Tunnel: "devcn", State: wire.StateRunning, HostKey: "SHA256:devcn"},
			// No gst on the server.
			{Tunnel: "db", State: wire.StateRunning, HostKey: "SHA256:db"},
			{Tunnel: "db-again", State: wire.StateDown, HostKey: "SHA256:db"},
		}},
		{Node: wire.NodeInfo{ID: id("devcn"), Name: "devcn", HostKeys: []string{"SHA256:devcn"}}, Seq: 1, Expires: live, Edges: []wire.Edge{
			{Tunnel: "far", State: wire.StateRunning, HostKey: "SHA256:far"},
		}},
		{Node: wire.NodeInfo{ID: id("gone"), Name: "gone"}, Seq: 1, Expires: now.Add(-time.Second)},
	}
	got := describe(buildTopology(self, recs, now))
	// Edges are sorted by node id, then tunnel; ids of shorter names sort
	// first here.
	want := "mini+gst+reach devcn+gst+reach m3+gst+reach db+reach far new | " +
		"m3>db:db:running m3>db:db-again:down m3>devcn:devcn:running m3>mini:mini:running:linked mini>new:new:connecting devcn>far:far:running"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestMergeKeepsNewest(t *testing.T) {
	n := newNode("mini")
	live := time.Now().Add(time.Hour)
	rec := func(name string, seq uint64) wire.Record {
		return wire.Record{Node: wire.NodeInfo{ID: id(name), Name: fmt.Sprint(name, seq)}, Seq: seq, Expires: live}
	}
	n.merge([]wire.Record{rec("m3", 2)})
	n.merge([]wire.Record{rec("m3", 1), {Node: wire.NodeInfo{ID: "bad"}, Seq: 9, Expires: live}})
	if r := n.records[id("m3")]; r.Node.Name != "m32" || len(n.records) != 1 {
		t.Fatalf("records %+v", n.records)
	}

	// Peers pass this node's own record back; that changes nothing.
	n.publish(false)
	select { // drop the request from Configure
	case <-n.update:
	default:
	}
	own := n.records[id("mini")]
	n.merge([]wire.Record{own})
	select {
	case <-n.update:
		t.Fatal("an echo of this node's record made it publish again")
	default:
	}

	// A record of this node from an earlier run makes it outdo that one.
	future := n.records[id("mini")].Seq + uint64(time.Hour)
	n.merge([]wire.Record{rec("mini", future)})
	<-n.update
	n.publish(false)
	if seq := n.records[id("mini")].Seq; seq <= future {
		t.Fatalf("own seq %d, earlier run %d", seq, future)
	}
}

func TestBatch(t *testing.T) {
	big := wire.Record{Node: wire.NodeInfo{ID: id("big")}, Edges: make([]wire.Edge, 1000)}
	for i := range big.Edges {
		big.Edges[i] = wire.Edge{Tunnel: fmt.Sprintf("tunnel-%d", i), Destination: strings.Repeat("d", 200), State: wire.StateRunning}
	}
	if !fits(big) {
		t.Fatal("a record of 1000 edges does not fit in a batch")
	}
	batches := batch([]wire.Record{big, big, big, big})
	if len(batches) < 2 {
		t.Fatalf("%d batches", len(batches))
	}
	total := 0
	for _, b := range batches {
		raw, _ := json.Marshal(wire.Records{Records: b})
		if len(raw) > wire.MaxFrame {
			t.Fatalf("batch of %d bytes", len(raw))
		}
		total += len(b)
	}
	if total != 4 {
		t.Fatalf("%d records in batches", total)
	}
	if b := batch(nil); len(b) != 1 || len(b[0]) != 0 {
		t.Fatalf("batches of nothing: %v", b)
	}

	huge := big
	huge.Edges = slices.Repeat(big.Edges, 4)[:maxEdges]
	huge.Edges[0].Destination = strings.Repeat("d", wire.MaxFrame)
	n := newNode("mini")
	huge.Seq, huge.Expires = 1, time.Now().Add(time.Hour)
	n.merge([]wire.Record{huge})
	if _, ok := n.records[huge.Node.ID]; ok {
		t.Fatal("kept a record too large to pass on")
	}
}
