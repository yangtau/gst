package wire

import "time"

// Methods of peer connections, which carry each call on its own stream.
const (
	MethodNodeInfo = "node.info" // no params; returns NodeInfo
)

// FeatureYamux in a peer Hello says that the connection continues as a
// yamux session, with one call per stream.
const FeatureYamux = "yamux"

// NodeInfo identifies a gst node in the mesh.
type NodeInfo struct {
	ID       string   `json:"id"`   // random and stable, set by gst mesh enable
	Name     string   `json:"name"` // chosen by the node's user; not unique
	Software string   `json:"software"`
	Features []string `json:"features,omitempty"`
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
