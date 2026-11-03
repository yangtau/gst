package cli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

//go:embed ui
var uiFiles embed.FS

func (a *app) uiCommand() *cobra.Command {
	var port int
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Show tunnels and the mesh in a browser",
		Long: `Show this machine's tunnels, its peer links, and the mesh as this machine
knows it in a web page that keeps itself up to date. gst ui serves the page
on 127.0.0.1 until interrupted, prints its address, and opens it in a
browser. The page can also start, stop, and restart this machine's
tunnels. The address ends with a secret that the page needs; anyone with
the address can use the page while gst ui runs.`,
		Example: `  gst ui
  gst ui --port 8370 --no-open   # on a server: ssh -L 8370:localhost:8370 myhost`,
		Args: a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			if port < 0 || port > 65535 {
				return usage("--port must be between 0 and 65535")
			}
			if _, err := a.backend(); err != nil {
				return err
			}
			listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err != nil {
				return withHint(fmt.Errorf("cannot listen on port %d: %w", port, err), "choose another port with --port, or 0 for any free one")
			}
			defer listener.Close()

			token := rand.Text()
			url := fmt.Sprintf("http://%s/#%s", listener.Addr(), token)
			srv := &http.Server{Handler: a.uiHandler(token), ReadHeaderTimeout: 10 * time.Second}
			stop := context.AfterFunc(cmd.Context(), func() { srv.Close() })
			defer stop()

			fmt.Fprintf(a.stdout, "gst ui: %s\nPress Ctrl-C to stop.\n", url)
			if !noOpen && a.openURL != nil {
				if err := a.openURL(url); err != nil {
					fmt.Fprintf(a.stderr, "note: no browser opened (%v); open the address above\n", err)
				}
			}
			if err := srv.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}),
	}
	cmd.Flags().IntVar(&port, "port", 0, "port on 127.0.0.1 to serve on (default: any free one)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "only print the address; do not open a browser")
	return cmd
}

// openBrowser opens url in the user's browser without waiting for it.
func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// uiState is what the gst ui page shows. Only the page reads it; unlike
// --json output, its fields may change.
type uiState struct {
	Software string `json:"software"`
	// Daemon says whether the gst daemon answered; without it, tunnel
	// states, peers, and the topology are unknown.
	Daemon   bool            `json:"daemon"`
	Node     *wire.NodeInfo  `json:"node,omitempty"` // this machine, when the mesh is enabled
	Tunnels  []tunnelView    `json:"tunnels"`
	Peers    []wire.PeerLink `json:"peers"`
	Topology *wire.Topology  `json:"topology,omitempty"`
	Errors   []string        `json:"errors,omitempty"`
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
	s := uiState{Software: software, Tunnels: []tunnelView{}, Peers: []wire.PeerLink{}}
	states, ok := b.States(ctx)
	s.Daemon = ok
	for _, name := range sortedNames(f) {
		t := f.Tunnels[name]
		status, lastErr := statusOf(name, t, states)
		s.Tunnels = append(s.Tunnels, newTunnelView(name, t, status, lastErr))
	}
	if f.Mesh == nil {
		return s, nil
	}
	s.Node = &wire.NodeInfo{ID: f.Mesh.ID, Name: f.Mesh.Name}
	if !ok {
		return s, nil
	}
	if peers, err := b.Peers(ctx); err != nil {
		s.Errors = append(s.Errors, "peer links: "+err.Error())
	} else if peers.Links != nil {
		s.Peers = peers.Links
	}
	if topo, err := b.Nodes(ctx); err != nil {
		s.Errors = append(s.Errors, "mesh nodes: "+err.Error())
	} else {
		s.Topology = &topo
	}
	return s, nil
}

// uiHandler serves the page and its state. The state needs the token as a
// bearer credential, which a page from another site cannot send without
// a CORS preflight that the handler never approves; the Host check stops
// DNS rebinding.
func (a *app) uiHandler(token string) http.Handler {
	authorized := func(r *http.Request) bool {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		return ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
	}
	var acting sync.Mutex // one change at a time, as from one command line
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
	mux.HandleFunc("POST /api/tunnels/{name}/{action}", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			http.Error(w, "open the address that gst ui printed", http.StatusUnauthorized)
			return
		}
		acting.Lock()
		defer acting.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		result, status := a.uiAction(ctx, r.PathValue("name"), r.PathValue("action"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(result)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "gst ui answers only on localhost", http.StatusForbidden)
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

// loopbackHost reports whether an HTTP Host header names this machine.
func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// uiResult is the outcome of a change made on the page: what the command
// line would print, and its error and hint.
type uiResult struct {
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
	Hint    string `json:"hint,omitempty"`
}

// uiAction starts, stops, or restarts one tunnel as gst start, stop, and
// restart do, and returns what they would print.
func (a *app) uiAction(ctx context.Context, name, action string) (uiResult, int) {
	var run func(ctx context.Context, out io.Writer, f *config.File, names []string, all bool) error
	switch action {
	case "start":
		run = a.startTunnels
	case "stop":
		run = a.stopTunnels
	case "restart":
		run = a.restartTunnels
	default:
		return uiResult{Error: fmt.Sprintf("unknown action %q", action)}, http.StatusNotFound
	}
	var out strings.Builder
	f, err := a.load()
	if err == nil {
		_, err = lookup(f, name)
		if err == nil {
			err = run(ctx, &out, f, []string{name}, false)
		}
	}
	result := uiResult{Message: strings.TrimSpace(out.String())}
	if err == nil {
		return result, http.StatusOK
	}
	result.Error = err.Error()
	var hinted *hintError
	if errors.As(err, &hinted) {
		result.Hint = hinted.hint
	}
	return result, http.StatusConflict
}
