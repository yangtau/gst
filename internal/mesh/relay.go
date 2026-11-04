package mesh

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/crypto/ssh"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/tunnel"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

const (
	maxPath      = 16               // nodes on a relay path
	relayTimeout = 30 * time.Second // to set up a relayed connection
)

// Dial connects to address on the machine that node names, through the
// mesh. node is a node name, id, or id prefix as gst mesh nodes shows
// them. The returned error is for interactive output: it may quote
// addresses.
func (n *Node) Dial(ctx context.Context, node, network, address string) (net.Conn, error) {
	if !n.Enabled() {
		return nil, errDisabled
	}
	topo := n.Topology()
	dest, err := FindNode(topo, node)
	if err != nil {
		return nil, err
	}
	path, err := Route(topo, dest.ID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, relayTimeout)
	defer cancel()
	return n.open(ctx, path, wire.Target{Network: network, Address: address})
}

// open connects to target along path, which starts with this node.
func (n *Node) open(ctx context.Context, path []string, target wire.Target) (net.Conn, error) {
	self := n.self()
	switch {
	case !config.ValidTarget(target.Network, target.Address):
		return nil, wire.Errorf(wire.CodeInvalidParams, "invalid target %s %q", target.Network, target.Address)
	case self.ID == "":
		return nil, wire.Errorf(wire.CodeFailed, "gst mesh is not enabled on the relaying machine")
	case len(path) == 0 || path[0] != self.ID:
		return nil, wire.Errorf(wire.CodeInvalidParams, "the relay path does not start at %s", self.Name)
	case len(path) > maxPath:
		return nil, wire.Errorf(wire.CodeInvalidParams, "the relay path is longer than %d nodes", maxPath)
	}
	if len(path) == 1 {
		var d net.Dialer
		conn, err := d.DialContext(ctx, target.Network, target.Address)
		if err != nil {
			return nil, wire.Errorf(wire.CodeFailed, "%s cannot connect to %s: %v", self.Name, target.Address, unwrapDial(err))
		}
		return conn, nil
	}
	next := path[1]
	if session := n.sessionTo(next); session != nil {
		return relayOver(ctx, session, path[1:], target)
	}
	if len(path) == 2 {
		if client := n.tunnelTo(next); client != nil {
			conn, err := client.DialContext(ctx, target.Network, target.Address)
			if err != nil {
				return nil, wire.Errorf(wire.CodeFailed, "the server of %s's tunnel cannot connect to %s: %v", self.Name, target.Address, err)
			}
			return conn, nil
		}
	}
	return nil, wire.Errorf(wire.CodeNotFound, "%s has no link to the next machine on the way; see gst mesh nodes", self.Name)
}

// unwrapDial drops the "dial tcp …:" prefix, which repeats the address.
func unwrapDial(err error) error {
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err
	}
	return err
}

// relayOver asks the peer at the other end of session to continue along
// path, which starts with that peer.
func relayOver(ctx context.Context, session *yamux.Session, path []string, target wire.Target) (net.Conn, error) {
	conn, err := call(ctx, session, wire.MethodRelayOpen, wire.RelayParams{Path: path, Target: target}, nil)
	if err != nil {
		var callErr *wire.Error
		if errors.As(err, &callErr) {
			return nil, callErr
		}
		if ctx.Err() != nil {
			return nil, wire.Errorf(wire.CodeFailed, "the relay timed out")
		}
		return nil, wire.Errorf(wire.CodeFailed, "the relay failed: %v", err)
	}
	return conn, nil
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
// When the peer link is lost, reading fails rather than ending cleanly, so
// that the relay at each end closes the connection instead of waiting for
// the other direction, which would never end.
type streamConn struct {
	*yamux.Stream
	session *yamux.Session
}

var errLinkLost = errors.New("the peer link closed")

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

// sessionTo returns the session of an up peer link with node id.
func (n *Node) sessionTo(id string) *yamux.Session {
	n.mu.Lock()
	defer n.mu.Unlock()
	for l := range n.links {
		if session := l.sessionTo(id); session != nil {
			return session
		}
	}
	return nil
}

// tunnelTo returns the connection of a running tunnel in the mesh whose
// server is the machine id.
func (n *Node) tunnelTo(id string) *ssh.Client {
	topo := n.Topology()
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, e := range topo.Edges {
		if e.From != topo.Self || e.To != id {
			continue
		}
		for t := range n.tunnels {
			if t.name == e.Tunnel {
				if client := t.connection(); client != nil {
					return client
				}
			}
		}
	}
	return nil
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
	defer target.Close()
	if err := accept(nil); err != nil {
		return err
	}
	tunnel.Relay(conn, target)
	return nil
}

// FindNode finds a machine of topo by its id, its name, or a prefix of at
// least four characters of its id.
func FindNode(topo wire.Topology, ref string) (wire.TopoNode, error) {
	var byName, byPrefix []wire.TopoNode
	for _, node := range topo.Nodes {
		switch {
		case node.ID == ref:
			return node, nil
		case node.Name == ref:
			byName = append(byName, node)
		case len(ref) >= 4 && node.Gst && strings.HasPrefix(node.ID, ref):
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
			ids[i] = node.ID
		}
		return wire.TopoNode{}, wire.Errorf(wire.CodeInvalidParams, "%q names several machines (%s); use an id from gst mesh nodes", ref, strings.Join(ids, ", "))
	}
	return wire.TopoNode{}, wire.Errorf(wire.CodeNotFound, "no machine %q in the mesh; see gst mesh nodes", ref)
}

// Route finds the shortest path from topo.Self to the machine id. A path
// goes on past a machine only over a peer link that is up, in either
// direction; its last step may also be a running tunnel without one.
func Route(topo wire.Topology, id string) ([]string, error) {
	prev := reach(topo.Edges, topo.Self)
	if _, ok := prev[id]; !ok {
		name := id
		for _, node := range topo.Nodes {
			if node.ID == id {
				name = node.Name
			}
		}
		return nil, wire.Errorf(wire.CodeNotFound, "no route to %s: it is not reachable through running tunnels and peer links; see gst mesh nodes", name)
	}
	var path []string
	for at := id; at != ""; at = prev[at] {
		path = append(path, at)
	}
	slices.Reverse(path)
	return path, nil
}
