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

// exposeStateText is an exposed port's state with the error of one that
// failed.
func exposeStateText(e wire.ExposeState) string {
	if e.Error != "" {
		return e.State + ": " + e.Error
	}
	return e.State
}

// linkText shows a link as the machines at its ends, such as laptop->gateway,
// and adds the link's own name, which gst remove takes, when it differs
// from the machine it leads to.
func linkText(from, to, name string) string {
	if to == "" || to == name {
		return from + "->" + name
	}
	return from + "->" + to + " (" + name + ")"
}

// stateLinkText is linkText for a link of the machine that from names; the
// machine at the other end is known while the link runs.
func stateLinkText(from string, l wire.LinkState) string {
	to := ""
	if l.Peer != nil {
		to = l.Peer.Name
	}
	return linkText(from, to, l.Name)
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
