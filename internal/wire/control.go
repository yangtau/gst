package wire

import "time"

// Methods of control connections.
const (
	MethodStatus  = "daemon.status" // no params; returns Status
	MethodReload  = "daemon.reload" // no params; returns Status after syncing with the configuration
	MethodRestart = "link.restart"  // RestartParams; returns Status
	MethodNodes   = "mesh.nodes"    // no params; returns Topology
	// MethodNodeStatus ("mesh.status") takes NodeParams and returns the
	// Status of that machine's daemon, asked through the mesh.
	MethodNodeStatus = "mesh.status"
	// MethodConnect ("mesh.connect") takes ConnectParams; on success the
	// control connection carries the bytes of a connection to the target.
	MethodConnect = "mesh.connect"
)

// Link states reported by the daemon.
const (
	StateStopped    = "stopped"    // configured but stopped
	StateConnecting = "connecting" // connecting over SSH or starting the peer session
	StateRunning    = "running"    // the peer session is up
	StateDown       = "down"       // failed; waiting to retry
)

// Status describes the daemon, its configuration, and what it runs.
type Status struct {
	Software string      `json:"software"`
	Build    string      `json:"build"`
	PID      int         `json:"pid"`
	Config   string      `json:"config"`
	Node     NodeInfo    `json:"node"`
	Proxy    Listener    `json:"proxy"` // Listen is empty when the proxy is off
	Links    []LinkState `json:"links"`
	Maps     []MapState  `json:"maps"`
	Routes   []Route     `json:"routes"`
}

// Listener is the state of one of the daemon's listeners, identified by
// its listen address. Error says why it does not listen.
type Listener struct {
	Listen string `json:"listen"`
	Error  string `json:"error,omitempty"`
}

// MapState is a map and the state of its listener.
type MapState struct {
	Listener
	Machine string `json:"machine"`
	Target  string `json:"target"`
}

// Route sends the proxy's connections to the hosts that Match matches
// through the machine Via.
type Route struct {
	Match string `json:"match"`
	Via   string `json:"via"`
}

// LinkState is a configured link and its state. Error is the last
// failure, which may quote the remote gst. Peer is the node at the other
// end while the link runs.
type LinkState struct {
	Name        string    `json:"name"`
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

// RestartParams names the links to restart.
type RestartParams struct {
	Names []string `json:"names"`
}

// ConnectParams name a machine of the mesh, as the user refers to it, and
// the target address on it.
type ConnectParams struct {
	Node   string `json:"node"`
	Target Target `json:"target"`
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
	Exit      bool      `json:"exit"`
	Reachable bool      `json:"reachable"`
	Path      []string  `json:"path,omitempty"`   // node ids from this node, for reachable nodes
	Expires   time.Time `json:"expires,omitzero"` // when its record expires; zero for this node
}

// TopoEdge is a running link from one node to another.
type TopoEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Link string `json:"link"` // its name on From
}
