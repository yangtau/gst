package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/crypto/ssh"

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
		return nil, wire.Errorf(wire.CodeFailed, "gst mesh is not enabled on this machine; run `gst mesh enable`")
	}
	target := wire.Target{Network: network, Address: address}
	if err := checkTarget(target); err != nil {
		return nil, err
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
	return n.open(ctx, path, target)
}

func checkTarget(t wire.Target) *wire.Error {
	switch t.Network {
	case "tcp":
		if _, _, err := net.SplitHostPort(t.Address); err != nil {
			return wire.Errorf(wire.CodeInvalidParams, "invalid target address %q", t.Address)
		}
	case "unix":
		if !strings.HasPrefix(t.Address, "/") {
			return wire.Errorf(wire.CodeInvalidParams, "invalid target socket %q", t.Address)
		}
	default:
		return wire.Errorf(wire.CodeInvalidParams, "unsupported target network %q", t.Network)
	}
	return nil
}

// open connects to target along path, which starts with this node.
func (n *Node) open(ctx context.Context, path []string, target wire.Target) (net.Conn, error) {
	self := n.self()
	switch {
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
	stream, err := session.OpenStream()
	if err != nil {
		return nil, wire.Errorf(wire.CodeFailed, "the peer link closed")
	}
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	err = wire.Call(stream, wire.MethodRelayOpen, wire.RelayParams{Path: path, Target: target}, nil)
	if !stop() || err != nil {
		stream.Close()
		var callErr *wire.Error
		if errors.As(err, &callErr) {
			return nil, callErr
		}
		if ctx.Err() != nil {
			return nil, wire.Errorf(wire.CodeFailed, "the relay timed out")
		}
		return nil, wire.Errorf(wire.CodeFailed, "the relay failed: %v", err)
	}
	return stream, nil
}

// sessionTo returns the session of an up peer link with node id.
func (n *Node) sessionTo(id string) *yamux.Session {
	n.mu.Lock()
	defer n.mu.Unlock()
	for l := range n.links {
		l.mu.Lock()
		up := l.info.State == wire.LinkUp && l.info.Peer.ID == id && l.session != nil && !l.session.IsClosed()
		session := l.session
		l.mu.Unlock()
		if up {
			return session
		}
	}
	return nil
}

// tunnelTo returns the connection of a running tunnel with mesh on whose
// server is the machine id.
func (n *Node) tunnelTo(id string) *ssh.Client {
	topo := n.Topology()
	for _, e := range topo.Edges {
		if e.From != topo.Self || e.To != id || e.State != wire.StateRunning {
			continue
		}
		n.mu.Lock()
		for l := range n.links {
			l.mu.Lock()
			client := l.client
			match := l.info.Direction == wire.LinkOut && l.info.Tunnel == e.Tunnel && client != nil
			l.mu.Unlock()
			if match {
				n.mu.Unlock()
				return client
			}
		}
		n.mu.Unlock()
	}
	return nil
}

// serveRelay answers relay.open on stream and then carries the
// connection's bytes until either side ends.
func (n *Node) serveRelay(ctx context.Context, stream *yamux.Stream, params json.RawMessage) {
	var p wire.RelayParams
	if err := wire.DecodeParams(params, &p); err != nil {
		wire.Reply(stream, nil, err)
		return
	}
	if err := checkTarget(p.Target); err != nil {
		wire.Reply(stream, nil, err)
		return
	}
	dialCtx, cancel := context.WithTimeout(ctx, relayTimeout)
	conn, err := n.open(dialCtx, p.Path, p.Target)
	cancel()
	if err != nil {
		wire.Reply(stream, nil, asWireError(err))
		return
	}
	defer conn.Close()
	if wire.Reply(stream, nil, nil) != nil {
		return
	}
	tunnel.Relay(stream, conn)
}

func asWireError(err error) *wire.Error {
	var callErr *wire.Error
	if errors.As(err, &callErr) {
		return callErr
	}
	return wire.Errorf(wire.CodeFailed, "%v", err)
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
	if id == topo.Self {
		return []string{id}, nil
	}
	// parent leads back to Self from every machine that relays.
	parent := map[string]string{topo.Self: ""}
	queue := []string{topo.Self}
	for len(queue) > 0 {
		from := queue[0]
		queue = queue[1:]
		for _, e := range topo.Edges {
			var next string
			switch {
			case e.From == from && (e.Linked || e.State == wire.StateRunning):
				next = e.To
			case e.To == from && e.Linked:
				next = e.From
			default:
				continue
			}
			if next == id {
				path := []string{id}
				for at := from; at != ""; at = parent[at] {
					path = append([]string{at}, path...)
				}
				return path, nil
			}
			if _, seen := parent[next]; e.Linked && !seen {
				parent[next] = from
				queue = append(queue, next)
			}
		}
	}
	name := id
	for _, node := range topo.Nodes {
		if node.ID == id {
			name = node.Name
		}
	}
	return nil, wire.Errorf(wire.CodeNotFound, "no route to %s: it is not reachable through running tunnels and peer links; see gst mesh nodes", name)
}
