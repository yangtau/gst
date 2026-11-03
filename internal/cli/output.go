package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/tunnel"
)

// tunnelView is a tunnel as gst list and show print it. Its JSON
// form is part of the command line interface.
type tunnelView struct {
	Name                     string        `json:"name"`
	Status                   string        `json:"status"`
	Error                    string        `json:"error,omitempty"`
	Destination              string        `json:"destination"`
	IdentityFile             string        `json:"identity_file,omitempty"`
	KnownHostsFile           string        `json:"known_hosts_file,omitempty"`
	ConnectTimeoutSeconds    int64         `json:"connect_timeout_seconds,omitempty"`
	KeepaliveIntervalSeconds int64         `json:"keepalive_interval_seconds,omitempty"`
	KeepaliveCountMax        int64         `json:"keepalive_count_max,omitempty"`
	CleanupListeners         bool          `json:"cleanup_listeners"`
	Mesh                     bool          `json:"mesh"`
	RemoteGst                string        `json:"remote_gst,omitempty"`
	Via                      string        `json:"via,omitempty"`
	Forwards                 []forwardView `json:"forwards"`
	Resolved                 *resolved     `json:"resolved,omitempty"`
	ResolveError             string        `json:"resolve_error,omitempty"`
}

// resolved is what the tunnel uses after applying the ssh configuration.
type resolved struct {
	Server                   string   `json:"server"`
	User                     string   `json:"user"`
	IdentityFiles            []string `json:"identity_files"`
	KnownHostsFiles          []string `json:"known_hosts_files"`
	ConnectTimeoutSeconds    int64    `json:"connect_timeout_seconds"`
	KeepaliveIntervalSeconds int64    `json:"keepalive_interval_seconds"`
	KeepaliveTimeoutSeconds  int64    `json:"keepalive_timeout_seconds"`
}

type forwardView struct {
	Number    int    `json:"number"`
	Direction string `json:"direction"`
	Listen    string `json:"listen"`
	Target    string `json:"target"`
	Spec      string `json:"spec"`
}

func forwardViews(forwards []config.Forward) []forwardView {
	views := make([]forwardView, len(forwards))
	for i, f := range forwards {
		views[i] = forwardView{i + 1, f.Direction, f.Listen, f.Target, f.Spec()}
	}
	return views
}

func (a *app) view(ctx context.Context, name string, t *config.Tunnel) tunnelView {
	status, lastErr := a.status(ctx, name, t)
	return tunnelView{
		Name:                     name,
		Status:                   status,
		Error:                    lastErr,
		Destination:              t.Destination,
		IdentityFile:             t.IdentityFile,
		KnownHostsFile:           t.KnownHostsFile,
		ConnectTimeoutSeconds:    t.ConnectTimeoutSeconds,
		KeepaliveIntervalSeconds: t.KeepaliveIntervalSeconds,
		KeepaliveCountMax:        t.KeepaliveCountMax,
		CleanupListeners:         t.CleanupListeners,
		Mesh:                     t.Mesh,
		RemoteGst:                t.RemoteGst,
		Via:                      t.Via,
		Forwards:                 forwardViews(t.Forwards),
	}
}

func resolvedView(opts tunnel.Options) *resolved {
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

func (v tunnelView) write(out io.Writer) error {
	w := table(out)
	row := func(key, value string) { fmt.Fprintf(w, "%s:\t%s\n", key, value) }
	row("Tunnel", v.Name)
	if v.Error != "" && v.Status != statusRunning {
		row("Status", v.Status+" (last error: "+v.Error+")")
	} else {
		row("Status", v.Status)
	}
	row("Destination", v.Destination)
	if v.Via == config.ViaMesh {
		row("Server", "none; connects through the gst mesh from "+v.Destination)
	} else if r := v.Resolved; r != nil {
		row("Server", r.User+"@"+r.Server)
		row("Identities", strings.Join(r.IdentityFiles, ", "))
		row("Known hosts", strings.Join(r.KnownHostsFiles, ", "))
		row("Connect timeout", fmt.Sprintf("%ds", r.ConnectTimeoutSeconds))
		row("Keepalive", fmt.Sprintf("every %ds, give up after %ds without a reply", r.KeepaliveIntervalSeconds, r.KeepaliveTimeoutSeconds))
	} else {
		row("Server", "unresolved: "+v.ResolveError)
	}
	if v.Via == "" {
		cleanup := "off"
		if v.CleanupListeners {
			cleanup = "on"
		}
		row("Cleanup listeners", cleanup)
		mesh := "off"
		if v.Mesh {
			mesh = "on"
			if v.RemoteGst != "" {
				mesh += ", remote gst " + v.RemoteGst
			}
		}
		row("Mesh", mesh)
	}
	if len(v.Forwards) == 0 {
		row("Forwards", "none")
	} else {
		row("Forwards", "")
		for _, f := range v.Forwards {
			fmt.Fprintf(w, "  %d\t%s\n", f.Number, f.Spec)
		}
	}
	return w.Flush()
}

func writeSteps(out io.Writer, steps []tunnel.Step) {
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

func table(out io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
}

func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
