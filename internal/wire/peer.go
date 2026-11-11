package wire

import (
	"encoding/json"
	"time"
)

// Methods that nodes call on each other through the mesh: a request
// message answered by a response message (internal/mesh/route).
const (
	// MethodPeerPing ("ping") takes no params and returns NodeInfo.
	MethodPeerPing = "ping"
	// MethodPeerStatus ("status") takes no params and returns the Status
	// of the callee's daemon.
	MethodPeerStatus = "status"
	// MethodDirectOffer ("direct.offer") takes a DirectOffer and returns
	// the callee's, after which the two nodes look for a UDP path.
	MethodDirectOffer = "direct.offer"
	// MethodDirectUpdate ("direct.update") takes the DirectOffer of a
	// running UDP path, with the caller's addresses now, and returns nothing:
	// the callee measures the path again.
	MethodDirectUpdate = "direct.update"
	// MethodDirectTrace ("direct.trace") takes no params and returns
	// DirectTrace: what the callee did in its latest tries at a UDP path
	// with the caller.
	MethodDirectTrace = "direct.trace"
	// MethodNetPrefixes ("net.prefixes") takes no params and returns
	// UsedPrefixes: what the subnet of the virtual network must not overlap
	// on the callee's machine.
	MethodNetPrefixes = "net.prefixes"
)

// NodeInfo identifies a gst node.
type NodeInfo struct {
	ID       string `json:"id"`   // random and stable
	Name     string `json:"name"` // chosen by the node's user; not unique
	Software string `json:"software,omitempty"`
}

// Membership is what a peer's Hello says of its place in the network.
type Membership struct {
	// Generation counts the times the node was added back after a
	// removal; a removal record covers the generations up to its own.
	Generation int64 `json:"generation"`
	// Admit says that a person added the link (gst add) and it has not
	// run yet: the other node may join again after a removal.
	Admit bool `json:"admit,omitempty"`
	// Removed is the removal record of the other node, in a refusal.
	Removed *Entry `json:"removed,omitempty"`
}

// Bye is the last frame of a link that a side ends on purpose.
type Bye struct {
	Error      *Error      `json:"error"`
	Membership *Membership `json:"membership,omitempty"`
}

// Entry is an entry of the table that every node keeps of the network
// (internal/mesh/state). Of two entries with one key, the higher Version
// counts; Value depends on the key: NodeValue for "node/<id>",
// RemovedValue for "removed/<id>", and SubnetValue for "subnet".
type Entry struct {
	Key     string          `json:"key"`
	Version []int64         `json:"version"`
	Value   json.RawMessage `json:"value"`
}

// Entries is the body of a sync frame.
type Entries struct {
	Entries []Entry `json:"entries"`
}

// NodeValue is a node as it describes itself: its version is its
// generation and a sequence number that follows the clock, so it also says
// when the node last published it.
type NodeValue struct {
	Name     string     `json:"name"`
	Software string     `json:"software"`
	Links    []NodeLink `json:"links"`
	// Net is the node's part in the virtual network; nil while it takes
	// no part.
	Net *NodeNet `json:"net,omitempty"`
}

// NodeLink is one of a node's running links.
type NodeLink struct {
	Peer   string `json:"peer"`   // the node id at its other end
	Opened bool   `json:"opened"` // this node opened it
}

// NodeNet is what the other nodes need to reach a node over the virtual
// network.
type NodeNet struct {
	Address   string `json:"address"`    // the node's IP address in the network's subnet
	PublicKey string `json:"public_key"` // its WireGuard key, in hexadecimal
}

// RemovedValue says who removed a node, and when; its version is the
// generation of the node that it removes.
type RemovedValue struct {
	By     string    `json:"by"`      // the id of the node that removed it
	ByName string    `json:"by_name"` // and its name then
	At     time.Time `json:"at"`
}

// SubnetValue is the subnet of the virtual network. Its version is
// [1, order] for a person's choice, which beats gst's; [0, -time] for
// gst's, where the earlier one counts; and [-1, -time] for gst's on a
// node alone, which loses to every network's.
type SubnetValue struct {
	Subnet string `json:"subnet"`
}

// UsedPrefixes lists the IPv4 prefixes in use on a machine: the prefixes of
// its interfaces, and the destinations of its routes.
type UsedPrefixes struct {
	Interfaces []string `json:"interfaces"`
	Routes     []string `json:"routes"`
}

// DirectTrace is what a node did in its latest try at a UDP path, and in
// the one before, which shows how that ended, each in order. It holds
// addresses, so it stays out of the daemon log.
type DirectTrace struct {
	Events   []DirectEvent `json:"events"`
	Previous []DirectEvent `json:"previous,omitempty"`
}

// DirectEvent is one thing that a try at a UDP path did.
type DirectEvent struct {
	Time time.Time `json:"time"`
	Text string    `json:"text"`
}

// DirectOffer is one node's side of a try at a UDP path: in the params of
// direct.offer, the caller's, and in its result, the callee's.
type DirectOffer struct {
	Token     string   `json:"token,omitempty"` // proves that a probe belongs to this try; the caller's counts
	Addresses []string `json:"addresses"`       // IP:port to probe
}
