package cli

import (
	"io"
	"text/tabwriter"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// linkStateText is a link's state with the last error of a link that is
// not running.
func linkStateText(l wire.LinkState) string {
	if l.Error != "" && l.State != wire.StateRunning {
		return l.State + " (last error: " + l.Error + ")"
	}
	return l.State
}

// exposeStateText is an exposed port's state with the error of one that
// failed.
func exposeStateText(e wire.ExposeState) string {
	if e.Error != "" {
		return e.State + ": " + e.Error
	}
	return e.State
}

// destinationText is a link's destination, with the machine at its other
// end, which is known while the link runs, when its name differs.
func destinationText(l wire.LinkState) string {
	if l.Peer == nil || l.Peer.Name == l.Destination {
		return l.Destination
	}
	return l.Destination + " (" + l.Peer.Name + ")"
}

func shortID(id string) string { return id[:min(8, len(id))] }

func table(out io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
}
