package state

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

var quiet = log.New(io.Discard, "", 0)

func id(c byte) string { return strings.Repeat(string(c), 32) }

func value(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

func node(c byte, gen, seq int64, name string) wire.Entry {
	return wire.Entry{Key: NodeKey(id(c)), Version: []int64{gen, seq}, Value: value(wire.NodeValue{Name: name, Software: "gst", Links: []wire.NodeLink{}})}
}

func removed(c byte, gen int64, by byte, at int) wire.Entry {
	return wire.Entry{Key: RemovedKey(id(c)), Version: []int64{gen}, Value: value(wire.RemovedValue{By: id(by), At: time.Unix(int64(at), 0).UTC()})}
}

func network(subnet string, manual bool, order int64) wire.Entry {
	m := int64(0)
	if manual {
		m = 1
	}
	return wire.Entry{Key: SubnetKey, Version: []int64{m, order}, Value: value(wire.SubnetValue{Subnet: subnet})}
}

func keys(t *Table) []string {
	var list []string
	for _, e := range t.Entries() {
		list = append(list, e.Key)
	}
	return list
}

func TestMergeRules(t *testing.T) {
	tb := Open("", quiet)
	if got := tb.Merge([]wire.Entry{node('a', 0, 5, "laptop")}); len(got) != 1 {
		t.Fatal("a new node was not taken")
	}
	if got := tb.Merge([]wire.Entry{node('a', 0, 4, "old")}); len(got) != 0 {
		t.Fatal("an older entry replaced a newer one")
	}
	if got := tb.Merge([]wire.Entry{node('a', 0, 5, "laptop")}); len(got) != 0 {
		t.Fatal("the same entry counted as news")
	}
	tb.Merge([]wire.Entry{node('a', 0, 6, "renamed")})
	e, _ := tb.Get(NodeKey(id('a')))
	if e.Version[1] != 6 {
		t.Fatalf("version %v", e.Version)
	}

	// A removal takes the node out and keeps its generation out.
	if got := tb.Merge([]wire.Entry{removed('a', 0, 'b', 1)}); len(got) != 1 {
		t.Fatal("the removal was not taken")
	}
	if _, ok := tb.Get(NodeKey(id('a'))); ok {
		t.Fatal("the removed node stays")
	}
	if got := tb.Merge([]wire.Entry{node('a', 0, 99, "laptop")}); len(got) != 0 {
		t.Fatal("a removed generation came back")
	}
	// The next generation is in the network again.
	if got := tb.Merge([]wire.Entry{node('a', 1, 1, "laptop")}); len(got) != 1 {
		t.Fatal("the next generation was refused")
	}
	// A removal of an older generation does not touch it.
	tb.Merge([]wire.Entry{removed('c', 0, 'b', 1)})
	if _, ok := tb.Get(NodeKey(id('a'))); !ok {
		t.Fatal("lost the node")
	}

	// Of two removals at once, the earlier counts, whatever the order.
	early, late := removed('d', 0, 'b', 1), removed('d', 0, 'a', 2)
	for _, order := range [][]wire.Entry{{early, late}, {late, early}} {
		tb := Open("", quiet)
		tb.Merge(order)
		if e, _ := tb.Get(RemovedKey(id('d'))); string(e.Value) != string(early.Value) {
			t.Fatalf("kept %s", e.Value)
		}
	}
}

func TestNetworkRules(t *testing.T) {
	tb := Open("", quiet)
	tb.Merge([]wire.Entry{network("10.64.0.0/16", false, -200)})
	// The earlier automatic choice counts.
	if got := tb.Merge([]wire.Entry{network("10.65.0.0/16", false, -300)}); len(got) != 0 {
		t.Fatal("a later automatic choice won")
	}
	if got := tb.Merge([]wire.Entry{network("10.66.0.0/16", false, -100)}); len(got) != 1 {
		t.Fatal("an earlier automatic choice lost")
	}
	// A person's choice beats any of gst's; the later one counts.
	tb.Merge([]wire.Entry{network("10.99.0.0/16", true, 5)})
	if got := tb.Merge([]wire.Entry{network("10.70.0.0/16", false, -1000)}); len(got) != 0 {
		t.Fatal("an automatic choice beat a person's")
	}
	if got := tb.Merge([]wire.Entry{network("10.98.0.0/16", true, 6)}); len(got) != 1 {
		t.Fatal("a later choice lost")
	}
	// Equal versions: the lower subnet.
	tb.Merge([]wire.Entry{network("10.97.0.0/16", true, 6)})
	e, _ := tb.Get(SubnetKey)
	var v wire.SubnetValue
	json.Unmarshal(e.Value, &v)
	if v.Subnet != "10.97.0.0/16" {
		t.Fatalf("subnet %s", v.Subnet)
	}
}

func TestInvalidEntries(t *testing.T) {
	big := node('a', 0, 1, "laptop")
	big.Value = value(wire.NodeValue{Name: "laptop", Software: strings.Repeat("x", MaxEntry)})
	for name, e := range map[string]wire.Entry{
		"key":        {Key: "other/x", Version: []int64{1}, Value: value(1)},
		"id":         {Key: "node/abc", Version: []int64{0, 1}, Value: value(wire.NodeValue{Name: "x"})},
		"version":    {Key: NodeKey(id('a')), Version: []int64{1}, Value: value(wire.NodeValue{Name: "x"})},
		"name":       {Key: NodeKey(id('a')), Version: []int64{0, 1}, Value: value(wire.NodeValue{Name: "Bad Name"})},
		"address":    {Key: NodeKey(id('a')), Version: []int64{0, 1}, Value: value(wire.NodeValue{Name: "x", Net: &wire.NodeNet{Address: "nope", PublicKey: strings.Repeat("00", 32)}})},
		"subnet":     network("8.8.0.0/16", true, 1),
		"kind":       {Key: SubnetKey, Version: []int64{2, 1}, Value: value(wire.SubnetValue{Subnet: "10.99.0.0/16"})},
		"removed by": {Key: RemovedKey(id('a')), Version: []int64{0}, Value: value(wire.RemovedValue{By: "x"})},
		"size":       big,
	} {
		if got := Open("", quiet).Merge([]wire.Entry{e}); len(got) != 0 {
			t.Errorf("%s: an invalid entry was taken", name)
		}
	}
}

func TestLimits(t *testing.T) {
	tb := Open("", quiet)
	var list []wire.Entry
	for i := range MaxNodes + 5 {
		list = append(list, wire.Entry{Key: NodeKey(strings.Repeat("0", 28) + hex4(i)), Version: []int64{0, 1}, Value: value(wire.NodeValue{Name: "n"})})
	}
	tb.Merge(list)
	if n := len(tb.Entries()); n != MaxNodes {
		t.Fatalf("%d entries", n)
	}
}

func hex4(i int) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[i>>12&15], digits[i>>8&15], digits[i>>4&15], digits[i&15]})
}

// Any order of the same entries ends in the same table.
func TestOrderDoesNotMatter(t *testing.T) {
	all := []wire.Entry{
		node('a', 0, 1, "a"), node('a', 0, 3, "a2"), node('a', 1, 1, "a3"), removed('a', 0, 'b', 1),
		node('b', 0, 2, "b"), removed('b', 0, 'a', 5), removed('b', 0, 'c', 4),
		node('c', 0, 1, "c"), node('c', 0, 2, "c2"),
		network("10.64.0.0/16", false, -5), network("10.65.0.0/16", false, -9), network("10.66.0.0/16", true, 3),
	}
	want := ""
	for range 50 {
		list := slices.Clone(all)
		rand.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
		tb := Open("", quiet)
		for _, e := range list {
			tb.Merge([]wire.Entry{e, e})
		}
		got, _ := json.Marshal(tb.Entries())
		if want == "" {
			want = string(got)
		} else if string(got) != want {
			t.Fatalf("order changed the table:\n%s\n%s", got, want)
		}
	}
	if !strings.Contains(want, `"node/`+id('a')) || strings.Contains(want, `"node/`+id('b')) {
		t.Fatalf("table %s", want)
	}
}

func TestSaveAndOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	tb := Open(path, quiet)
	tb.Reset(id('a'))
	tb.Merge([]wire.Entry{node('a', 0, 1, "laptop"), network("10.64.0.0/16", false, -1)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tb.Run(ctx); close(done) }()
	cancel()
	<-done
	again := Open(path, quiet)
	if again.Owner() != id('a') || len(again.Entries()) != 2 {
		t.Fatalf("reopened %s %v", again.Owner(), keys(again))
	}
}

// mesh connects floods with in-memory links that count frames.
type mesh struct {
	mu     sync.Mutex
	nodes  map[string]*Flood
	frames int
}

func (m *mesh) connect(a, b string) (cut func()) {
	send := func(from, to string) func([]byte) bool {
		return func(body []byte) bool {
			m.mu.Lock()
			m.frames++
			dest := m.nodes[to]
			m.mu.Unlock()
			// Deliver in the background, as a link does.
			go dest.Receive(from, body)
			return true
		}
	}
	ra := m.nodes[a].Add(b, send(a, b))
	rb := m.nodes[b].Add(a, send(b, a))
	return func() { ra(); rb() }
}

func newMesh(names ...string) *mesh {
	m := &mesh{nodes: map[string]*Flood{}}
	for _, n := range names {
		m.nodes[n] = NewFlood(Open("", quiet))
	}
	return m
}

func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting: " + msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func has(f *Flood, e wire.Entry) bool {
	got, ok := f.table.Get(e.Key)
	return ok && slices.Equal(got.Version, e.Version)
}

func TestFloodReachesEveryNodeAndStops(t *testing.T) {
	// a - b - c - a: a ring, and d off c.
	m := newMesh("a", "b", "c", "d")
	m.connect("a", "b")
	m.connect("b", "c")
	m.connect("c", "a")
	m.connect("c", "d")
	e := node('a', 0, 1, "laptop")
	m.nodes["a"].Put(e)
	for _, n := range []string{"b", "c", "d"} {
		waitFor(t, "the entry at "+n, func() bool { return has(m.nodes[n], e) })
	}
	time.Sleep(50 * time.Millisecond)
	m.mu.Lock()
	frames := m.frames
	m.mu.Unlock()
	// a sends 2; b and c pass it on to their other neighbours: at most 1
	// and 2 more, and one of them crosses the ring for nothing.
	if frames > 6 {
		t.Fatalf("%d frames for one entry", frames)
	}
}

func TestNewLinkSendsTheWholeTable(t *testing.T) {
	m := newMesh("a", "b")
	old := node('a', 0, 1, "laptop")
	gone := removed('e', 0, 'a', 1)
	m.nodes["a"].Put(old, gone)
	m.nodes["b"].Put(node('b', 0, 1, "devbox"))
	m.connect("a", "b")
	waitFor(t, "both tables", func() bool {
		return has(m.nodes["b"], old) && has(m.nodes["b"], gone) && has(m.nodes["a"], node('b', 0, 1, "devbox"))
	})
}

func TestSecondLinkToAPeer(t *testing.T) {
	m := newMesh("a", "b")
	cut1 := m.connect("a", "b")
	m.connect("a", "b")
	cut1()
	e := node('a', 0, 7, "laptop")
	m.nodes["a"].Put(e)
	waitFor(t, "over the other link", func() bool { return has(m.nodes["b"], e) })
}
