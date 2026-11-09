package mesh

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

var now = time.Now()

// rec returns the live record of the node called name, with links to the
// named nodes (link name = peer name).
func rec(name string, peers ...string) wire.Record {
	r := wire.Record{Node: wire.NodeInfo{ID: nid(name), Name: name, Software: "gst test"}, Seq: 1, Expires: now.Add(time.Hour), Links: []wire.Edge{}}
	for _, p := range peers {
		r.Links = append(r.Links, wire.Edge{Link: config.LinkID(p), Peer: nid(p)})
	}
	return r
}

// accepted adds to recs the links that their nodes accepted, as each node
// lists in its own record.
func accepted(recs []wire.Record) []wire.Record {
	out := slices.Clone(recs)
	for i := range out {
		out[i].Links = slices.Clone(out[i].Links)
	}
	for _, r := range recs {
		for _, e := range r.Links {
			for i := range out {
				if out[i].Node.ID == e.Peer {
					out[i].Links = append(out[i].Links, wire.Edge{Peer: r.Node.ID})
				}
			}
		}
	}
	return out
}

func ids(names ...string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = nid(n)
	}
	return out
}

func TestBuildTopology(t *testing.T) {
	selfNoExpiry := rec("a", "b")
	selfNoExpiry.Expires = time.Time{}

	for _, tt := range []struct {
		name  string
		self  string
		recs  []wire.Record
		paths map[string][]string // reachable nodes by name -> path by names; absent names are unreachable
		nodes []string            // all nodes, in order
		edges int
	}{
		{
			name:  "chain with a placeholder for an unknown peer",
			self:  "a",
			recs:  []wire.Record{selfNoExpiry, rec("b", "c"), rec("lonely")},
			paths: map[string][]string{"a": {"a"}, "b": {"a", "b"}, "c": {"a", "b", "c"}},
			nodes: []string{"a", nid("c")[:8], "b", "lonely"}, // c is named by its id until its record arrives
			edges: 2,
		},
		{
			name:  "links carry connections both ways",
			self:  "a",
			recs:  []wire.Record{rec("a"), rec("b", "a"), rec("c", "b")},
			paths: map[string][]string{"a": {"a"}, "b": {"a", "b"}, "c": {"a", "b", "c"}},
			nodes: []string{"a", "b", "c"},
			edges: 2,
		},
		{
			name:  "the shortest path wins",
			self:  "a",
			recs:  []wire.Record{rec("a", "b", "d"), rec("b", "c"), rec("c", "d"), rec("d")},
			paths: map[string][]string{"a": {"a"}, "b": {"a", "b"}, "c": {"a", "b", "c"}, "d": {"a", "d"}},
			nodes: []string{"a", "b", "c", "d"},
			edges: 4,
		},
		{
			name:  "a cycle terminates",
			self:  "b",
			recs:  []wire.Record{rec("a", "b"), rec("b", "c"), rec("c", "a")},
			paths: map[string][]string{"a": {"b", "a"}, "b": {"b"}, "c": {"b", "c"}},
			nodes: []string{"b", "a", "c"},
			edges: 3,
		},
		{
			name:  "unlinked nodes are unreachable",
			self:  "a",
			recs:  []wire.Record{rec("a"), rec("b", "c"), rec("c")},
			paths: map[string][]string{"a": {"a"}},
			nodes: []string{"a", "b", "c"},
			edges: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			topo := buildTopology(nid(tt.self), accepted(tt.recs), now)
			if topo.Self != nid(tt.self) {
				t.Fatalf("self = %s", topo.Self)
			}
			if len(topo.Edges) != tt.edges {
				t.Fatalf("edges = %+v, want %d", topo.Edges, tt.edges)
			}
			var got []string
			for _, n := range topo.Nodes {
				got = append(got, n.Name)
			}
			if !slices.Equal(got, tt.nodes) {
				t.Fatalf("nodes = %v, want %v", got, tt.nodes)
			}
			paths := map[string][]string{}
			for name, path := range tt.paths {
				paths[nid(name)] = path
			}
			for _, n := range topo.Nodes {
				want, reachable := paths[n.ID]
				if n.ID == nid(tt.self) {
					want, reachable = []string{tt.self}, true
				}
				if n.Reachable != reachable {
					t.Errorf("%s reachable = %v, want %v", n.Name, n.Reachable, reachable)
				}
				if reachable && !slices.Equal(n.Path, ids(want...)) {
					t.Errorf("%s path = %v, want %v", n.Name, n.Path, want)
				}
				if !reachable && n.Path != nil {
					t.Errorf("%s has path %v", n.Name, n.Path)
				}
			}
		})
	}
}

func TestBuildTopologyIgnoresExpiredRecordsInAnyOrder(t *testing.T) {
	expired := rec("old", "a")
	expired.Expires = now.Add(-time.Second)
	for _, recs := range [][]wire.Record{{rec("a", "old"), expired}, {expired, rec("a", "old")}} {
		topo := buildTopology(nid("a"), recs, now)
		if len(topo.Edges) != 1 || topo.Edges[0].To != nid("old") {
			t.Fatalf("edges = %+v, want only a's link to the expired node", topo.Edges)
		}
	}
}

// A link that the node at its other end no longer lists is gone, though
// the opening node's record, which cannot reach the others, still lists it.
func TestBuildTopologyNeedsBothEnds(t *testing.T) {
	topo := buildTopology(nid("b"), []wire.Record{rec("a", "b"), rec("b")}, now)
	if len(topo.Edges) != 0 || topo.Nodes[1].Reachable {
		t.Fatalf("topology = %+v, want a unreachable", topo)
	}
	topo = buildTopology(nid("b"), accepted([]wire.Record{rec("a", "b"), rec("b")}), now)
	if len(topo.Edges) != 1 || !topo.Nodes[1].Reachable {
		t.Fatalf("topology = %+v, want a reachable", topo)
	}
}

func TestBuildTopologySortsAndCarriesNodeInfo(t *testing.T) {
	r := rec("zed")
	topo := buildTopology(nid("zed"), accepted([]wire.Record{rec("bob", "alice"), rec("alice"), r}), now)
	var names []string
	for _, n := range topo.Nodes {
		names = append(names, n.Name)
	}
	if !slices.Equal(names, []string{"zed", "alice", "bob"}) {
		t.Fatalf("order = %v, want self first, then by name", names)
	}
	if topo.Nodes[1].Software != "gst test" || topo.Nodes[1].Expires.IsZero() {
		t.Fatalf("nodes = %+v", topo.Nodes)
	}
	if e := topo.Edges[0]; e.From != nid("bob") || e.To != nid("alice") || e.Link != config.LinkID("alice") {
		t.Fatalf("edge = %+v", e)
	}
}

func TestMerge(t *testing.T) {
	future := time.Now().Add(time.Hour)
	mk := func(name string, seq uint64, mod func(*wire.Record)) wire.Record {
		r := wire.Record{Node: wire.NodeInfo{ID: nid(name), Name: name}, Seq: seq, Expires: future}
		if mod != nil {
			mod(&r)
		}
		return r
	}
	manyLinks := func(n int) func(*wire.Record) {
		return func(r *wire.Record) {
			for i := 0; i < n; i++ {
				r.Links = append(r.Links, wire.Edge{Link: config.LinkID("x"), Peer: nid("p")})
			}
		}
	}
	for _, tt := range []struct {
		name string
		recs []wire.Record
		want map[string]uint64 // seq by node name afterwards
	}{
		{"stores", []wire.Record{mk("b", 5, nil)}, map[string]uint64{"b": 5}},
		{"newer replaces", []wire.Record{mk("b", 5, nil), mk("b", 7, nil)}, map[string]uint64{"b": 7}},
		{"older is ignored", []wire.Record{mk("b", 5, nil), mk("b", 3, nil), mk("b", 5, nil)}, map[string]uint64{"b": 5}},
		{"invalid id", []wire.Record{{Node: wire.NodeInfo{ID: "zz", Name: "b"}, Seq: 1, Expires: future}}, nil},
		{"expired", []wire.Record{mk("b", 1, func(r *wire.Record) { r.Expires = time.Now().Add(-time.Second) })}, nil},
		{"too many links", []wire.Record{mk("b", 1, manyLinks(maxEdges+1))}, nil},
		{"max links", []wire.Record{mk("b", 1, manyLinks(maxEdges))}, map[string]uint64{"b": 1}},
		{"too big to pass on", []wire.Record{mk("b", 1, func(r *wire.Record) { r.Node.Software = strings.Repeat("x", maxBatch) })}, nil},
		{"others still merge after a bad one", []wire.Record{mk("b", 1, manyLinks(maxEdges+1)), mk("c", 2, nil)}, map[string]uint64{"c": 2}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := newNode(t, "a")
			n.merge(tt.recs)
			got := map[string]uint64{}
			n.mu.Lock()
			for _, r := range n.records {
				if r.Node.ID != nid("a") {
					got[r.Node.Name] = r.Seq
				}
			}
			n.mu.Unlock()
			if len(got) != len(tt.want) {
				t.Fatalf("records = %v, want %v", got, tt.want)
			}
			for name, seq := range tt.want {
				if got[name] != seq {
					t.Fatalf("records = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestMergeLimitsRecords(t *testing.T) {
	n := New("gst test", nil, nil)
	n.info = wire.NodeInfo{ID: nid("a"), Name: "a"}
	future := time.Now().Add(time.Hour)
	for i := range maxRecords + 10 {
		id := fmt.Sprintf("%032x", i+1)
		n.merge([]wire.Record{{Node: wire.NodeInfo{ID: id, Name: "n"}, Seq: 1, Expires: future}})
	}
	if len(n.records) != maxRecords {
		t.Fatalf("%d records, want %d", len(n.records), maxRecords)
	}
	// A known node can still update its record when the table is full.
	for id := range n.records {
		n.merge([]wire.Record{{Node: wire.NodeInfo{ID: id, Name: "n"}, Seq: 2, Expires: future}})
		if n.records[id].Seq != 2 {
			t.Fatal("full table refused an update")
		}
		break
	}
}

func TestMergeWithoutIdentityIgnoresRecords(t *testing.T) {
	n := New("gst test", nil, nil)
	n.merge([]wire.Record{rec("b")})
	if len(n.records) != 0 {
		t.Fatalf("records = %v", n.records)
	}
}

func TestNewerSendsEachRecordOnce(t *testing.T) {
	n := newNode(t, "a")
	waitFor(t, "own record", func() bool { return n.known()[nid("a")] != 0 })
	n.merge([]wire.Record{rec("b"), rec("c")})
	sent := map[string]uint64{nid("b"): 1} // the caller has b already
	recs, _ := n.newer(sent)
	var names []string
	for _, r := range recs {
		names = append(names, r.Node.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"a", "c"}) {
		t.Fatalf("sent %v", names)
	}
	if again, _ := n.newer(sent); len(again) != 0 {
		t.Fatalf("sent again: %+v", again)
	}
}

func TestBatchSplitsToFitFrames(t *testing.T) {
	if got := batch(nil); len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("batch(nil) = %v, want one empty list", got)
	}
	big := func(name string) wire.Record {
		r := rec(name)
		r.Node.Software = strings.Repeat("x", maxBatch/3)
		return r
	}
	batches := batch([]wire.Record{big("a"), big("b"), big("c"), big("d")})
	total := 0
	for _, b := range batches {
		if len(b) == 0 {
			t.Fatal("empty batch")
		}
		total += len(b)
		var frame wire.Records
		frame.Records = b
		if err := wire.WriteFrame(discard{}, frame); err != nil {
			t.Fatalf("batch does not fit a frame: %v", err)
		}
	}
	if len(batches) < 2 || total != 4 {
		t.Fatalf("%d batches with %d records", len(batches), total)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// People see the machines that can be reached now, not every record that
// has yet to expire, nor the links of those records.
func TestMachinesLeaveOutWhatCannotBeReached(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	ab, _ := linkUp(t, a, b, "b"), linkUp(t, b, c, "c")
	waitFor(t, "a reaches c", func() bool { return reaches(a, "c", "a", "b", "c") })
	if m := a.Machines(); len(m.Nodes) != 3 || len(m.Edges) != 2 {
		t.Fatalf("machines: %+v", m)
	}

	ab.cut()
	ab.wait(t)
	waitFor(t, "a loses b", func() bool { n, _ := findTopo(a.Topology(), nid("b")); return !n.Reachable })
	if topo := a.Topology(); len(topo.Nodes) != 3 {
		t.Fatalf("the records should stay until they expire: %+v", topo)
	}
	m := a.Machines()
	if len(m.Nodes) != 1 || m.Nodes[0].ID != nid("a") || len(m.Edges) != 0 {
		t.Fatalf("machines: %+v", m)
	}
}
