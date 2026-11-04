package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/sshconn"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// linkView is a link as gst link list and show print it. Its JSON form is
// part of the command line interface.
type linkView struct {
	Name                     string         `json:"name"`
	Status                   string         `json:"status"`
	Error                    string         `json:"error,omitempty"`
	Peer                     *wire.NodeInfo `json:"peer,omitempty"` // the node at the other end, while running
	Destination              string         `json:"destination"`
	IdentityFile             string         `json:"identity_file,omitempty"`
	KnownHostsFile           string         `json:"known_hosts_file,omitempty"`
	ConnectTimeoutSeconds    int64          `json:"connect_timeout_seconds,omitempty"`
	KeepaliveIntervalSeconds int64          `json:"keepalive_interval_seconds,omitempty"`
	KeepaliveCountMax        int64          `json:"keepalive_count_max,omitempty"`
	RemoteGst                string         `json:"remote_gst,omitempty"`
	Resolved                 *resolved      `json:"resolved,omitempty"`
	ResolveError             string         `json:"resolve_error,omitempty"`
}

// resolved is what the link uses after applying the ssh configuration.
type resolved struct {
	Server                   string   `json:"server"`
	User                     string   `json:"user"`
	IdentityFiles            []string `json:"identity_files"`
	KnownHostsFiles          []string `json:"known_hosts_files"`
	ConnectTimeoutSeconds    int64    `json:"connect_timeout_seconds"`
	KeepaliveIntervalSeconds int64    `json:"keepalive_interval_seconds"`
	KeepaliveTimeoutSeconds  int64    `json:"keepalive_timeout_seconds"`
}

func newLinkView(name string, l *config.Link, st *wire.Status) linkView {
	status, state := linkStatus(name, l, st)
	return linkView{
		Name:                     name,
		Status:                   status,
		Error:                    state.Error,
		Peer:                     state.Peer,
		Destination:              l.Destination,
		IdentityFile:             l.IdentityFile,
		KnownHostsFile:           l.KnownHostsFile,
		ConnectTimeoutSeconds:    l.ConnectTimeoutSeconds,
		KeepaliveIntervalSeconds: l.KeepaliveIntervalSeconds,
		KeepaliveCountMax:        l.KeepaliveCountMax,
		RemoteGst:                l.RemoteGst,
	}
}

func resolvedView(opts sshconn.Options) *resolved {
	return &resolved{
		Server:                   opts.Server,
		User:                     opts.User,
		IdentityFiles:            opts.IdentityFiles,
		KnownHostsFiles:          opts.KnownHostsFiles,
		ConnectTimeoutSeconds:    int64(opts.ConnectTimeout / time.Second),
		KeepaliveIntervalSeconds: int64(opts.KeepaliveInterval / time.Second),
		KeepaliveTimeoutSeconds:  int64(opts.KeepaliveTimeout / time.Second),
	}
}

// statusText is the status with the last error of a link that is not running.
func (v linkView) statusText() string {
	if v.Error != "" && v.Status != statusRunning {
		return v.Status + " (last error: " + v.Error + ")"
	}
	return v.Status
}

// peerText names the node at the other end of a running link.
func (v linkView) peerText() string {
	if v.Peer == nil {
		return "-"
	}
	return v.Peer.Name + " (" + shortID(v.Peer.ID) + ")"
}

func (v linkView) write(out io.Writer) error {
	w := table(out)
	row := func(key, value string) { fmt.Fprintf(w, "%s:\t%s\n", key, value) }
	row("Link", v.Name)
	row("Status", v.statusText())
	row("Peer", v.peerText())
	row("Destination", v.Destination)
	if r := v.Resolved; r != nil {
		row("Server", r.User+"@"+r.Server)
		row("Identities", strings.Join(r.IdentityFiles, ", "))
		row("Known hosts", strings.Join(r.KnownHostsFiles, ", "))
		row("Connect timeout", fmt.Sprintf("%ds", r.ConnectTimeoutSeconds))
		row("Keepalive", fmt.Sprintf("every %ds, give up after %ds without a reply", r.KeepaliveIntervalSeconds, r.KeepaliveTimeoutSeconds))
	} else {
		row("Server", "unresolved: "+v.ResolveError)
	}
	row("Remote gst", cmp.Or(v.RemoteGst, "gst"))
	return w.Flush()
}

func writeSteps(out io.Writer, steps []sshconn.Step) {
	w := table(out)
	for _, s := range steps {
		mark := "ok"
		if !s.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", mark, s.Name, s.Detail)
		if s.Hint != "" {
			fmt.Fprintf(w, "\t\thint: %s\n", s.Hint)
		}
	}
	w.Flush()
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
