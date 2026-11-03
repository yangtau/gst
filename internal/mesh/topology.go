package mesh

import (
	"cmp"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/crypto/ssh"

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
	var edges []wire.Edge
	if n.edges != nil {
		edges = n.edges()
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.info.ID == "" {
		return
	}
	edges = n.linkEdges(edges)
	old, ok := n.records[n.info.ID]
	if ok && !force && !n.republish && reflect.DeepEqual(old.Node, n.info) && reflect.DeepEqual(old.Edges, edges) {
		return
	}
	now := time.Now()
	seq := max(old.Seq+1, n.seqFloor+1, uint64(now.UnixNano()))
	n.records[n.info.ID] = wire.Record{Node: n.info, Seq: seq, Expires: now.Add(n.ttl), Edges: edges}
	n.republish = false
	n.notify()
}

// linkEdges fills in the peer links of this node's edges; n.mu is held.
func (n *Node) linkEdges(edges []wire.Edge) []wire.Edge {
	out := make([]wire.Edge, 0, len(edges))
	for _, e := range edges {
		e.Peer, e.Linked = "", false
		for l := range n.links {
			st := l.state()
			if st.Direction != wire.LinkOut || st.Tunnel != e.Tunnel {
				continue
			}
			if st.State == wire.LinkUp {
				e.Peer, e.Linked = st.Peer.ID, true
			} else {
				e.Peer = l.peerID()
			}
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b wire.Edge) int { return cmp.Compare(a.Tunnel, b.Tunnel) })
	return out
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
func (n *Node) merge(recs []wire.Record) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.info.ID == "" {
		return
	}
	now := time.Now()
	changed := false
	for _, r := range recs {
		if !config.ValidMeshID(r.Node.ID) || len(r.Edges) > maxEdges || !r.Expires.After(now) {
			continue
		}
		if r.Node.ID == n.info.ID {
			// An older run of this daemon published it; outdo it.
			if r.Seq >= n.records[n.info.ID].Seq {
				n.seqFloor = max(n.seqFloor, r.Seq)
				n.republish = true
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
// pushes until the session ends. A peer without topology.sync sends none.
func (n *Node) sync(ctx context.Context, session *yamux.Session) {
	stream, err := session.OpenStream()
	if err != nil {
		return
	}
	defer stream.Close()
	defer context.AfterFunc(ctx, func() { stream.Close() })()
	var recs wire.Records
	if err := wire.Call(stream, wire.MethodTopologySync, wire.SyncParams{Known: n.known()}, &recs); err != nil {
		return
	}
	for {
		n.merge(recs.Records)
		recs = wire.Records{}
		if err := wire.ReadFrame(stream, &recs); err != nil {
			return
		}
	}
}

// serveSync answers topology.sync: it sends the records newer than the
// caller's and then every change, until the session ends.
func (n *Node) serveSync(stream *yamux.Stream, session *yamux.Session, params []byte) {
	var p wire.SyncParams
	if err := wire.DecodeParams(params, &p); err != nil {
		wire.Reply(stream, nil, err)
		return
	}
	sent := map[string]uint64{}
	for id, seq := range p.Known {
		sent[id] = seq
	}
	recs, changed := n.newer(sent)
	stream.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if wire.Reply(stream, wire.Records{Records: recs}, nil) != nil {
		return
	}
	for {
		select {
		case <-changed:
		case <-session.CloseChan():
			return
		}
		recs, changed = n.newer(sent)
		if len(recs) == 0 {
			continue
		}
		stream.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if wire.WriteFrame(stream, wire.Records{Records: recs}) != nil {
			return
		}
	}
}

// Topology reports the mesh as this node knows it.
func (n *Node) Topology() wire.Topology {
	n.mu.Lock()
	self := n.info
	recs := make([]wire.Record, 0, len(n.records))
	for _, r := range n.records {
		recs = append(recs, r)
	}
	n.mu.Unlock()
	if self.ID == "" {
		return wire.Topology{Nodes: []wire.TopoNode{}, Edges: []wire.TopoEdge{}}
	}
	t := buildTopology(self, recs, time.Now())
	t.Enabled = true
	return t
}

// buildTopology joins the live records into machines and the tunnels
// between them. A tunnel leads to the gst node it is linked with, or else
// to the gst node whose host keys include the server's, or else to an SSH
// server without gst.
func buildTopology(self wire.NodeInfo, recs []wire.Record, now time.Time) wire.Topology {
	recs = slices.DeleteFunc(slices.Clone(recs), func(r wire.Record) bool {
		return r.Node.ID != self.ID && !r.Expires.After(now)
	})
	slices.SortFunc(recs, func(a, b wire.Record) int { return cmp.Compare(a.Node.ID, b.Node.ID) })

	nodes := map[string]*wire.TopoNode{}
	hostOwner := map[string]string{}
	for _, r := range recs {
		nodes[r.Node.ID] = &wire.TopoNode{ID: r.Node.ID, Name: r.Node.Name, Gst: true, Software: r.Node.Software, HostKeys: r.Node.HostKeys, Expires: r.Expires}
		for _, k := range r.Node.HostKeys {
			if _, taken := hostOwner[k]; !taken {
				hostOwner[k] = r.Node.ID
			}
		}
	}
	if nodes[self.ID] == nil {
		nodes[self.ID] = &wire.TopoNode{ID: self.ID, Name: self.Name, Gst: true, Software: self.Software, HostKeys: self.HostKeys}
	}

	edges := []wire.TopoEdge{}
	for _, r := range recs {
		for _, e := range r.Edges {
			var to string
			switch {
			case e.Peer != "" && nodes[e.Peer] != nil && nodes[e.Peer].Gst:
				to = e.Peer
			case e.HostKey != "" && hostOwner[e.HostKey] != "":
				to = hostOwner[e.HostKey]
			case e.HostKey != "":
				to = "ssh:" + e.HostKey
			default:
				to = "tunnel:" + r.Node.ID + "/" + e.Tunnel
			}
			if nodes[to] == nil {
				nodes[to] = &wire.TopoNode{ID: to, Name: e.Tunnel}
			}
			edges = append(edges, wire.TopoEdge{From: r.Node.ID, To: to, Tunnel: e.Tunnel, Destination: e.Destination, State: e.State, Linked: e.Linked})
		}
	}

	slices.SortFunc(edges, func(a, b wire.TopoEdge) int {
		return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.Tunnel, b.Tunnel))
	})

	// This node reaches a machine over a running tunnel of a node it
	// reaches, and goes on from there through a peer link, in either
	// direction, since relaying needs gst at both ends.
	nodes[self.ID].Reachable = true
	relays := map[string]bool{self.ID: true}
	queue := []string{self.ID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, e := range edges {
			var next string
			switch {
			case e.From == id && (e.Linked || e.State == wire.StateRunning):
				next = e.To
			case e.To == id && e.Linked:
				next = e.From
			default:
				continue
			}
			nodes[next].Reachable = true
			if e.Linked && !relays[next] {
				relays[next] = true
				queue = append(queue, next)
			}
		}
	}

	t := wire.Topology{Self: self.ID, Nodes: make([]wire.TopoNode, 0, len(nodes)), Edges: edges}
	for _, node := range nodes {
		t.Nodes = append(t.Nodes, *node)
	}
	slices.SortFunc(t.Nodes, func(a, b wire.TopoNode) int {
		if a.ID == self.ID || b.ID == self.ID {
			return boolOrder(a.ID == self.ID, b.ID == self.ID)
		}
		return cmp.Or(boolOrder(a.Gst, b.Gst), cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return t
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

// readHostKeys returns the fingerprints of the public keys matching glob.
func readHostKeys(glob string) []string {
	paths, _ := filepath.Glob(glob)
	var keys []string
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			continue
		}
		keys = append(keys, ssh.FingerprintSHA256(key))
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}
