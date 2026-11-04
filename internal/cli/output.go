package cli

import (
	"encoding/json"
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

// peerText names the node at the other end of a running link.
func peerText(peer *wire.NodeInfo) string {
	if peer == nil {
		return "-"
	}
	return peer.Name + " (" + shortID(peer.ID) + ")"
}

func shortID(id string) string { return id[:min(8, len(id))] }

func table(out io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
}

func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
