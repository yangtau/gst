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
	// stream leads to the node at the end of the path, and carries calls
	// to its peer methods, one after another.
	MethodRelayOpen = "relay.open"
	// MethodPeerStatus ("node.status") takes no params and returns the
	// Status of the callee's daemon.
	MethodPeerStatus = "node.status"
	// MethodDirectOffer ("direct.offer") takes a DirectOffer and returns
	// the callee's, after which the two nodes look for a UDP path.
	MethodDirectOffer = "direct.offer"
	// MethodDirectUpdate ("direct.update") takes the DirectOffer of a
	// running UDP path, with the caller's addresses now, and returns nothing:
	// the callee measures the path again.
	MethodDirectUpdate = "direct.update"
	// MethodLinkRemove ("link.remove") takes LinkRemove and returns
	// nothing: the caller is about to remove the link for good.
	MethodLinkRemove = "link.remove"
	// MethodDirectTrace ("direct.trace") takes DirectTraceParams and
	// returns DirectTrace: what the callee did in its latest try at a
	// UDP path with that node.
	MethodDirectTrace = "direct.trace"
	// MethodNetPackets ("net.packets") takes PacketsParams. On success the
	// stream carries the packets of the virtual network in both
	// directions, each after its length as a big-endian uint16.
	MethodNetPackets = "net.packets"
	// MethodNetNetworks ("net.networks") takes no params and returns
	// Networks: what the subnet of the virtual network must not overlap
	// on the callee's machine.
	MethodNetNetworks = "net.networks"
)

// FeatureYamux in a peer Hello says that the connection continues as a
// yamux session, with one call per stream. Peers require it.
const FeatureYamux = "yamux"

// RelayParams are the params of relay.open. Path lists the nodes that
// the stream passes, starting with the callee and ending with the node
// that answers the calls.
type RelayParams struct {
	Path []string `json:"path"`
}

// NodeInfo identifies a gst node.
type NodeInfo struct {
	ID       string `json:"id"`   // random and stable
	Name     string `json:"name"` // chosen by the node's user; not unique
	Software string `json:"software"`
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
	// Net is the node's part in the virtual network; nil while it takes
	// no part.
	Net *NetInfo `json:"net,omitempty"`
}

// NetInfo is what the other nodes need to reach a node over the virtual
// network. Subnet is the whole network's, which the nodes agree on: the
// one with the highest Since counts.
type NetInfo struct {
	Subnet    string `json:"subnet"`     // CIDR prefix
	Since     int64  `json:"since"`      // when a person chose the subnet, in Unix nanoseconds; negated when gst chose it
	Address   string `json:"address"`    // the node's IP address in Subnet
	PublicKey string `json:"public_key"` // its WireGuard key, in hexadecimal
}

// PacketsParams are the params of net.packets.
type PacketsParams struct {
	Node string `json:"node"` // the caller's id
}

// Networks lists the IP prefixes in use on a machine.
type Networks struct {
	Prefixes []string `json:"prefixes"`
}

// Edge is one of a node's running links.
type Edge struct {
	Name string `json:"name"` // the link's name on the node
	Peer string `json:"peer"` // the node id at its other end
}

// LinkRemove names a link that its opener is removing.
type LinkRemove struct {
	Node string `json:"node"` // the caller's id
	Link string `json:"link"` // the link's name, as in the caller's record
}

// DirectTraceParams name the node at the other end of a UDP path.
type DirectTraceParams struct {
	Node string `json:"node"` // its id
}

// DirectTrace is what a node did in its latest try at a UDP path, in
// order. It holds addresses, so it stays out of the daemon log.
type DirectTrace struct {
	Events []DirectEvent `json:"events"`
}

// DirectEvent is one thing that a try at a UDP path did.
type DirectEvent struct {
	Time time.Time `json:"time"`
	Text string    `json:"text"`
}

// DirectOffer is one node's side of a try at a UDP path: in the params of
// direct.offer, the caller's, and in its result, the callee's.
type DirectOffer struct {
	Node      string   `json:"node,omitempty"`  // the caller's id; only in the params
	Token     string   `json:"token,omitempty"` // proves that a probe belongs to this try; the caller's counts
	Addresses []string `json:"addresses"`       // IP:port to probe
}
