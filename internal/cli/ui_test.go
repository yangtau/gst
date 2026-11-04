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

func (c *uiClient) post(path, auth string) (int, uiResult) {
	c.t.Helper()
	res := c.do("POST", path, auth)
	var r uiResult
	json.NewDecoder(res.Body).Decode(&r)
	return res.StatusCode, r
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
	for _, res := range []*http.Response{res, c.do("GET", "/api/state", ""), c.do("POST", "/api/links/a/stop", "")} {
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
	e.mustGst("link", "add", "web", "myhost")
	c := newUIClient(t, e)
	for _, auth := range []string{"", "Bearer wrong", "Bearer ", "Bearer secret2", "Bearer secre", "secret", "Basic secret", "bearer secret"} {
		if res := c.do("GET", "/api/state", auth); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("state with %q: %s", auth, res.Status)
		}
		if code, _ := c.post("/api/links/web/stop", auth); code != http.StatusUnauthorized {
			t.Errorf("stop with %q: %d", auth, code)
		}
	}
	if e.link("web").Stopped {
		t.Fatal("a request without the token stopped the link")
	}
	if res := c.do("GET", "/api/state", "Bearer secret"); res.StatusCode != http.StatusOK {
		t.Fatalf("state with the token: %s", res.Status)
	}
	// The token does not go in the URL.
	if res := c.do("GET", "/api/state?token=secret", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token in the query: %s", res.Status)
	}
	// State is read-only and actions are POST-only.
	if res := c.do("POST", "/api/state", "Bearer secret"); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/state: %s", res.Status)
	}
	if res := c.do("GET", "/api/links/web/stop", "Bearer secret"); res.StatusCode == http.StatusOK || e.link("web").Stopped {
		t.Errorf("GET of an action: %s", res.Status)
	}
}

func TestUIRejectsOtherHosts(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "web", "myhost")
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
		targets := []struct{ method, path string }{{"GET", "/"}, {"GET", "/api/state"}}
		if want == http.StatusForbidden {
			targets = append(targets, struct{ method, path string }{"POST", "/api/links/web/stop"})
		}
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
	// A rebound page is refused before anything runs.
	if e.link("web").Stopped {
		t.Fatal("a request for another host stopped the link")
	}
}

func TestUIStateShape(t *testing.T) {
	e := newEnv(t)
	e.mustGst("name", "laptop")
	e.mustGst("link", "add", "web", "myhost")
	e.mustGst("link", "add", "db")
	e.mustGst("link", "stop", "db")
	e.mustGst("proxy", "1081")
	e.mustGst("map", "add", "15432", "devcn:5432")
	e.mustGst("route", "add", "corp.example.com", "devcn")
	e.back.topology = meshTopology()
	c := newUIClient(t, e)

	// The page reads these keys.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(c.rawState()), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"software", "daemon", "down", "node", "proxy", "links", "maps", "routes", "topology"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("state lacks %q: %s", key, c.rawState())
		}
	}
	if _, ok := raw["errors"]; ok {
		t.Errorf("state has errors: %s", raw["errors"])
	}

	s := c.state()
	if s.Software != software || !s.Daemon || s.Down {
		t.Fatalf("state %+v", s)
	}
	if s.Node == nil || s.Node.Name != "laptop" || !config.ValidNodeID(s.Node.ID) {
		t.Fatalf("node %+v", s.Node)
	}
	if s.Proxy != (proxyView{Address: "127.0.0.1:1081", Status: statusListening}) {
		t.Fatalf("proxy %+v", s.Proxy)
	}
	if len(s.Links) != 2 || s.Links[0].Name != "db" || s.Links[0].Status != statusStopped ||
		s.Links[1].Name != "web" || s.Links[1].Status != statusRunning || s.Links[1].Peer == nil || s.Links[1].Peer.Name != "web" || s.Links[1].Destination != "myhost" {
		t.Fatalf("links %+v", s.Links)
	}
	if !slices.Equal(s.Maps, []mapView{{Map: config.Map{Listen: "127.0.0.1:15432", Machine: "devcn", Target: "localhost:5432"}, Status: statusListening}}) {
		t.Fatalf("maps %+v", s.Maps)
	}
	if !slices.Equal(s.Routes, config.Routes{{Match: "corp.example.com", Via: "devcn"}}) {
		t.Fatalf("routes %+v", s.Routes)
	}
	if s.Topology == nil || s.Topology.Self != nodeID("aaaa1111") || len(s.Topology.Nodes) != 5 || len(s.Topology.Edges) != 3 {
		t.Fatalf("topology %+v", s.Topology)
	}

	// The state follows the configuration and the daemon at each request.
	e.back.linkStates["web"] = wire.LinkState{Name: "web", State: wire.StateDown, Error: "SSH connection closed"}
	e.back.listenErrors["127.0.0.1:15432"] = "address already in use"
	e.mustGst("exit", "on")
	s = c.state()
	if web := s.Links[1]; web.Status != statusDown || web.Error != "SSH connection closed" || web.Peer != nil {
		t.Fatalf("web %+v", web)
	}
	if m := s.Maps[0]; m.Status != statusFailed || m.Error != "address already in use" {
		t.Fatalf("map %+v", m)
	}
	if !s.Node.Exit {
		t.Fatalf("node %+v", s.Node)
	}
}

func TestUIStateWithoutDaemonOrConfig(t *testing.T) {
	e := newEnv(t)
	c := newUIClient(t, e)

	// A new machine has no node, links, maps, or routes; the lists are empty, not null.
	body := c.rawState()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"links", "maps", "routes"} {
		if string(raw[key]) != "[]" {
			t.Errorf("%s = %s, want []", key, raw[key])
		}
	}
	for _, key := range []string{"node", "topology"} {
		if _, ok := raw[key]; ok {
			t.Errorf("state has %s: %s", key, body)
		}
	}
	if s := c.state(); s.Daemon || s.Down || s.Topology != nil || s.Proxy.Status != statusNotRun || s.Proxy.Address != "127.0.0.1:1080" {
		t.Fatalf("state %+v", s)
	}

	// Links of a machine whose daemon does not answer are down, and there is no topology.
	e.mustGst("link", "add", "web", "myhost")
	e.mustGst("down")
	s := c.state()
	if s.Daemon || !s.Down || s.Topology != nil || s.Node == nil || len(s.Links) != 1 || s.Links[0].Status != statusDown {
		t.Fatalf("state %+v", s)
	}
}

func TestUIStateReportsMeshErrors(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	e.back.nodesErr = errors.New("timed out")
	c := newUIClient(t, e)
	s := c.state()
	if !s.Daemon || s.Topology != nil || !slices.Equal(s.Errors, []string{"mesh nodes: timed out"}) {
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

func TestUIActions(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "web", "myhost")
	e.mustGst("link", "add", "db")
	e.mustGst("link", "stop", "db")
	c := newUIClient(t, e)

	if code, r := c.post("/api/links/web/stop", "Bearer secret"); code != http.StatusOK || r.Message != "Stopped link web." || r.Error != "" {
		t.Fatalf("stop: %d %+v", code, r)
	}
	if !e.link("web").Stopped {
		t.Fatal("web still runs")
	}
	if s := c.state(); s.Links[1].Status != statusStopped {
		t.Fatalf("state after stop: %+v", s.Links)
	}
	if code, r := c.post("/api/links/db/start", "Bearer secret"); code != http.StatusOK || r.Message != "Started link db." {
		t.Fatalf("start: %d %+v", code, r)
	}
	if e.link("db").Stopped {
		t.Fatal("db is stopped")
	}
	e.resetCalls()
	if code, r := c.post("/api/links/db/restart", "Bearer secret"); code != http.StatusOK || r.Message != "Restarted link db." || !slices.Equal(e.back.calls, []string{"sync", "restart db"}) {
		t.Fatalf("restart: %d %+v %v", code, r, e.back.calls)
	}
	// Starting a started link and stopping a stopped one are not errors.
	if code, r := c.post("/api/links/db/start", "Bearer secret"); code != http.StatusOK || r.Message != "Link db is already started." {
		t.Fatalf("start again: %d %+v", code, r)
	}
	if code, r := c.post("/api/links/web/stop", "Bearer secret"); code != http.StatusOK || r.Message != "Link web is already stopped." {
		t.Fatalf("stop again: %d %+v", code, r)
	}
}

func TestUIActionErrors(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "web", "myhost")
	e.mustGst("link", "stop", "web")
	c := newUIClient(t, e)

	code, r := c.post("/api/links/web/restart", "Bearer secret")
	if code != http.StatusConflict || r.Error != "link web is stopped" || r.Hint != "start it with `gst link start web`" {
		t.Fatalf("restart a stopped link: %d %+v", code, r)
	}
	code, r = c.post("/api/links/nope/start", "Bearer secret")
	if code != http.StatusConflict || r.Error != "no link named nope" || !strings.Contains(r.Hint, "links: web") {
		t.Fatalf("start a missing link: %d %+v", code, r)
	}
	for _, action := range []string{"remove", "delete", "START", "", "stop%20web"} {
		code, r := c.post("/api/links/web/"+action, "Bearer secret")
		if action == "" {
			// No action is no route.
			if code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
				t.Errorf("empty action: %d", code)
			}
			continue
		}
		if code != http.StatusNotFound || !strings.Contains(r.Error, "unknown action") {
			t.Errorf("action %q: %d %+v", action, code, r)
		}
	}

	// A link that cannot connect is not started.
	delete(e.hosts, "myhost")
	code, r = c.post("/api/links/web/start", "Bearer secret")
	if code != http.StatusConflict || !strings.Contains(r.Error, "start web:") || r.Hint == "" {
		t.Fatalf("start a link that cannot connect: %d %+v", code, r)
	}
	if !e.link("web").Stopped {
		t.Fatal("web was started")
	}

	// A failing daemon is reported with the command's wording.
	e.hosts["myhost"] = e.hosts["db"]
	e.back.syncErr = errors.New("service manager unavailable")
	code, r = c.post("/api/links/web/start", "Bearer secret")
	if code != http.StatusConflict || !strings.Contains(r.Error, "service manager unavailable") {
		t.Fatalf("start with a failing daemon: %d %+v", code, r)
	}
}

func TestUIActionsOneAtATime(t *testing.T) {
	e := newEnv(t)
	names := []string{"a", "b", "c", "d", "e"}
	for _, name := range names {
		e.mustGst("link", "add", name, "myhost")
	}
	c := newUIClient(t, e)
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, r := c.post("/api/links/"+name+"/stop", "Bearer secret"); code != http.StatusOK {
				t.Errorf("stop %s: %d %+v", name, code, r)
			}
		}()
	}
	wg.Wait()
	for _, name := range names {
		if !e.link(name).Stopped {
			t.Errorf("%s still runs", name)
		}
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
	e.mustGst("link", "add", "web", "myhost")
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
	if err != nil || res.StatusCode != http.StatusOK || len(s.Links) != 1 {
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
