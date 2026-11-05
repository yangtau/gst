package mesh

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/relay"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

const (
	maxPath      = 16               // nodes on a relay path
	relayTimeout = 30 * time.Second // to set up a relayed connection
)

// Dial connects to address on the machine that ref names, along the
// shortest path through the mesh. ref is as config.ParseMachine reads it;
// see FindNode. The returned error is for interactive output: it may quote
// addresses.
func (n *Node) Dial(ctx context.Context, ref, network, address string) (net.Conn, error) {
	topo := n.Topology()
	dest, err := FindNode(topo, ref)
	if err != nil {
		return nil, err
	}
	if !dest.Reachable {
		return nil, wire.Errorf(wire.CodeNotFound, "no route to %s: no running links lead there; see gst status", dest.Name)
	}
	n.tryDirect(dest.ID)
	ctx, cancel := context.WithTimeout(ctx, relayTimeout)
	defer cancel()
	return n.open(ctx, dest.Path, wire.Target{Network: network, Address: address})
}

// open connects to target along path, which starts with this node.
func (n *Node) open(ctx context.Context, path []string, target wire.Target) (net.Conn, error) {
	self := n.Self()
	switch {
	case target != (wire.Target{Network: wire.NetworkGst}) && !config.ValidTarget(target.Network, target.Address):
		return nil, wire.Errorf(wire.CodeInvalidParams, "invalid target %s %q", target.Network, target.Address)
	case self.ID == "":
		return nil, wire.Errorf(wire.CodeFailed, "gst on the relaying machine has no node identity")
	case len(path) == 0 || path[0] != self.ID:
		return nil, wire.Errorf(wire.CodeInvalidParams, "the relay path does not start at %s", self.Name)
	case len(path) > maxPath:
		return nil, wire.Errorf(wire.CodeInvalidParams, "the relay path is longer than %d nodes", maxPath)
	}
	if len(path) == 1 {
		if target.Network == wire.NetworkGst {
			return n.callee(), nil
		}
		if !self.Exit && !onThisMachine(target) {
			return nil, wire.Errorf(wire.CodeForbidden, "%s is not an exit, so it connects only to addresses on its own machine; run `gst exit on` there to let it connect to %s", self.Name, target.Address)
		}
		var d net.Dialer
		conn, err := d.DialContext(ctx, target.Network, target.Address)
		if err != nil {
			return nil, wire.Errorf(wire.CodeFailed, "%s cannot connect to %s: %v", self.Name, target.Address, relay.DialError(err))
		}
		return conn, nil
	}
	for {
		session := n.sessionTo(path[1])
		if session == nil {
			return nil, wire.Errorf(wire.CodeNotFound, "%s has no running link to the next machine on the way; see gst status", self.Name)
		}
		conn, err := call(ctx, session, wire.MethodRelayOpen, wire.RelayParams{Path: path[1:], Target: target}, nil)
		if err == nil {
			return conn, nil
		}
		var callErr *wire.Error
		switch {
		case errors.As(err, &callErr):
			return nil, callErr
		case ctx.Err() != nil:
			return nil, wire.Errorf(wire.CodeFailed, "the relay timed out")
		case closed(session) && n.sessionTo(path[1]) != nil:
			// A direct link broke while it was the way to the next
			// node, which another link also leads to.
			continue
		}
		return nil, wire.Errorf(wire.CodeFailed, "the relay failed: %v", err)
	}
}

// callee returns a connection that carries calls to this node's peer
// methods, as one to the gst node at the end of a path does.
func (n *Node) callee() net.Conn {
	caller, callee := net.Pipe()
	go func() {
		defer callee.Close()
		n.peer.Serve(context.Background(), callee)
	}()
	return caller
}

// Call calls method on the machine that ref names, along the mesh, as
// Dial finds it; ctx bounds the call.
func (n *Node) Call(ctx context.Context, ref, method string, params, result any) error {
	conn, err := n.Dial(ctx, ref, wire.NetworkGst, "")
	if err != nil {
		return err
	}
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { conn.Close() })()
	if err := wire.Call(conn, method, params, result); err != nil {
		if ctx.Err() != nil {
			return wire.Errorf(wire.CodeFailed, "%s did not answer in time", ref)
		}
		return err
	}
	return nil
}

// Ping calls node.info on the machine that ref names, along the mesh,
// and reports the machines on the way and the time the answer took.
func (n *Node) Ping(ctx context.Context, ref string) (wire.Ping, error) {
	topo := n.Topology()
	dest, err := FindNode(topo, ref)
	if err != nil {
		return wire.Ping{}, err
	}
	n.mu.Lock()
	direct := len(dest.Path) == 2 && n.directLink(dest.ID) != nil
	n.mu.Unlock()
	start := time.Now()
	p := wire.Ping{Direct: direct}
	if err := n.Call(ctx, dest.ID, wire.MethodNodeInfo, nil, &p.Node); err != nil {
		return wire.Ping{}, err
	}
	p.TimeMS = float64(time.Since(start).Microseconds()) / 1000
	p.Via = []wire.NodeInfo{}
	for _, id := range dest.Path[1:max(1, len(dest.Path)-1)] {
		if i := slices.IndexFunc(topo.Nodes, func(t wire.TopoNode) bool { return t.ID == id }); i >= 0 {
			t := topo.Nodes[i]
			p.Via = append(p.Via, wire.NodeInfo{ID: t.ID, Name: t.Name, Software: t.Software, Exit: t.Exit})
		}
	}
	return p, nil
}

// onThisMachine reports whether target is an address on the machine that
// connects to it: a Unix socket, localhost, or a loopback address.
func onThisMachine(target wire.Target) bool {
	if target.Network == "unix" {
		return true
	}
	host, _, err := net.SplitHostPort(target.Address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// serveRelay answers relay.open and then carries the connection's bytes
// until either side ends.
func (n *Node) serveRelay(ctx context.Context, conn net.Conn, p wire.RelayParams, accept func(any) error) error {
	dialCtx, cancel := context.WithTimeout(ctx, relayTimeout)
	target, err := n.open(dialCtx, p.Path, p.Target)
	cancel()
	if err != nil {
		return err
	}
	if err := accept(nil); err != nil {
		target.Close()
		return err
	}
	relay.Pipe(conn, target)
	return nil
}

// call opens a stream to the peer at the other end of s and calls method
// on it; ctx bounds the call. The stream stays open for what follows the
// answer, such as a relayed connection or pushed frames.
func call(ctx context.Context, s session, method string, params, result any) (net.Conn, error) {
	conn, err := s.Open(ctx)
	if err != nil {
		return nil, errLinkLost
	}
	// Closing a stream may only end writing; a deadline ends the wait for
	// the answer.
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	err = wire.Call(conn, method, params, result)
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// FindNode finds a node of topo by ref: its id, its name, the name of a
// link to it, or a prefix of at least four characters of its id, after
// dropping a ".gst" suffix. Names and prefixes must be unique.
func FindNode(topo wire.Topology, ref string) (wire.TopoNode, error) {
	ref = strings.TrimSuffix(strings.ToLower(ref), ".gst")
	byID := map[string]wire.TopoNode{}
	for _, node := range topo.Nodes {
		if node.ID == ref {
			return node, nil
		}
		byID[node.ID] = node
	}
	var byName, byLink, byPrefix []wire.TopoNode
	for _, node := range topo.Nodes {
		switch {
		case node.Name == ref:
			byName = append(byName, node)
		case len(ref) >= 4 && strings.HasPrefix(node.ID, ref):
			byPrefix = append(byPrefix, node)
		}
	}
	seen := map[string]bool{}
	for _, e := range topo.Edges {
		if e.Link == ref && !seen[e.To] {
			seen[e.To] = true
			byLink = append(byLink, byID[e.To])
		}
	}
	for _, found := range [][]wire.TopoNode{byName, byLink, byPrefix} {
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
		return wire.TopoNode{}, wire.Errorf(wire.CodeInvalidParams, "%q names several machines (%s); use an id from gst status", ref, strings.Join(ids, ", "))
	}
	return wire.TopoNode{}, wire.Errorf(wire.CodeNotFound, "no machine %q in the mesh; see gst status", ref)
}
