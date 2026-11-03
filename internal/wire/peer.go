package wire

import "time"

// Methods of peer connections, which carry each call on its own stream.
const (
	MethodNodeInfo = "node.info" // no params; returns NodeInfo
	// MethodTopologySync ("topology.sync") takes SyncParams and returns
	// Records: the callee's records that are newer than the caller's.
	// The stream then carries further Records frames, one per change,
	// until either side closes it.
	MethodTopologySync = "topology.sync"
	// MethodRelayOpen ("relay.open") takes RelayParams. On success the
	// stream carries the bytes of a connection to the target.
	MethodRelayOpen = "relay.open"
)

// RelayParams are the params of relay.open. Path lists the nodes that
// the connection passes, starting with the callee and ending with the
// node that the target address is relative to. Each node passes the rest
// of the path to the next one; the last gst node on the path connects to
// the target itself or, when the last node is an SSH server, over its
// tunnel to that server.
type RelayParams struct {
	Path   []string `json:"path"`
	Target Target   `json:"target"`
}

// Target is an address to connect to: network "tcp" with host:port, or
// "unix" with a socket path.
type Target struct {
	Network string `json:"network"`
	Address string `json:"address"`
}

// FeatureYamux in a peer Hello says that the connection continues as a
// yamux session, with one call per stream.
const FeatureYamux = "yamux"

// NodeInfo identifies a gst node in the mesh.
type NodeInfo struct {
	ID       string   `json:"id"`   // random and stable, set by gst mesh enable
	Name     string   `json:"name"` // chosen by the node's user; not unique
	Software string   `json:"software"`
	Features []string `json:"features,omitempty"`
	// HostKeys are the SHA-256 fingerprints of the node's sshd host keys,
	// which tell that a tunnel's server is this node.
	HostKeys []string `json:"host_keys,omitempty"`
}

// SyncParams are the params of topology.sync: the sequence number of
// every record the caller has, by node id.
type SyncParams struct {
	Known map[string]uint64 `json:"known"`
}

// Records carries link-state records.
type Records struct {
	Records []Record `json:"records"`
}

// Record is a node's link-state record. Each node publishes only its own
// and raises Seq with every change; the others keep the record with the
// highest Seq until it expires and pass it on to their peers.
type Record struct {
	Node    NodeInfo  `json:"node"`
	Seq     uint64    `json:"seq"`
	Expires time.Time `json:"expires"`
	Edges   []Edge    `json:"edges"`
}

// Edge is one of a node's tunnels with mesh on.
type Edge struct {
	Tunnel      string `json:"tunnel"`
	Destination string `json:"destination"`
	State       string `json:"state"`              // the tunnel's state, as in TunnelState
	HostKey     string `json:"host_key,omitempty"` // the server's host key fingerprint, once connected
	Peer        string `json:"peer,omitempty"`     // the id of the gst node on the server, once linked
	Linked      bool   `json:"linked"`             // the peer link is up
}

// MethodPeers is the control method that lists peer links; no params,
// returns Peers.
const MethodPeers = "mesh.peers"

// Peers describes this node and its peer links.
type Peers struct {
	Enabled bool       `json:"enabled"`
	Node    NodeInfo   `json:"node"`
	Links   []PeerLink `json:"links"`
}

// MethodConnect is the control method that connects through the mesh;
// it takes ConnectParams and, on success, the control connection carries
// the bytes of the connection.
const MethodConnect = "mesh.connect"

// ConnectParams name the machine, by node name, id, or id prefix, and the
// target address on it.
type ConnectParams struct {
	Node   string `json:"node"`
	Target Target `json:"target"`
}

// MethodNodes is the control method that reports the mesh as this node
// knows it; no params, returns Topology.
const MethodNodes = "mesh.nodes"

// Topology is the mesh as one node knows it.
type Topology struct {
	Enabled bool       `json:"enabled"`
	Self    string     `json:"self"` // this node's id
	Nodes   []TopoNode `json:"nodes"`
	Edges   []TopoEdge `json:"edges"`
}

// TopoNode is a machine in the mesh: a gst node, identified by its node
// id, or an SSH server without gst, identified as "ssh:" plus its host key
// fingerprint, or as "tunnel:<node id>/<tunnel>" before the first
// connection.
type TopoNode struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"` // the node name; a tunnel name for servers without gst
	Gst       bool      `json:"gst"`
	Software  string    `json:"software,omitempty"`
	HostKeys  []string  `json:"host_keys,omitempty"`
	Reachable bool      `json:"reachable"`        // this node can reach it through running tunnels and peer links
	Expires   time.Time `json:"expires,omitzero"` // when its record expires, for gst nodes
}

// TopoEdge is a tunnel with mesh on from one node to another.
type TopoEdge struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Tunnel      string `json:"tunnel"`
	Destination string `json:"destination"`
	State       string `json:"state"`
	Linked      bool   `json:"linked"`
}

// Link directions.
const (
	LinkOut = "out" // over one of this node's tunnels
	LinkIn  = "in"  // a peer's tunnel to this node
)

// Link states.
const (
	LinkConnecting = "connecting"
	LinkUp         = "up"
	LinkDown       = "down" // failed; waiting to retry
)

// PeerLink is one link to a peer. Peer is empty until the link is up.
// Error is for interactive output: it may quote the remote gst.
type PeerLink struct {
	Direction string    `json:"direction"`
	Tunnel    string    `json:"tunnel,omitempty"` // for outgoing links
	State     string    `json:"state"`
	Since     time.Time `json:"since"`
	Peer      NodeInfo  `json:"peer"`
	Error     string    `json:"error,omitempty"`
}
