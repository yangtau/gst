package wire

import "time"

// Methods of control connections.
const (
	MethodStatus = "daemon.status" // no params; returns Status
	MethodReload = "daemon.reload" // no params; returns Status after syncing with the configuration
	MethodNodes  = "mesh.nodes"    // no params; returns Topology
	// MethodNodeStatus ("mesh.status") takes NodeParams and returns the
	// Status of that machine's daemon, asked through the mesh.
	MethodNodeStatus = "mesh.status"
	// MethodPing ("mesh.ping") takes NodeParams, calls node.info on that
	// machine through the mesh, and returns Ping.
	MethodPing = "mesh.ping"
	// MethodNetPing ("net.ping") takes NetPingParams and returns NetPing: a
	// packet to that machine and back over the virtual network.
	MethodNetPing = "net.ping"
	// MethodDirect ("mesh.direct") takes NodeParams and returns
	// DirectReport.
	MethodDirect = "mesh.direct"
)

// Link states reported by the daemon.
const (
	StateConnecting = "connecting" // connecting over SSH or starting the peer session
	StateRunning    = "running"    // the peer session is up
	StateDown       = "down"       // failed; waiting to retry
)

// Status describes the daemon, its configuration, and what it runs.
type Status struct {
	Software string        `json:"software"`
	Build    string        `json:"build"`
	PID      int           `json:"pid"`
	Config   string        `json:"config"`
	Node     NodeInfo      `json:"node"`
	Links    []LinkState   `json:"links"`
	Exposes  []ExposeState `json:"exposes,omitempty"`
	Net      *NetState     `json:"net,omitempty"` // nil while this daemon cannot run the network
}

// NetState is this machine's part in the virtual network. Error says why
// it does not work; the rest is what it reached so far.
type NetState struct {
	Subnet    string    `json:"subnet,omitempty"`
	Address   string    `json:"address,omitempty"`
	Interface string    `json:"interface,omitempty"` // empty until the network is up
	Error     string    `json:"error,omitempty"`
	Hint      string    `json:"hint,omitempty"` // what to do about Error
	Peers     []NetPeer `json:"peers"`
}

// Ways that packets take to a peer.
const (
	PathDirect = "direct" // over UDP
	PathSSH    = "ssh"    // along the SSH links of the mesh
)

// NetPeer is another machine of the virtual network.
type NetPeer struct {
	Node      string    `json:"node"` // its id
	Name      string    `json:"name"`
	Address   string    `json:"address"`
	Path      string    `json:"path"`               // PathDirect or PathSSH
	Endpoint  string    `json:"endpoint,omitempty"` // the IP:port of the other end of a UDP path
	Handshake time.Time `json:"handshake,omitzero"` // the latest WireGuard handshake
	Received  uint64    `json:"received"`           // bytes
	Sent      uint64    `json:"sent"`               // bytes
}

// LinkState is a configured link and its state. Error is the last
// failure, which may quote the remote gst. Peer is the node at the other
// end while the link runs.
type LinkState struct {
	Destination string    `json:"destination"`
	State       string    `json:"state"`
	Since       time.Time `json:"since,omitzero"`
	Error       string    `json:"error,omitempty"`
	Reconnects  int       `json:"reconnects"`
	Peer        *NodeInfo `json:"peer,omitempty"`
}

// NodeParams name a machine of the mesh, as the user refers to it.
type NodeParams struct {
	Node string `json:"node"`
}

// Ping is a machine's answer over the SSH links of the mesh: the machine,
// the machines in between, in order, and how long the answer took, in
// milliseconds, which includes setting up the relay at each machine on the
// way.
type Ping struct {
	Node   NodeInfo   `json:"node"`
	Via    []NodeInfo `json:"via"`
	TimeMS float64    `json:"time_ms"`
}

// NetPingParams asks for a reply over the virtual network.
type NetPingParams struct {
	Node string `json:"node"`
	Size int    `json:"size,omitempty"` // of the packet's UDP payload, in bytes
}

// NetPing is a reply over the virtual network: it took the way that every
// program's packets take, through the interface and WireGuard.
type NetPing struct {
	Node    NodeInfo `json:"node"`
	Address string   `json:"address"`
	// Path is PathDirect or PathSSH, and empty for this machine.
	Path     string     `json:"path,omitempty"`
	Endpoint string     `json:"endpoint,omitempty"` // the IP:port of the other end of a UDP path
	Via      []NodeInfo `json:"via"`                // the machines on the way, over SSH
	TimeMS   float64    `json:"time_ms"`
}

// DirectReport is the UDP path to a machine, or why there is none, and
// what this node and that machine did in their latest try at one.
type DirectReport struct {
	Node NodeInfo `json:"node"`
	// Off says that UDP paths are off on this machine.
	Off    bool        `json:"off,omitempty"`
	Trying bool        `json:"trying,omitempty"` // a try is running
	Path   *DirectPath `json:"path,omitempty"`   // the running path
	// Next is when a new try can start, after a failure.
	Next  time.Time   `json:"next,omitzero"`
	Local DirectTrace `json:"local"`
	Peer  DirectTrace `json:"peer"`
	// PeerError says why the machine's own trace is missing.
	PeerError string `json:"peer_error,omitempty"`
	// Stats says where packets get lost on this machine.
	Stats DirectStats `json:"stats"`
}

// DirectStats is what this machine knows of the packets of the network.
type DirectStats struct {
	Port        int      `json:"port,omitempty"`       // of the UDP sockets
	Interfaces  []string `json:"interfaces,omitempty"` // that they are bound to
	ReadBuffer  int      `json:"read_buffer,omitempty"`
	WriteBuffer int      `json:"write_buffer,omitempty"`
	// To the machine of the report: packets that UDP could not send, which
	// then took the SSH links, and packets lost as the queue of those was full.
	WriteErrors uint64 `json:"write_errors"`
	QueueDrops  uint64 `json:"queue_drops"`
	// From any machine: WireGuard packets from a source of no UDP path,
	// and packets lost as WireGuard could not keep up.
	Strays       uint64 `json:"strays"`
	InboundDrops uint64 `json:"inbound_drops"`
}

// DirectPath is a running UDP path as this node sees it: the IP:port at
// each end.
type DirectPath struct {
	Local  string `json:"local"`
	Remote string `json:"remote"`
}

// Topology is the mesh as one node knows it.
type Topology struct {
	Self  string     `json:"self"` // this node's id
	Nodes []TopoNode `json:"nodes"`
	Edges []TopoEdge `json:"edges"`
}

// TopoNode is a gst node of the mesh.
type TopoNode struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Software  string    `json:"software,omitempty"`
	Reachable bool      `json:"reachable"`
	Path      []string  `json:"path,omitempty"`   // node ids from this node, for reachable nodes
	Direct    bool      `json:"direct,omitempty"` // this node has a UDP path to it
	Expires   time.Time `json:"expires,omitzero"` // when its record expires; zero for this node
	Net       *NetInfo  `json:"net,omitempty"`    // its part in the virtual network
}

// TopoEdge is a running link from one node to another.
type TopoEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Link string `json:"link"` // its id on From
}

// States of exposed ports; StateRunning means listening.
const (
	StateWaiting = "waiting" // for the virtual network
	StateFailed  = "failed"  // could not listen; Error says why
	StateStopped = "stopped" // the daemon does not run the rule: gst is down or not reachable
)

// ExposeState distinguishes a listening port from one waiting for the network
// or unable to bind. Running does not imply that the target is reachable.
type ExposeState struct {
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	Target   string `json:"target"`
	Address  string `json:"address,omitempty"`
	State    string `json:"state"`
	Error    string `json:"error,omitempty"`
}
