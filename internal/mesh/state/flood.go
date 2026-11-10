package state

import (
	"encoding/json"
	"errors"
	"slices"
	"sync"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Flood passes the table between a node and its neighbours, over their
// links. Its rules are two: a new link starts with both sides sending their
// whole table, and an entry that beats what a node has goes on to every
// neighbour but the one it came from; one that does not stops there. A
// neighbour is a node, which several links may lead to: entries go
// over the newest, as an older one may be gone without its end knowing.
type Flood struct {
	table *Table

	mu    sync.Mutex
	peers map[string][]*sender // by node id, in the order the links came up
}

type sender struct {
	send func(body []byte) bool
}

// NewFlood floods t.
func NewFlood(t *Table) *Flood {
	return &Flood{table: t, peers: map[string][]*sender{}}
}

// Add adds a link to the neighbour peer, over which send sends the body of
// a sync frame, and sends it the whole table. It returns the function that
// removes the link.
func (f *Flood) Add(peer string, send func(body []byte) bool) (remove func()) {
	s := &sender{send}
	f.mu.Lock()
	f.peers[peer] = append(f.peers[peer], s)
	f.mu.Unlock()
	// After adding, so that whatever changes from now on goes this way too.
	sendAll(s, f.table.Entries())
	return func() {
		f.mu.Lock()
		list := slices.DeleteFunc(f.peers[peer], func(o *sender) bool { return o == s })
		var last *sender
		if len(list) == 0 {
			delete(f.peers, peer)
		} else {
			f.peers[peer] = list
			last = list[len(list)-1]
		}
		f.mu.Unlock()
		if last != nil {
			// What went over the link that ended may be lost.
			sendAll(last, f.table.Entries())
		}
	}
}

// Receive merges the body of a sync frame from the neighbour peer, and
// passes on what was new.
func (f *Flood) Receive(peer string, body []byte) error {
	var batch wire.Entries
	if err := json.Unmarshal(body, &batch); err != nil {
		return errors.New("invalid sync frame")
	}
	f.pass(f.table.Merge(batch.Entries), peer)
	return nil
}

// Put writes entries of this node, and reports those that beat what the
// table has, which go to every neighbour.
func (f *Flood) Put(entries ...wire.Entry) []wire.Entry {
	accepted := f.table.Merge(entries)
	f.pass(accepted, "")
	return accepted
}

func (f *Flood) pass(entries []wire.Entry, from string) {
	if len(entries) == 0 {
		return
	}
	bodies := batches(entries)
	f.mu.Lock()
	defer f.mu.Unlock()
	for peer, list := range f.peers {
		if peer != from {
			for _, body := range bodies {
				list[len(list)-1].send(body)
			}
		}
	}
}

// sendAll sends entries to s.
func sendAll(s *sender, entries []wire.Entry) {
	for _, body := range batches(entries) {
		s.send(body)
	}
}

// batches puts entries in frames of at most maxBatch bytes.
func batches(entries []wire.Entry) [][]byte {
	var bodies [][]byte
	var batch []wire.Entry
	size := 0
	flush := func() {
		if body, err := json.Marshal(wire.Entries{Entries: batch}); err == nil {
			bodies = append(bodies, body)
		}
		batch, size = nil, 0
	}
	for _, e := range entries {
		n := len(e.Key) + len(e.Value) + 64
		if size+n > maxBatch && len(batch) > 0 {
			flush()
		}
		batch = append(batch, e)
		size += n
	}
	if len(batch) > 0 {
		flush()
	}
	return bodies
}
