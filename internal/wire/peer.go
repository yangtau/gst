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
	// MethodPeerStatus ("node.status") takes no params and returns the
	// Status of the callee's daemon.
	MethodPeerStatus = "node.status"
	// MethodDirectOffer ("direct.offer") takes a DirectOffer and returns
	// the callee's, after which the two nodes try a direct link.
	MethodDirectOffer = "direct.offer"
	// MethodLinkRemove ("link.remove") takes LinkRemove and returns
	// nothing: the caller is about to remove the link for good.
	MethodLinkRemove = "link.remove"
	// MethodDirectTrace ("direct.trace") takes DirectTraceParams and
	// returns DirectTrace: what the callee did in its latest try at a
	// direct link with that node.
	MethodDirectTrace = "direct.trace"
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

// Target is an address to connect to: network "tcp" with host:port,
// "unix" with a socket path, or NetworkGst.
type Target struct {
	Network string `json:"network"`
	Address string `json:"address"`
}

// NetworkGst, with no address, is the gst node at the end of the path
// itself: the connection carries peer calls to it, one after another.
const NetworkGst = "gst"

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
	// Direct marks a direct link, which has no name, that this node
	// opened. A direct link that the other node opened has no marks.
	Direct bool `json:"direct,omitempty"`
	// Local and Remote are the addresses of a direct link as the node
	// that opened it sees them, each an IP:port.
	Local  string `json:"local,omitempty"`
	Remote string `json:"remote,omitempty"`
}

// LinkRemove names a link that its opener is removing.
type LinkRemove struct {
	Node string `json:"node"` // the caller's id
	Link string `json:"link"` // the link's name, as in the caller's record
}

// DirectTraceParams name the node at the other end of a direct link.
type DirectTraceParams struct {
	Node string `json:"node"` // its id
}

// DirectTrace is what a node did in its latest try at a direct link, in
// order. It holds addresses, so it stays out of the daemon log.
type DirectTrace struct {
	Events []DirectEvent `json:"events"`
}

// DirectEvent is one thing that a try at a direct link did.
type DirectEvent struct {
	Time time.Time `json:"time"`
	Text string    `json:"text"`
}

// DirectOffer is one node's side of a direct link: in the params of
// direct.offer, the caller's, and in its result, the callee's.
type DirectOffer struct {
	Node        string   `json:"node,omitempty"`  // the caller's id; only in the params
	Token       string   `json:"token,omitempty"` // proves that a probe belongs to this try; the caller's counts
	Addresses   []string `json:"addresses"`       // IP:port to probe
	Fingerprint string   `json:"fingerprint"`     // SHA-256 of the certificate that the node will present
}
