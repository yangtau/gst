package cli

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

//go:embed ui
var uiFiles embed.FS

func (a *app) uiCommand() *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Show the network in a web page",
		Long: `Show the machines of the network, and the gst IP, exposed ports, and
links of each, in a web page that keeps itself up to date. gst ui serves
the page on this machine's gst IP until interrupted, so that every machine
of the network, this one included, can open it at <name>.gst. It prints
the address; open it in a browser. It needs the network up.

The address ends with a secret that the page needs; anyone with the
address can see the page while gst ui runs.`,
		Example: `  gst ui
  gst ui --port 8370   # then open http://<name>.gst:8370/#... on any machine of the network`,
		Args: a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			if port < 0 || port > 65535 {
				return usage("--port must be between 0 and 65535")
			}
			if _, err := a.backend(); err != nil {
				return err
			}
			st := a.status(cmd.Context())
			if st == nil {
				return withHint(errNoDaemon, "start it with `gst up`")
			}
			if st.Net == nil || st.Net.Address == "" || st.Net.Interface == "" {
				err, hint := errors.New("this machine is not on the gst network"), "run `gst up`"
				if st.Net != nil && st.Net.Error != "" {
					err, hint = fmt.Errorf("%w: %s", err, st.Net.Error), cmp.Or(st.Net.Hint, hint)
				}
				return withHint(err, hint)
			}
			listener, err := net.Listen("tcp", net.JoinHostPort(st.Net.Address, strconv.Itoa(port)))
			if err != nil {
				return withHint(fmt.Errorf("cannot listen on port %d of the gst IP: %w", port, err), "choose another port with --port, or 0 for any free one")
			}
			defer listener.Close()

			token := rand.Text()
			srv := &http.Server{Handler: a.uiHandler(token, st.Net.Address), ReadHeaderTimeout: 10 * time.Second}
			stop := context.AfterFunc(cmd.Context(), func() { srv.Close() })
			defer stop()

			fmt.Fprintf(a.stdout, "gst ui: http://%s:%d/#%s\nPress Ctrl-C to stop.\n", nodeHost(st.Node.Name), listener.Addr().(*net.TCPAddr).Port, token)
			if err := srv.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}),
	}
	cmd.Flags().IntVar(&port, "port", 0, "port on the gst IP to serve on (default: any free one)")
	return cmd
}

// nodeHost is the host name of the machine named name on the gst network.
func nodeHost(name string) string { return name + ".gst" }

// uiState is what the gst ui page shows of this machine and the mesh.
// Only the page reads it, so its fields may change.
type uiState struct {
	Software string       `json:"software"`
	Down     bool         `json:"down"` // gst down
	Node     *config.Node `json:"node,omitempty"`
	// Status is what the daemon reports; nil when no daemon answers.
	Status   *wire.Status   `json:"status,omitempty"`
	Topology *wire.Topology `json:"topology,omitempty"`
	Errors   []string       `json:"errors,omitempty"`
}

func (a *app) uiState(ctx context.Context) (uiState, error) {
	f, err := a.load()
	if err != nil {
		return uiState{}, err
	}
	b, err := a.backend()
	if err != nil {
		return uiState{}, err
	}
	s := uiState{Software: software, Down: f.Down, Node: f.Node, Status: a.status(ctx)}
	if s.Status == nil {
		return s, nil
	}
	if topo, err := b.Nodes(ctx); err != nil {
		s.Errors = append(s.Errors, "machines: "+err.Error())
	} else {
		s.Topology = &topo
	}
	return s, nil
}

// uiHandler serves the page and its state; gstIP is the gst IP it serves
// on. The state needs the token as a
// bearer credential, which a page from another site cannot send without
// a CORS preflight that the handler never approves; the Host check stops
// DNS rebinding.
func (a *app) uiHandler(token, gstIP string) http.Handler {
	authorized := func(r *http.Request) bool {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		return ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
	}
	mux := http.NewServeMux()
	static, err := fs.Sub(uiFiles, "ui")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			http.Error(w, "open the address that gst ui printed", http.StatusUnauthorized)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		state, err := a.uiState(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(state)
	})
	mux.HandleFunc("GET /api/nodes/{node}", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			http.Error(w, "open the address that gst ui printed", http.StatusUnauthorized)
			return
		}
		b, err := a.backend()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		st, err := b.NodeStatus(r.Context(), r.PathValue("node"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(st)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !uiHost(r.Host, gstIP) {
			http.Error(w, "gst ui answers only to <name>.gst and the gst IP", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

// uiHost reports whether an HTTP Host header names a machine of the gst
// network: a name under .gst, or gstIP, this machine's gst IP. Only gst
// answers for .gst names, so another site cannot point its name at the
// page (DNS rebinding).
func uiHost(host, gstIP string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	if name, ok := strings.CutSuffix(host, ".gst"); ok {
		return config.ValidateName(name) == nil
	}
	return host != "" && host == gstIP
}
