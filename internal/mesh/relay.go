package mesh

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/hashicorp/yamux"

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
		return nil, wire.Errorf(wire.CodeNotFound, "no route to %s: no running links lead there; see gst nodes", dest.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, relayTimeout)
	defer cancel()
	return n.open(ctx, dest.Path, wire.Target{Network: network, Address: address})
}

// open connects to target along path, which starts with this node.
func (n *Node) open(ctx context.Context, path []string, target wire.Target) (net.Conn, error) {
	self := n.Self()
	switch {
	case !config.ValidTarget(target.Network, target.Address):
		return nil, wire.Errorf(wire.CodeInvalidParams, "invalid target %s %q", target.Network, target.Address)
	case self.ID == "":
		return nil, wire.Errorf(wire.CodeFailed, "gst on the relaying machine has no node identity")
	case len(path) == 0 || path[0] != self.ID:
		return nil, wire.Errorf(wire.CodeInvalidParams, "the relay path does not start at %s", self.Name)
	case len(path) > maxPath:
		return nil, wire.Errorf(wire.CodeInvalidParams, "the relay path is longer than %d nodes", maxPath)
	}
	if len(path) == 1 {
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
	session := n.sessionTo(path[1])
	if session == nil {
		return nil, wire.Errorf(wire.CodeNotFound, "%s has no running link to the next machine on the way; see gst nodes", self.Name)
	}
	conn, err := call(ctx, session, wire.MethodRelayOpen, wire.RelayParams{Path: path[1:], Target: target}, nil)
	if err != nil {
		var callErr *wire.Error
		switch {
		case errors.As(err, &callErr):
			return nil, callErr
		case ctx.Err() != nil:
			return nil, wire.Errorf(wire.CodeFailed, "the relay timed out")
		}
		return nil, wire.Errorf(wire.CodeFailed, "the relay failed: %v", err)
	}
	return conn, nil
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

// call opens a stream to the peer at the other end of session and calls
// method on it; ctx bounds the call. The stream stays open for what
// follows the answer, such as a relayed connection or pushed frames.
func call(ctx context.Context, session *yamux.Session, method string, params, result any) (streamConn, error) {
	stream, err := session.OpenStream()
	if err != nil {
		return streamConn{}, errLinkLost
	}
	conn := streamConn{stream, session}
	// Closing a yamux stream only ends writing; a deadline ends the wait
	// for the answer.
	stop := context.AfterFunc(ctx, func() { stream.SetDeadline(time.Now()) })
	err = wire.Call(conn, method, params, result)
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		conn.Close()
		return streamConn{}, err
	}
	return conn, nil
}

// streamConn is a connection over a yamux stream. CloseWrite ends
// writing, as closing a yamux stream does, while Close also ends reading.
// When the link is lost, reading fails rather than ending cleanly, so
// that the relay at each end closes the connection instead of waiting for
// the other direction, which would never end.
type streamConn struct {
	*yamux.Stream
	session *yamux.Session
}

var errLinkLost = errors.New("the link closed")

func (c streamConn) Read(p []byte) (int, error) {
	n, err := c.Stream.Read(p)
	if err == io.EOF && c.session.IsClosed() {
		err = errLinkLost
	}
	return n, err
}

func (c streamConn) CloseWrite() error { return c.Stream.Close() }

func (c streamConn) Close() error {
	err := c.Stream.Close()
	c.Stream.SetReadDeadline(time.Now())
	return err
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
			ids[i] = node.ID[:8]
		}
		return wire.TopoNode{}, wire.Errorf(wire.CodeInvalidParams, "%q names several machines (%s); use an id from gst nodes", ref, strings.Join(ids, ", "))
	}
	return wire.TopoNode{}, wire.Errorf(wire.CodeNotFound, "no machine %q in the mesh; see gst nodes", ref)
}
