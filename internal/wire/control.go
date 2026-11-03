package wire

import "time"

// Methods of control connections.
const (
	MethodStatus  = "daemon.status"  // no params; returns Status
	MethodReload  = "daemon.reload"  // no params; returns Status after syncing with the configuration
	MethodRestart = "tunnel.restart" // RestartParams; returns Status
	// MethodPeers ("mesh.peers") is declared with the peer types.
)

// Tunnel states reported by the daemon. A tunnel that the daemon does not
// run is absent from Status.
const (
	StateConnecting = "connecting" // connecting or starting its forwards
	StateRunning    = "running"    // connected with every forward listening
	StateDown       = "down"       // failed; waiting to retry
)

// Status describes the daemon and the tunnels it runs.
type Status struct {
	Software string        `json:"software"`
	Build    string        `json:"build"`
	PID      int           `json:"pid"`
	Config   string        `json:"config"`
	Tunnels  []TunnelState `json:"tunnels"`
}

// TunnelState is the state of one tunnel that the daemon runs. Error is
// the last failure and, like the log, omits addresses, users, and paths.
type TunnelState struct {
	Name       string    `json:"name"`
	State      string    `json:"state"`
	Since      time.Time `json:"since"`
	Error      string    `json:"error,omitempty"`
	Reconnects int       `json:"reconnects"`
}

// RestartParams names the tunnels to restart.
type RestartParams struct {
	Names []string `json:"names"`
}
