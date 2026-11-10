package mesh

import (
	"cmp"
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/mesh/state"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// nodeEntry is a nodeEntry of the network, as the table has it.
type nodeEntry struct {
	seq   int64 // the version of its entry, which follows the clock
	value wire.NodeValue
}

// nodeEntries returns the nodeEntries that entries hold.
func nodeEntries(entries []wire.Entry) map[string]nodeEntry {
	list := map[string]nodeEntry{}
	for _, e := range entries {
		kind, id, _ := state.ParseKey(e.Key)
		if kind != state.KindNode {
			continue
		}
		var v wire.NodeValue
		if json.Unmarshal(e.Value, &v) != nil {
			continue
		}
		list[id] = nodeEntry{seq: e.Version[1], value: v}
	}
	return list
}

// graph returns the links that carry messages: for this node, the links
// that run, and between others, those that the nodes at both ends list,
// as the node at an end that lost one may be unable to tell the others.
// n.mu is held.
func (n *Node) graph(nodes map[string]nodeEntry) map[string][]string {
	self := n.info.ID
	pairs := map[[2]string]bool{}
	add := func(a, b string) {
		if a > b {
			a, b = b, a
		}
		pairs[[2]string{a, b}] = true
	}
	for l := range n.links {
		add(self, l.Peer.ID)
	}
	for id, m := range nodes {
		if id == self {
			continue
		}
		for _, l := range m.value.Links {
			other, ok := nodes[l.Peer]
			if ok && l.Peer != self && slices.ContainsFunc(other.value.Links, func(o wire.NodeLink) bool { return o.Peer == id }) {
				add(id, l.Peer)
			}
		}
	}
	g := map[string][]string{}
	for p := range pairs {
		g[p[0]] = append(g[p[0]], p[1])
		g[p[1]] = append(g[p[1]], p[0])
	}
	for id := range g {
		slices.Sort(g[id])
	}
	return g
}

// shortest finds the shortest routes from self in g, in hops. Of routes of
// the same length, it takes the one whose first hop has the lowest id, so
// that all of a neighbour's traffic tends to take one way. It returns the
// route to each node it reaches, self's included.
func shortest(g map[string][]string, self string) map[string][]string {
	prev := map[string]string{self: ""}
	first := map[string]string{}
	level := []string{self}
	for len(level) > 0 {
		var next []string
		for _, at := range level {
			for _, to := range g[at] {
				hop := first[at]
				if at == self {
					hop = to
				}
				if _, seen := prev[to]; !seen {
					prev[to], first[to] = at, hop
					next = append(next, to)
				} else if slices.Contains(next, to) && hop < first[to] {
					prev[to], first[to] = at, hop
				}
			}
		}
		level = next
	}
	routes := map[string][]string{}
	for id := range prev {
		var route []string
		for at := id; at != ""; at = prev[at] {
			route = append(route, at)
		}
		slices.Reverse(route)
		routes[id] = route
	}
	return routes
}

// setRoutes works out the routes from entries, the table, or none while
// entries is nil. n.mu is held.
func (n *Node) setRoutes(entries []wire.Entry) {
	routes := map[string][]string{}
	if entries != nil {
		routes = shortest(n.graph(nodeEntries(entries)), n.info.ID)
	}
	now := time.Now()
	for id := range n.routes {
		if routes[id] == nil {
			n.lost[id] = now
		}
	}
	for id := range routes {
		delete(n.lost, id)
	}
	next := map[string]string{}
	for id, route := range routes {
		if len(route) > 1 {
			next[id] = route[1]
		}
	}
	n.routes = routes
	n.router.SetRoutes(next)
	if v, ok := n.Subnet(); ok {
		if subnet, err := netip.ParsePrefix(v.Subnet); err == nil {
			n.subnetPrefix.Store(&subnet)
		}
	}
}

// Topology reports the network as this node knows it: its nodes, online
// or not, and the links that run between them.
func (n *Node) Topology() wire.Topology {
	n.mu.Lock()
	defer n.mu.Unlock()
	self := n.info
	topo := wire.Topology{Self: self.ID, Nodes: []wire.TopoNode{}, Links: []wire.TopoLink{}}
	if self.ID == "" {
		return topo
	}
	if v, ok := n.Subnet(); ok {
		topo.Subnet = v.Subnet
	}
	nodes := map[string]*wire.TopoNode{self.ID: {ID: self.ID, Name: self.Name, Software: self.Software, Net: n.net}}
	if n.removed == nil {
		all := nodeEntries(n.table.Entries())
		for id, m := range all {
			if id != self.ID {
				// Its entry says when it last published, at least every
				// refresh; this node may have seen it later.
				seen := time.Unix(0, m.seq)
				if lost := n.lost[id]; lost.After(seen) {
					seen = lost
				}
				nodes[id] = &wire.TopoNode{ID: id, Name: m.value.Name, Software: m.value.Software, Seen: seen, Net: m.value.Net}
			}
		}
		for l := range n.links {
			if n.isRemoved(l.Peer.ID) {
				continue // Its link is about to close.
			}
			if nodes[l.Peer.ID] == nil {
				// Its entry has not arrived yet.
				nodes[l.Peer.ID] = &wire.TopoNode{ID: l.Peer.ID, Name: l.Peer.Name, Software: l.Peer.Software}
			}
			if l.Opened {
				topo.Links = append(topo.Links, wire.TopoLink{From: self.ID, To: l.Peer.ID})
			} else {
				topo.Links = append(topo.Links, wire.TopoLink{From: l.Peer.ID, To: self.ID})
			}
		}
		for id, m := range all {
			for _, l := range m.value.Links {
				other, ok := all[l.Peer]
				if id == self.ID || l.Peer == self.ID || !ok || !l.Opened {
					continue
				}
				if slices.ContainsFunc(other.value.Links, func(o wire.NodeLink) bool { return o.Peer == id && !o.Opened }) {
					topo.Links = append(topo.Links, wire.TopoLink{From: id, To: l.Peer})
				}
			}
		}
	}
	for id, route := range n.routes {
		if node := nodes[id]; node != nil {
			node.Reachable, node.Route = true, route
		}
	}
	nodes[self.ID].Reachable, nodes[self.ID].Route = true, []string{self.ID}
	for _, node := range nodes {
		if node.ID != self.ID {
			node.Direct = n.packetPeer(node.ID).direct.Load() != nil
		}
		topo.Nodes = append(topo.Nodes, *node)
	}
	slices.SortFunc(topo.Nodes, func(a, b wire.TopoNode) int {
		return cmp.Or(boolOrder(a.ID == self.ID, b.ID == self.ID), cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	slices.SortFunc(topo.Links, func(a, b wire.TopoLink) int {
		return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.To, b.To))
	})
	topo.Links = slices.Compact(topo.Links)
	return topo
}

// FindNode finds a node of topo by ref: its id, its name, or a prefix of
// at least four characters of its id, after dropping a ".gst" suffix.
// Names and prefixes must be unique.
func FindNode(topo wire.Topology, ref string) (wire.TopoNode, error) {
	ref = strings.TrimSuffix(strings.ToLower(ref), ".gst")
	for _, node := range topo.Nodes {
		if node.ID == ref {
			return node, nil
		}
	}
	var byName, byPrefix []wire.TopoNode
	for _, node := range topo.Nodes {
		switch {
		case node.Name == ref:
			byName = append(byName, node)
		case len(ref) >= 4 && strings.HasPrefix(node.ID, ref):
			byPrefix = append(byPrefix, node)
		}
	}
	for _, found := range [][]wire.TopoNode{byName, byPrefix} {
		switch len(found) {
		case 0:
			continue
		case 1:
			return found[0], nil
		}
		ids := make([]string, len(found))
		for i, node := range found {
			ids[i] = node.ID[:min(8, len(node.ID))]
		}
		return wire.TopoNode{}, wire.Errorf(wire.CodeInvalidParams, "%q names several machines (%s); use an id from `gst status`", ref, strings.Join(ids, ", "))
	}
	return wire.TopoNode{}, wire.Errorf(wire.CodeNotFound, "no machine %q in the network; see `gst status`", ref)
}

// Via returns the nodes between this node and dest along the links.
func Via(topo wire.Topology, dest wire.TopoNode) []wire.NodeInfo {
	via := []wire.NodeInfo{}
	if len(dest.Route) < 3 {
		return via
	}
	for _, id := range dest.Route[1 : len(dest.Route)-1] {
		if i := slices.IndexFunc(topo.Nodes, func(t wire.TopoNode) bool { return t.ID == id }); i >= 0 {
			t := topo.Nodes[i]
			via = append(via, t.Info())
		}
	}
	return via
}

// Call calls method on the node that ref names, along the links; ctx
// bounds the call, route.DefaultTimeout when it has no deadline.
func (n *Node) Call(ctx context.Context, ref, method string, params, result any) error {
	dest, err := FindNode(n.Topology(), ref)
	if err != nil {
		return err
	}
	if !dest.Reachable {
		return unreachable(dest)
	}
	return n.router.Call(ctx, dest.ID, method, params, result)
}

// unreachable is the error for a call to dest, which no route leads to.
// A UDP path carries only packets of the virtual network, so a node that
// has one is online still.
func unreachable(dest wire.TopoNode) error {
	if dest.Direct {
		return wire.Errorf(wire.CodeUnreachable, "no link leads to %s, which is online only directly", dest.Name)
	}
	return wire.Errorf(wire.CodeUnreachable, "%s is offline", dest.Name)
}

// Ping calls ping on the node that ref names, along the links, and
// reports the nodes on the way and the time the answer took. It checks
// the links, not the virtual network, and also tries a UDP route.
func (n *Node) Ping(ctx context.Context, ref string) (wire.Ping, error) {
	topo := n.Topology()
	dest, err := FindNode(topo, ref)
	if err != nil {
		return wire.Ping{}, err
	}
	if !dest.Reachable {
		return wire.Ping{}, unreachable(dest)
	}
	n.tryDirect(dest.ID)
	start := time.Now()
	var p wire.Ping
	if err := n.router.Call(ctx, dest.ID, wire.MethodPeerPing, nil, &p.Node); err != nil {
		return wire.Ping{}, err
	}
	p.TimeMS = float64(time.Since(start).Microseconds()) / 1000
	p.Via = Via(topo, dest)
	return p, nil
}

// Remove removes the node that ref names from the network, for good:
// only a person's gst add brings it back. Removing this node leaves the
// network.
func (n *Node) Remove(ref string) (wire.Removal, error) {
	topo := n.Topology()
	dest, err := FindNode(topo, ref)
	if err != nil {
		return wire.Removal{}, err
	}
	n.mu.Lock()
	self := n.info
	if n.removed != nil {
		n.mu.Unlock()
		return wire.Removal{}, wire.Errorf(wire.CodeRemoved, "this machine is not in a network")
	}
	gen := n.generation
	if dest.ID != self.ID {
		gen = 0
		if e, ok := n.table.Get(state.NodeKey(dest.ID)); ok {
			gen = e.Version[0]
		}
		for l := range n.links {
			if l.Peer.ID == dest.ID {
				gen = max(gen, l.Membership.Generation)
			}
		}
	}
	// The nodes that reach this one only through the removed one.
	g := n.graph(nodeEntries(n.table.Entries()))
	delete(g, dest.ID)
	after := shortest(g, self.ID)
	stranded := []wire.NodeInfo{}
	for id := range n.routes {
		if _, ok := after[id]; !ok && id != dest.ID && dest.ID != self.ID {
			if i := slices.IndexFunc(topo.Nodes, func(t wire.TopoNode) bool { return t.ID == id }); i >= 0 {
				t := topo.Nodes[i]
				stranded = append(stranded, t.Info())
			}
		}
	}
	n.mu.Unlock()
	slices.SortFunc(stranded, func(a, b wire.NodeInfo) int { return cmp.Compare(a.Name, b.Name) })
	value, _ := json.Marshal(wire.RemovedValue{By: self.ID, ByName: self.Name, At: time.Now().UTC()})
	n.flood.Put(wire.Entry{Key: state.RemovedKey(dest.ID), Version: []int64{gen}, Value: value})
	n.Update()
	return wire.Removal{Node: dest.Info(), Self: dest.ID == self.ID, Stranded: stranded}, nil
}
