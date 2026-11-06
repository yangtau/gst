package mesh

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Limits on what a node accepts from its peers.
const (
	maxRecords = 1024
	maxEdges   = 1024
)

// Update asks Run to publish this node's record if it changed.
func (n *Node) Update() {
	select {
	case n.update <- struct{}{}:
	default:
	}
}

// Run publishes this node's record whenever it changes and before it
// expires, and drops expired records, until ctx is done.
func (n *Node) Run(ctx context.Context) {
	defer n.stop()
	refresh := time.NewTicker(n.ttl / 3)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.update:
			n.publish(false)
		case <-refresh.C:
			n.publish(true)
			n.prune()
		}
	}
}

// publish replaces this node's record when it changed, or always with
// force, raising its Seq. Seq follows the clock, so that it keeps growing
// across restarts of the daemon.
func (n *Node) publish(force bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.info.ID == "" {
		return
	}
	edges := n.edges()
	old, ok := n.records[n.info.ID]
	if ok && !force && old.Node == n.info && slices.Equal(old.Links, edges) && sameNet(old.Net, n.net) {
		return
	}
	now := time.Now()
	seq := max(old.Seq+1, n.seqFloor+1, uint64(now.UnixNano()))
	n.records[n.info.ID] = wire.Record{Node: n.info, Seq: seq, Expires: now.Add(n.ttl), Links: edges, Net: n.net}
	n.notify()
}

func sameNet(a, b *wire.NetInfo) bool {
	return a == b || a != nil && b != nil && *a == *b
}

// SetNet sets what this node's record says of its part in the virtual
// network; nil takes it out.
func (n *Node) SetNet(info *wire.NetInfo) {
	n.mu.Lock()
	n.net = info
	n.mu.Unlock()
	n.Update()
}

// Changed returns a channel that is closed at the next change of what
// this node knows of the mesh.
func (n *Node) Changed() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.changed
}

// edges describes the running links; n.mu is held.
func (n *Node) edges() []wire.Edge {
	edges := []wire.Edge{}
	for l := range n.links {
		if !n.usable(l) {
			continue
		}
		e := wire.Edge{Name: l.name, Peer: l.peer.ID}
		if l.direct && l.opened {
			e.Direct, e.Local, e.Remote = true, l.local, l.remote
		}
		edges = append(edges, e)
	}
	slices.SortFunc(edges, func(a, b wire.Edge) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Peer, b.Peer), boolOrder(a.Direct, b.Direct))
	})
	return edges
}

// notify wakes the streams that push records; n.mu is held.
func (n *Node) notify() {
	close(n.changed)
	n.changed = make(chan struct{})
}

func (n *Node) prune() {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	for id, r := range n.records {
		if id != n.info.ID && !r.Expires.After(now) {
			delete(n.records, id)
		}
	}
}

// known returns the Seq of every record, by node id.
func (n *Node) known() map[string]uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	known := make(map[string]uint64, len(n.records))
	for id, r := range n.records {
		known[id] = r.Seq
	}
	return known
}

// newer returns the live records newer than sent, which it updates, and
// the channel that is closed at the next change.
func (n *Node) newer(sent map[string]uint64) ([]wire.Record, <-chan struct{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	recs := []wire.Record{}
	for id, r := range n.records {
		if r.Seq > sent[id] && r.Expires.After(now) {
			recs = append(recs, r)
			sent[id] = r.Seq
		}
	}
	return recs, n.changed
}

// merge keeps the records that are newer than the ones this node has.
// valid reports whether a record that a peer sent is well formed.
func valid(r wire.Record) bool {
	if !config.ValidNodeID(r.Node.ID) || len(r.Links) > maxEdges || !fits(r) || !validNet(r.Net) {
		return false
	}
	for _, e := range r.Links {
		if !config.ValidNodeID(e.Peer) || e.Name != "" && config.ValidateName(e.Name) != nil {
			return false
		}
	}
	return true
}

// validNet reports whether a record's part in the virtual network is well
// formed.
func validNet(info *wire.NetInfo) bool {
	if info == nil {
		return true
	}
	subnet, err := config.ParseSubnet(info.Subnet)
	if err != nil {
		return false
	}
	address, err := netip.ParseAddr(info.Address)
	key, keyErr := hex.DecodeString(info.PublicKey)
	return err == nil && subnet.Contains(address) && keyErr == nil && len(key) == 32
}

func (n *Node) merge(recs []wire.Record) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.info.ID == "" {
		return
	}
	now := time.Now()
	changed := false
	for _, r := range recs {
		if !valid(r) || !r.Expires.After(now) {
			continue
		}
		if r.Node.ID == n.info.ID {
			// An older run of this daemon published it; outdo it. Peers
			// also pass this node's current record back, which is no news.
			if r.Seq > n.records[n.info.ID].Seq {
				n.seqFloor = max(n.seqFloor, r.Seq)
				delete(n.records, n.info.ID) // so that publish replaces it
				n.Update()
			}
			continue
		}
		old, ok := n.records[r.Node.ID]
		if ok && old.Seq >= r.Seq || !ok && len(n.records) >= maxRecords {
			continue
		}
		n.records[r.Node.ID] = r
		changed = true
	}
	if changed {
		n.notify()
	}
}

// sync asks the peer for its records and keeps merging the ones it
// pushes until the session ends. When the exchange fails, sync closes the
// session, so that the link starts over rather than staying up without
// updates.
func (n *Node) sync(ctx context.Context, s session) {
	var recs wire.Records
	conn, err := call(ctx, s, wire.MethodTopologySync, wire.SyncParams{Known: n.known()}, &recs)
	if err == nil {
		defer conn.Close()
		defer context.AfterFunc(ctx, func() { conn.Close() })()
	}
	for err == nil {
		n.merge(recs.Records)
		recs = wire.Records{}
		err = wire.ReadFrame(conn, &recs)
	}
	if ctx.Err() == nil {
		s.Close()
	}
}

// serveSync answers topology.sync: it sends the records newer than the
// caller's and then every change, until the session ends.
func (n *Node) serveSync(ctx context.Context, conn net.Conn, p wire.SyncParams, accept func(any) error) error {
	sent := map[string]uint64{}
	for id, seq := range p.Known {
		sent[id] = seq
	}
	// The answer carries the first batch; further batches follow as pushes.
	recs, changed := n.newer(sent)
	batches := batch(recs)
	conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err := accept(wire.Records{Records: batches[0]}); err != nil {
		return err
	}
	for pending := batches[1:]; ; {
		for _, b := range pending {
			conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if err := wire.WriteFrame(conn, wire.Records{Records: b}); err != nil {
				return err
			}
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil
		}
		recs, changed = n.newer(sent)
		pending = nil
		if len(recs) > 0 {
			pending = batch(recs)
		}
	}
}

// maxBatch bounds the encoded records of one frame. Nodes keep only
// records that fit, so that they can pass every record on.
const maxBatch = wire.MaxFrame / 2

func fits(r wire.Record) bool {
	raw, err := json.Marshal(r)
	return err == nil && len(raw) <= maxBatch
}

// batch splits records into lists that each fit in a frame; it returns at
// least one list.
func batch(recs []wire.Record) [][]wire.Record {
	batches := [][]wire.Record{{}}
	size := 0
	for _, r := range recs {
		raw, _ := json.Marshal(r)
		last := len(batches) - 1
		if size+len(raw) > maxBatch && len(batches[last]) > 0 {
			batches = append(batches, []wire.Record{})
			last, size = last+1, 0
		}
		batches[last] = append(batches[last], r)
		size += len(raw)
	}
	return batches
}

// Topology reports the mesh as this node knows it, with this node's
// links as they are now rather than as last published.
func (n *Node) Topology() wire.Topology {
	n.mu.Lock()
	self := n.info
	recs := make([]wire.Record, 0, len(n.records))
	for id, r := range n.records {
		if id != self.ID {
			recs = append(recs, r)
		}
	}
	recs = append(recs, wire.Record{Node: self, Links: n.edges(), Net: n.net})
	direct := map[string]bool{}
	for l := range n.links {
		direct[l.peer.ID] = direct[l.peer.ID] || l.direct && n.usable(l)
	}
	n.mu.Unlock()
	if self.ID == "" {
		return wire.Topology{Nodes: []wire.TopoNode{}, Edges: []wire.TopoEdge{}}
	}
	topo := buildTopology(self.ID, recs, time.Now())
	for i := range topo.Nodes {
		topo.Nodes[i].Direct = direct[topo.Nodes[i].ID]
	}
	return topo
}

// buildTopology joins the live records into nodes and the links between
// them, and finds the shortest path from self to each node it reaches.
// Links carry connections both ways.
func buildTopology(self string, recs []wire.Record, now time.Time) wire.Topology {
	nodes := map[string]*wire.TopoNode{}
	for _, r := range recs {
		if r.Node.ID == self || r.Expires.After(now) {
			nodes[r.Node.ID] = &wire.TopoNode{ID: r.Node.ID, Name: r.Node.Name, Software: r.Node.Software, Exit: r.Node.Exit, Expires: r.Expires, Net: r.Net}
		}
	}
	// A link counts only while the nodes at both ends list it: the node
	// at an end that lost it may be unable to tell the others, until its
	// record expires, but the node at the other end can.
	live := slices.DeleteFunc(slices.Clone(recs), func(r wire.Record) bool { return r.Node.ID != self && !r.Expires.After(now) })
	peers := map[string]map[string]bool{}
	for _, r := range live {
		peers[r.Node.ID] = map[string]bool{}
		for _, e := range r.Links {
			peers[r.Node.ID][e.Peer] = true
		}
	}
	edges := []wire.TopoEdge{}
	for _, r := range live {
		for _, e := range r.Links {
			if e.Name == "" && !e.Direct || peers[e.Peer] != nil && !peers[e.Peer][r.Node.ID] {
				// Accepted links show as the opening node's; a link
				// that the other end no longer lists is gone.
				continue
			}
			if nodes[e.Peer] == nil {
				// Its record has not arrived yet.
				nodes[e.Peer] = &wire.TopoNode{ID: e.Peer, Name: e.Name}
			}
			edges = append(edges, wire.TopoEdge{From: r.Node.ID, To: e.Peer, Link: e.Name, Direct: e.Direct, Local: e.Local, Remote: e.Remote})
		}
	}
	slices.SortFunc(edges, func(a, b wire.TopoEdge) int {
		return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.Link, b.Link), cmp.Compare(a.To, b.To))
	})

	prev := reach(edges, self)
	for id := range prev {
		node := nodes[id]
		node.Reachable = true
		for at := id; at != ""; at = prev[at] {
			node.Path = append(node.Path, at)
		}
		slices.Reverse(node.Path)
	}

	t := wire.Topology{Self: self, Nodes: make([]wire.TopoNode, 0, len(nodes)), Edges: edges}
	for _, node := range nodes {
		t.Nodes = append(t.Nodes, *node)
	}
	slices.SortFunc(t.Nodes, func(a, b wire.TopoNode) int {
		return cmp.Or(boolOrder(a.ID == self, b.ID == self), cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return t
}

// reach finds the nodes that self reaches over links, in either
// direction. It returns, for each, the node before it on a shortest path;
// self has none.
func reach(edges []wire.TopoEdge, self string) map[string]string {
	prev := map[string]string{self: ""}
	queue := []string{self}
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		for _, e := range edges {
			next := ""
			switch at {
			case e.From:
				next = e.To
			case e.To:
				next = e.From
			}
			if _, seen := prev[next]; next != "" && !seen {
				prev[next] = at
				queue = append(queue, next)
			}
		}
	}
	return prev
}

// boolOrder sorts true before false.
func boolOrder(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1
	}
	return 1
}
