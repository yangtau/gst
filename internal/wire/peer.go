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

// FeatureYamux in a peer Hello says that the connection continues as a
// yamux session, with one call per stream. Peers require it.
const FeatureYamux = "yamux"

// RelayParams are the params of relay.open. Path lists the nodes that
// the connection passes, starting with the callee and ending with the
// node that connects to the target.
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

// NodeInfo identifies a gst node.
type NodeInfo struct {
	ID       string `json:"id"`   // random and stable
	Name     string `json:"name"` // chosen by the node's user; not unique
	Software string `json:"software"`
	// Exit says that the node connects to addresses that are not on
	// its machine for others.
	Exit bool `json:"exit,omitempty"`
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
	Links   []Edge    `json:"links"`
}

// Edge is one of a node's running links.
type Edge struct {
	Name string `json:"name"` // the link's name on the node
	Peer string `json:"peer"` // the node id at its other end
}
