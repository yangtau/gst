package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func TestUIState(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "web", "myhost", "-L", "8080:localhost:80")
	e.mustGst("add", "db", "myhost", "-L", "15432:localhost:5432")
	e.mustGst("stop", "db")
	srv := httptest.NewServer(e.app(io.Discard, io.Discard).uiHandler("secret"))
	defer srv.Close()

	get := func(path, auth string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}
	state := func() uiState {
		t.Helper()
		res := get("/api/state", "Bearer secret")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("state: %s", res.Status)
		}
		var s uiState
		if err := json.NewDecoder(res.Body).Decode(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// The page itself needs no token; the state does.
	res := get("/", "")
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "app.js") {
		t.Fatalf("page: %s %q", res.Status, body)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("Content-Security-Policy %q", csp)
	}
	for _, path := range []string{"/app.js", "/style.css"} {
		if res := get(path, ""); res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %s", path, res.Status)
		}
	}
	for _, auth := range []string{"", "Bearer wrong", "secret"} {
		if res := get("/api/state", auth); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("state with %q: %s", auth, res.Status)
		}
	}

	s := state()
	if !s.Daemon || s.Node != nil || s.Topology != nil || len(s.Tunnels) != 2 {
		t.Fatalf("state without the mesh: %+v", s)
	}
	if s.Tunnels[0].Name != "db" || s.Tunnels[0].Status != statusStopped || s.Tunnels[1].Name != "web" || s.Tunnels[1].Status != statusRunning {
		t.Fatalf("tunnels %+v", s.Tunnels)
	}

	// The state follows the configuration and the daemon at each request.
	e.mustGst("mesh", "enable", "--name", "mini")
	e.back.topology = wire.Topology{Enabled: true, Self: "a1", Nodes: []wire.TopoNode{{ID: "a1", Name: "mini", Gst: true, Reachable: true}}}
	e.back.states = map[string]wire.TunnelState{"web": {Name: "web", State: wire.StateDown, Error: "SSH connection closed"}}
	s = state()
	if s.Node == nil || s.Node.Name != "mini" || s.Topology == nil || len(s.Topology.Nodes) != 1 {
		t.Fatalf("state with the mesh: %+v", s)
	}
	if web := s.Tunnels[1]; web.Status != statusDown || web.Error != "SSH connection closed" {
		t.Fatalf("web %+v", web)
	}
}

func TestUIRejectsOtherHosts(t *testing.T) {
	e := newEnv(t)
	h := e.app(io.Discard, io.Discard).uiHandler("secret")
	for host, want := range map[string]int{
		"127.0.0.1:8370":   http.StatusOK,
		"localhost:8370":   http.StatusOK,
		"[::1]:8370":       http.StatusOK,
		"evil.example.com": http.StatusForbidden,
		"10.0.0.1:8370":    http.StatusForbidden,
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %s: %d, want %d", host, rec.Code, want)
		}
	}
}

func TestUICommand(t *testing.T) {
	e := newEnv(t)
	e.expectCode(exitUsage, "ui", "--port", "70000")
	e.expectCode(exitUsage, "ui", "extra")
}
