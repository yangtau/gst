// Package state keeps the table that every node holds of the network: an
// entry per member (node/<id>), one per removal (removed/<id>), and the
// network's subnet (network). Nodes flood the table to each other over
// their links: each link starts with both sides sending their whole table,
// and an entry that is new to a node goes on to its other neighbours. Of
// two entries with one key, the one with the higher version counts, by a
// rule per kind of entry, so that every node ends up with the same table
// whatever order the entries come in. The table lives on disk and nothing
// in it expires: a machine that is offline stays a member until someone
// removes it.
package state

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Limits on the table, against a node that goes wrong.
const (
	MaxNodes   = 1024     // node entries
	MaxRemoved = 1024     // removed entries
	MaxEntry   = 64 << 10 // bytes of an entry's value
	maxLinks   = 1024     // links of a node entry
	maxBatch   = wire.MaxFrame / 2
)

// Kinds of entries, by the start of their keys.
const (
	KindNode    = "node"
	KindRemoved = "removed"
	KindNetwork = "network"
)

// NetworkKey is the key of the network's subnet.
const NetworkKey = KindNetwork

// NodeKey is the key of node id's entry.
func NodeKey(id string) string { return KindNode + "/" + id }

// RemovedKey is the key of the removal of node id.
func RemovedKey(id string) string { return KindRemoved + "/" + id }

// ParseKey returns the kind of an entry and the node id it is about.
func ParseKey(key string) (kind, id string, ok bool) {
	if key == NetworkKey {
		return KindNetwork, "", true
	}
	kind, id, found := strings.Cut(key, "/")
	if !found || kind != KindNode && kind != KindRemoved || !config.ValidNodeID(id) {
		return "", "", false
	}
	return kind, id, true
}

// Valid checks an entry: its key, version, and value.
func Valid(e wire.Entry) error {
	kind, _, ok := ParseKey(e.Key)
	if !ok {
		return errors.New("invalid key")
	}
	if len(e.Value) > MaxEntry {
		return fmt.Errorf("the entry is larger than %d bytes", MaxEntry)
	}
	switch kind {
	case KindNode:
		var v wire.NodeValue
		if len(e.Version) != 2 || e.Version[0] < 0 || e.Version[1] <= 0 || json.Unmarshal(e.Value, &v) != nil {
			return errors.New("invalid node entry")
		}
		if config.ValidateName(v.Name) != nil || len(v.Links) > maxLinks {
			return errors.New("invalid node entry")
		}
		for _, l := range v.Links {
			if !config.ValidNodeID(l.Peer) {
				return errors.New("invalid link in a node entry")
			}
		}
		if v.Net != nil {
			a, err := netip.ParseAddr(v.Net.Address)
			key, keyErr := hex.DecodeString(v.Net.PublicKey)
			if err != nil || !a.Is4() || keyErr != nil || len(key) != 32 {
				return errors.New("invalid network part of a node entry")
			}
		}
	case KindRemoved:
		var v wire.RemovedValue
		if len(e.Version) != 1 || e.Version[0] < 0 || json.Unmarshal(e.Value, &v) != nil || !config.ValidNodeID(v.By) {
			return errors.New("invalid removal")
		}
	case KindNetwork:
		var v wire.NetworkValue
		if len(e.Version) != 2 || e.Version[0] < -1 || e.Version[0] > 1 || json.Unmarshal(e.Value, &v) != nil {
			return errors.New("invalid network entry")
		}
		if _, err := config.ParseSubnet(v.Subnet); err != nil {
			return errors.New("invalid network entry")
		}
	}
	return nil
}

// Newer reports whether entry a beats entry b, which has the same key: the
// higher version, and between equal versions, which only different nodes
// that write the same key at once produce, a choice that every node makes
// the same way: the earlier removal, or the lower subnet.
func Newer(a, b wire.Entry) bool {
	if c := slices.Compare(a.Version, b.Version); c != 0 {
		return c > 0
	}
	kind, _, _ := ParseKey(a.Key)
	switch kind {
	case KindRemoved:
		var va, vb wire.RemovedValue
		json.Unmarshal(a.Value, &va)
		json.Unmarshal(b.Value, &vb)
		if c := cmp.Or(va.At.Compare(vb.At), cmp.Compare(va.By, vb.By)); c != 0 {
			return c < 0
		}
	case KindNetwork:
		var va, vb wire.NetworkValue
		json.Unmarshal(a.Value, &va)
		json.Unmarshal(b.Value, &vb)
		if c := cmp.Compare(va.Subnet, vb.Subnet); c != 0 {
			return c < 0
		}
	}
	return bytes.Compare(a.Value, b.Value) < 0
}

// Table is a node's copy of the table.
type Table struct {
	log  *log.Logger
	path string // where it is saved; nowhere when empty

	mu      sync.Mutex
	owner   string // the node whose table it is
	entries map[string]wire.Entry
	counts  map[string]int // entries by kind
	changed chan struct{}  // closed and replaced at every change
	dirty   chan struct{}
}

// file is the table on disk.
type file struct {
	Node    string       `json:"node"`
	Entries []wire.Entry `json:"entries"`
}

// Open reads the table saved at path, if any; an empty path keeps it in
// memory only. A table that cannot be read starts empty: the other nodes
// have it.
func Open(path string, l *log.Logger) *Table {
	t := &Table{log: l, path: path, entries: map[string]wire.Entry{}, counts: map[string]int{}, changed: make(chan struct{}), dirty: make(chan struct{}, 1)}
	if path == "" {
		return t
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return t
	}
	var f file
	if json.Unmarshal(data, &f) != nil {
		t.log.Print("the saved table of the network is unreadable; starting empty")
		return t
	}
	t.owner = f.Node
	t.merge(f.Entries)
	return t
}

// Owner returns the node whose table it is.
func (t *Table) Owner() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.owner
}

// Reset empties the table for node owner, keeping only keep.
func (t *Table) Reset(owner string, keep ...wire.Entry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.owner = owner
	clear(t.entries)
	clear(t.counts)
	t.merge(keep)
	t.touch()
}

// Get returns the entry with key.
func (t *Table) Get(key string) (wire.Entry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[key]
	return e, ok
}

// Entries returns the whole table, sorted by key.
func (t *Table) Entries() []wire.Entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sorted()
}

func (t *Table) sorted() []wire.Entry {
	list := make([]wire.Entry, 0, len(t.entries))
	for _, e := range t.entries {
		list = append(list, e)
	}
	slices.SortFunc(list, func(a, b wire.Entry) int { return strings.Compare(a.Key, b.Key) })
	return list
}

// Changed returns a channel that is closed at the next change.
func (t *Table) Changed() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.changed
}

// Merge keeps the entries that beat what the table has, and returns them:
// what is news to this node, to pass on.
func (t *Table) Merge(entries []wire.Entry) []wire.Entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	accepted := t.merge(entries)
	if len(accepted) > 0 {
		t.touch()
	}
	return accepted
}

// touch tells the watchers and the saver of a change; t.mu is held.
func (t *Table) touch() {
	close(t.changed)
	t.changed = make(chan struct{})
	select {
	case t.dirty <- struct{}{}:
	default:
	}
}

func (t *Table) merge(entries []wire.Entry) []wire.Entry {
	var accepted []wire.Entry
	for _, e := range entries {
		if old, ok := t.entries[e.Key]; ok {
			if c := slices.Compare(e.Version, old.Version); c < 0 || c == 0 && string(e.Value) == string(old.Value) {
				continue // Known already, as most of a sync is.
			}
		}
		if err := Valid(e); err != nil {
			t.log.Printf("table: dropped an entry: %v", err)
			continue
		}
		kind, id, _ := ParseKey(e.Key)
		if kind == KindNode {
			if r, ok := t.entries[RemovedKey(id)]; ok && r.Version[0] >= e.Version[0] {
				continue // A removed node.
			}
		}
		old, ok := t.entries[e.Key]
		if ok && !Newer(e, old) {
			continue
		}
		if !ok && (kind == KindNode && t.counts[kind] >= MaxNodes || kind == KindRemoved && t.counts[kind] >= MaxRemoved) {
			t.log.Printf("table: dropped an entry: the table holds as many of kind %s as it can", kind)
			continue
		}
		e.Value = slices.Clone(e.Value)
		t.entries[e.Key] = e
		if !ok {
			t.counts[kind]++
		}
		accepted = append(accepted, e)
		if kind == KindRemoved {
			if n, ok := t.entries[NodeKey(id)]; ok && n.Version[0] <= e.Version[0] {
				delete(t.entries, NodeKey(id))
				t.counts[KindNode]--
			}
		}
	}
	return accepted
}

// Run saves the table after changes until ctx is done, and once more then.
func (t *Table) Run(ctx context.Context) {
	defer t.save()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.dirty:
		}
		// Changes come in bursts, such as a whole table from a new link.
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
		t.save()
	}
}

func (t *Table) save() {
	if t.path == "" {
		return
	}
	t.mu.Lock()
	data, err := json.MarshalIndent(file{Node: t.owner, Entries: t.sorted()}, "", "  ")
	t.mu.Unlock()
	if err == nil {
		err = config.WriteFile(t.path, append(data, '\n'))
	}
	if err != nil {
		t.log.Printf("table: save failed: %v", err)
	}
}
