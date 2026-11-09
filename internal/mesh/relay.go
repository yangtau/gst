package mesh

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/relay"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

const (
	maxPath      = 16               // nodes on a relay path
	relayTimeout = 30 * time.Second // to set up a relayed connection
)

// Dial opens a stream that carries calls to the peer methods of the
// machine that ref names, along the shortest path through the mesh. ref is
// as config.ParseMachine reads it; see FindNode. The returned error is for
// interactive output.
func (n *Node) Dial(ctx context.Context, ref string) (net.Conn, error) {
	dest, err := FindNode(n.Topology(), ref)
	if err != nil {
		return nil, err
	}
	if !dest.Reachable {
		return nil, wire.Errorf(wire.CodeNotFound, "no way to %s: no running links lead there; see `gst status`", dest.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, relayTimeout)
	defer cancel()
	return n.open(ctx, dest.Path)
}

// open opens a stream along path, which starts with this node, to the
// node at its end.
func (n *Node) open(ctx context.Context, path []string) (net.Conn, error) {
	self := n.Self()
	switch {
	case self.ID == "":
		return nil, wire.Errorf(wire.CodeFailed, "gst on the relaying machine has no node identity")
	case len(path) == 0 || path[0] != self.ID:
		return nil, wire.Errorf(wire.CodeInvalidParams, "the relay path does not start at %s", self.Name)
	case len(path) > maxPath:
		return nil, wire.Errorf(wire.CodeInvalidParams, "the relay path is longer than %d nodes", maxPath)
	}
	if len(path) == 1 {
		return n.callee(), nil
	}
	s, ok := n.sessionTo(path[1])
	if !ok {
		return nil, wire.Errorf(wire.CodeNotFound, "%s has no running link to the next machine on the way; see `gst status`", self.Name)
	}
	conn, err := call(ctx, s, wire.MethodRelayOpen, wire.RelayParams{Path: path[1:]}, nil)
	if err == nil {
		return conn, nil
	}
	var callErr *wire.Error
	switch {
	case errors.As(err, &callErr):
		return nil, callErr
	case ctx.Err() != nil:
		return nil, wire.Errorf(wire.CodeFailed, "the relay timed out")
	}
	return nil, wire.Errorf(wire.CodeFailed, "the relay failed: %v", err)
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
	conn, err := n.Dial(ctx, ref)
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

// Ping calls node.info on the machine that ref names, along the links, and
// reports the machines on the way and the time the answer took. It checks
// the links, not the virtual network, and also tries a UDP path.
func (n *Node) Ping(ctx context.Context, ref string) (wire.Ping, error) {
	topo := n.Topology()
	dest, err := FindNode(topo, ref)
	if err != nil {
		return wire.Ping{}, err
	}
	n.tryDirect(dest.ID)
	start := time.Now()
	var p wire.Ping
	if err := n.Call(ctx, dest.ID, wire.MethodNodeInfo, nil, &p.Node); err != nil {
		return wire.Ping{}, err
	}
	p.TimeMS = float64(time.Since(start).Microseconds()) / 1000
	p.Via = Via(topo, dest)
	return p, nil
}

// Via returns the machines between this node and dest along the links.
func Via(topo wire.Topology, dest wire.TopoNode) []wire.NodeInfo {
	via := []wire.NodeInfo{}
	for _, id := range dest.Path[1:max(1, len(dest.Path)-1)] {
		if i := slices.IndexFunc(topo.Nodes, func(t wire.TopoNode) bool { return t.ID == id }); i >= 0 {
			t := topo.Nodes[i]
			via = append(via, wire.NodeInfo{ID: t.ID, Name: t.Name, Software: t.Software})
		}
	}
	return via
}

// serveRelay answers relay.open and then carries the stream's bytes until
// either side ends.
func (n *Node) serveRelay(ctx context.Context, conn net.Conn, p wire.RelayParams, accept func(any) error) error {
	dialCtx, cancel := context.WithTimeout(ctx, relayTimeout)
	target, err := n.open(dialCtx, p.Path)
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
// answer, such as relayed calls or pushed frames.
func call(ctx context.Context, s session, method string, params, result any) (net.Conn, error) {
	conn, err := s.open()
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
