package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// uiClient talks to the gst ui handler of an environment.
type uiClient struct {
	t   *testing.T
	srv *httptest.Server
}

func newUIClient(t *testing.T, e *testEnv) *uiClient {
	t.Helper()
	srv := httptest.NewServer(e.app(io.Discard, io.Discard).uiHandler("secret"))
	t.Cleanup(srv.Close)
	return &uiClient{t, srv}
}

func (c *uiClient) do(method, path, auth string) *http.Response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.srv.URL+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { res.Body.Close() })
	return res
}

func (c *uiClient) rawState() string {
	c.t.Helper()
	res := c.do("GET", "/api/state", "Bearer secret")
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		c.t.Fatalf("state: %s %s", res.Status, body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		c.t.Fatalf("state Content-Type %q", ct)
	}
	return string(body)
}

func (c *uiClient) state() uiState {
	c.t.Helper()
	return decode[uiState](c.t, c.rawState())
}

func TestUIServesPageAndSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	c := newUIClient(t, e)

	// The page itself needs no token.
	res := c.do("GET", "/", "")
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "app.js") {
		t.Fatalf("page: %s %q", res.Status, body)
	}
	for _, path := range []string{"/app.js", "/style.css"} {
		if res := c.do("GET", path, ""); res.StatusCode != http.StatusOK {
			t.Errorf("%s: %s", path, res.Status)
		}
	}
	if res := c.do("GET", "/nope.js", ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown file: %s", res.Status)
	}

	// Every response, errors included, carries the security headers.
	// (The file server drops Cache-Control from its own 404s.)
	for _, res := range []*http.Response{res, c.do("GET", "/api/state", ""), c.do("POST", "/api/state", "")} {
		h := res.Header
		if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: Content-Security-Policy %q", res.Request.URL.Path, csp)
		}
		for key, want := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
			if got := h.Get(key); got != want {
				t.Errorf("%s: %s = %q, want %q", res.Request.URL.Path, key, got, want)
			}
		}
		// No CORS: another site cannot read the answers.
		if got := h.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s: Access-Control-Allow-Origin %q", res.Request.URL.Path, got)
		}
	}
}

func TestUIRequiresToken(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "web", "myhost")
	c := newUIClient(t, e)
	for _, auth := range []string{"", "Bearer wrong", "Bearer ", "Bearer secret2", "Bearer secre", "secret", "Basic secret", "bearer secret"} {
		if res := c.do("GET", "/api/state", auth); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("state with %q: %s", auth, res.Status)
		}
	}
	if res := c.do("GET", "/api/state", "Bearer secret"); res.StatusCode != http.StatusOK {
		t.Fatalf("state with the token: %s", res.Status)
	}
	// The token does not go in the URL.
	if res := c.do("GET", "/api/state?token=secret", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token in the query: %s", res.Status)
	}
}

func TestUIIsReadOnly(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "web", "myhost")
	c := newUIClient(t, e)
	e.resetCalls()
	for _, path := range []string{"/api/state", "/api/nodes/devbox", "/api/links/web/stop", "/api/links/web/restart", "/api/links/web/start"} {
		for _, method := range []string{"POST", "PUT", "DELETE"} {
			if res := c.do(method, path, "Bearer secret"); res.StatusCode == http.StatusOK {
				t.Errorf("%s %s: %s", method, path, res.Status)
			}
		}
	}
	if res := c.do("GET", "/api/links/web/stop", "Bearer secret"); res.StatusCode != http.StatusNotFound {
		t.Errorf("GET of the removed link action: %s", res.Status)
	}
	if e.link("web") == nil || len(e.back.calls) != 0 {
		t.Fatalf("the page changed something: link %+v, calls %v", e.link("web"), e.back.calls)
	}
}

func TestUIRejectsOtherHosts(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "web", "myhost")
	h := e.app(io.Discard, io.Discard).uiHandler("secret")
	for host, want := range map[string]int{
		"127.0.0.1:8370":                  http.StatusOK,
		"127.0.0.1":                       http.StatusOK,
		"localhost:8370":                  http.StatusOK,
		"LOCALHOST:8370":                  http.StatusOK,
		"localhost":                       http.StatusOK,
		"[::1]:8370":                      http.StatusOK,
		"[::1]":                           http.StatusOK,
		"127.1.2.3:8370":                  http.StatusOK,
		"evil.example.com":                http.StatusForbidden,
		"evil.example:80":                 http.StatusForbidden,
		"10.0.0.1:8370":                   http.StatusForbidden,
		"0.0.0.0:8370":                    http.StatusForbidden,
		"localhost.evil.example.com:8370": http.StatusForbidden,
		"":                                http.StatusForbidden,
	} {
		targets := []struct{ method, path string }{{"GET", "/"}, {"GET", "/api/state"}, {"GET", "/api/nodes/devbox"}}
		for _, target := range targets {
			req := httptest.NewRequest(target.method, target.path, nil)
			req.Host = host
			req.Header.Set("Authorization", "Bearer secret")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if want == http.StatusForbidden {
				if rec.Code != want {
					t.Errorf("%s %s with Host %q: %d, want %d", target.method, target.path, host, rec.Code, want)
				}
			} else if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
				t.Errorf("%s %s with Host %q: %d", target.method, target.path, host, rec.Code)
			}
		}
	}
}

func TestUIStateShape(t *testing.T) {
	e := newEnv(t)
	e.mustGst("name", "laptop")
	e.mustGst("add", "web", "myhost")
	e.mustGst("add", "db")
	e.back.topology = meshTopology()
	c := newUIClient(t, e)

	// The page reads these keys.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(c.rawState()), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"software", "down", "node", "status", "topology"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("state lacks %q: %s", key, c.rawState())
		}
	}
	for _, key := range []string{"errors", "daemon", "links"} {
		if _, ok := raw[key]; ok {
			t.Errorf("state has %q at the top level: %s", key, raw[key])
		}
	}

	s := c.state()
	if s.Software != software || s.Down {
		t.Fatalf("state %+v", s)
	}
	if s.Node == nil || s.Node.Name != "laptop" || !config.ValidNodeID(s.Node.ID) {
		t.Fatalf("node %+v", s.Node)
	}
	if s.Status == nil {
		t.Fatal("state has no status")
	}
	st := s.Status
	if len(st.Links) != 2 || st.Links[0].Name != "db" || st.Links[0].State != wire.StateRunning ||
		st.Links[1].Name != "web" || st.Links[1].State != wire.StateRunning || st.Links[1].Peer == nil || st.Links[1].Peer.Name != "web" {
		t.Fatalf("links %+v", st.Links)
	}
	if s.Topology == nil || s.Topology.Self != nodeID("aaaa1111") || len(s.Topology.Nodes) != 5 || len(s.Topology.Edges) != 3 {
		t.Fatalf("topology %+v", s.Topology)
	}

	// The state follows the configuration and the daemon at each request.
	e.back.linkStates["web"] = wire.LinkState{Name: "web", State: wire.StateDown, Error: "SSH connection closed"}
	s = c.state()
	if web := s.Status.Links[1]; web.State != wire.StateDown || web.Error != "SSH connection closed" || web.Peer != nil {
		t.Fatalf("web %+v", web)
	}
}

func TestUIStateWithoutDaemonOrConfig(t *testing.T) {
	e := newEnv(t)
	c := newUIClient(t, e)

	// A new machine has no node, status, or topology.
	body := c.rawState()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"node", "status", "topology", "errors"} {
		if _, ok := raw[key]; ok {
			t.Errorf("state has %s: %s", key, body)
		}
	}
	if s := c.state(); s.Down || s.Status != nil || s.Topology != nil || s.Software != software {
		t.Fatalf("state %+v", s)
	}

	// A machine whose daemon does not answer has its node, but no status or topology.
	e.mustGst("add", "web", "myhost")
	e.mustGst("down")
	s := c.state()
	if !s.Down || s.Status != nil || s.Topology != nil || s.Node == nil {
		t.Fatalf("state %+v", s)
	}
}

func TestUIStateReportsMeshErrors(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	e.back.nodesErr = errors.New("timed out")
	c := newUIClient(t, e)
	s := c.state()
	if s.Status == nil || s.Topology != nil || !slices.Equal(s.Errors, []string{"mesh nodes: timed out"}) {
		t.Fatalf("state %+v", s)
	}
}

func TestUIStateWithBrokenConfig(t *testing.T) {
	e := newEnv(t)
	write(t, e.config, "{")
	c := newUIClient(t, e)
	res := c.do("GET", "/api/state", "Bearer secret")
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "invalid JSON") {
		t.Fatalf("state: %s %q", res.Status, body)
	}
}

func TestLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost":       true,
		"localhost:80":    true,
		"LocalHost:80":    true,
		"127.0.0.1":       true,
		"127.0.0.1:8370":  true,
		"[::1]":           true,
		"[::1]:8370":      true,
		"::1":             true,
		"192.168.1.1":     false,
		"example.com":     false,
		"example.com:80":  false,
		"[2001:db8::1]:1": false,
		"":                false,
	} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// syncBuffer is a bytes.Buffer that can be read while a command writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestUICommandServes(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "web", "myhost")
	var stdout, stderr syncBuffer
	a := e.app(&stdout, &stderr)
	var opened []string
	a.openURL = func(url string) error { opened = append(opened, url); return errors.New("no display") }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- a.execute(ctx, []string{"--config", e.config, "ui"}) }()

	urlPattern := regexp.MustCompile(`gst ui: (http://127\.0\.0\.1:\d+/)#([0-9A-Za-z]+)\n`)
	var m []string
	deadline := time.Now().Add(5 * time.Second)
	for m == nil && time.Now().Before(deadline) {
		if m = urlPattern.FindStringSubmatch(stdout.String()); m == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if m == nil {
		t.Fatalf("no address printed: stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	contains(t, stdout.String(), "Press Ctrl-C to stop.\n")

	req, _ := http.NewRequest("GET", m[1]+"api/state", nil)
	req.Header.Set("Authorization", "Bearer "+m[2])
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var s uiState
	err = json.NewDecoder(res.Body).Decode(&s)
	res.Body.Close()
	if err != nil || res.StatusCode != http.StatusOK || s.Status == nil || len(s.Status.Links) != 1 {
		t.Fatalf("state: %s %+v %v", res.Status, s, err)
	}
	// The token printed is the only one that works.
	req.Header.Set("Authorization", "Bearer x"+m[2])
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %v %v", res, err)
	}

	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gst ui did not stop")
	}
	if len(opened) != 1 || opened[0] != m[1]+"#"+m[2] {
		t.Fatalf("opened %v", opened)
	}
	// A browser that does not open is a note, not a failure.
	contains(t, stderr.String(), "note: no browser opened (no display); open the address above\n")
}

func TestUICommandNoOpen(t *testing.T) {
	e := newEnv(t)
	var stdout syncBuffer
	a := e.app(&stdout, io.Discard)
	opened := false
	a.openURL = func(string) error { opened = true; return nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- a.execute(ctx, []string{"--config", e.config, "ui", "--no-open"}) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stdout.String(), "Press Ctrl-C") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if opened {
		t.Fatal("--no-open opened a browser")
	}
}

func TestUICommandErrors(t *testing.T) {
	e := newEnv(t)
	e.expectError(exitUsage, []string{"ui", "--port", "70000"}, "error: --port must be between 0 and 65535\n", "usage: gst ui")
	e.expectError(exitUsage, []string{"ui", "--port", "-1"}, "error: --port must be between 0 and 65535\n")
	e.expectError(exitUsage, []string{"ui", "extra"}, `error: unexpected argument "extra"`)

	// A port in use is a failure with a hint.
	var stdout syncBuffer
	a := e.app(&stdout, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- a.execute(ctx, []string{"--config", e.config, "ui", "--no-open"}) }()
	m := regexp.MustCompile(`127\.0\.0\.1:(\d+)`)
	var port string
	for deadline := time.Now().Add(5 * time.Second); port == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if p := m.FindStringSubmatch(stdout.String()); p != nil {
			port = p[1]
		}
	}
	if port == "" {
		cancel()
		t.Fatal("gst ui did not start")
	}
	e.expectError(exitFailure, []string{"ui", "--port", port}, "error: cannot listen on port "+port, "hint: choose another port with --port, or 0 for any free one\n")
	cancel()
	<-done
}
