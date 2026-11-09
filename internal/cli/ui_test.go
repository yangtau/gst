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
	srv := httptest.NewServer(e.app(io.Discard, io.Discard).uiHandler("secret", "10.64.0.7"))
	t.Cleanup(srv.Close)
	return &uiClient{t, srv}
}

func (c *uiClient) do(method, path, auth string) *http.Response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.srv.URL+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Host = "laptop.gst:8370"
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
	e.mustGst("link", "myhost")
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
	e.mustGst("link", "myhost")
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
	if e.link("myhost") == nil || len(e.back.calls) != 0 {
		t.Fatalf("the page changed something: link %+v, calls %v", e.link("myhost"), e.back.calls)
	}
}

func TestUIRejectsOtherHosts(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "myhost")
	h := e.app(io.Discard, io.Discard).uiHandler("secret", "10.64.0.7")
	for host, want := range map[string]int{
		"10.64.0.7:8370":                  http.StatusOK,
		"10.64.0.7":                       http.StatusOK,
		"laptop.gst:8370":                 http.StatusOK,
		"Laptop.GST":                      http.StatusOK,
		"devbox.gst":                      http.StatusOK,
		"127.0.0.1:8370":                  http.StatusForbidden,
		"localhost:8370":                  http.StatusForbidden,
		"[::1]:8370":                      http.StatusForbidden,
		"evil.example.com":                http.StatusForbidden,
		"evil.example:80":                 http.StatusForbidden,
		"10.0.0.1:8370":                   http.StatusForbidden,
		"0.0.0.0:8370":                    http.StatusForbidden,
		"evil.example.gst:8370":           http.StatusForbidden,
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
	e.mustGst("link", "myhost")
	e.mustGst("link", "db")
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
	if len(st.Links) != 2 || st.Links[0].Destination != "myhost" || st.Links[0].State != wire.StateRunning ||
		st.Links[1].Destination != "db" || st.Links[1].State != wire.StateRunning || st.Links[1].Peer == nil || st.Links[1].Peer.Name != "db" {
		t.Fatalf("links %+v", st.Links)
	}
	if s.Topology == nil || s.Topology.Self != nodeID("aaaa1111") || len(s.Topology.Nodes) != 5 || len(s.Topology.Edges) != 3 {
		t.Fatalf("topology %+v", s.Topology)
	}

	// The state follows the configuration and the daemon at each request.
	e.back.linkStates["db"] = wire.LinkState{Destination: "db", State: wire.StateDown, Error: "SSH connection closed"}
	s = c.state()
	if db := s.Status.Links[1]; db.State != wire.StateDown || db.Error != "SSH connection closed" || db.Peer != nil {
		t.Fatalf("db %+v", db)
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
	e.mustGst("link", "myhost")
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
	if s.Status == nil || s.Topology != nil || !slices.Equal(s.Errors, []string{"machines: timed out"}) {
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

func TestUIHost(t *testing.T) {
	for host, want := range map[string]bool{
		"10.64.0.7":       true,
		"10.64.0.7:8370":  true,
		"devbox.gst":      true,
		"devbox.gst:8370": true,
		"DevBox.GST:80":   true,
		"10.64.0.8":       false,
		"a.b.gst":         false,
		".gst":            false,
		"localhost":       false,
		"localhost:80":    false,
		"127.0.0.1:8370":  false,
		"[::1]:8370":      false,
		"example.com":     false,
		"example.com:80":  false,
		"[2001:db8::1]:1": false,
		"":                false,
	} {
		if got := uiHost(host, "10.64.0.7"); got != want {
			t.Errorf("uiHost(%q) = %v, want %v", host, got, want)
		}
	}
	if uiHost("", "") {
		t.Error("an empty Host matches an empty gst IP")
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

// onNet puts the environment's machine on a gst network whose address is
// 127.0.0.1, where a test can reach it.
func onNet(e *testEnv) {
	e.mustGst("name", "laptop")
	e.mustGst("up")
	e.back.net = &wire.NetState{Subnet: "127.0.0.0/8", Address: "127.0.0.1", Interface: "utun9", Peers: []wire.NetPeer{}}
}

func TestUICommandServes(t *testing.T) {
	e := newEnv(t)
	onNet(e)
	e.mustGst("link", "myhost")
	var stdout, stderr syncBuffer
	a := e.app(&stdout, &stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- a.execute(ctx, []string{"--config", e.config, "ui"}) }()

	urlPattern := regexp.MustCompile(`gst ui: http://laptop\.gst:(\d+)/#([0-9A-Za-z]+)\n`)
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

	// The gst IP is 127.0.0.1 here; a browser asks for laptop.gst.
	req, _ := http.NewRequest("GET", "http://127.0.0.1:"+m[1]+"/api/state", nil)
	req.Host = "laptop.gst:" + m[1]
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
}

func TestUICommandErrors(t *testing.T) {
	e := newEnv(t)
	e.expectError(exitUsage, []string{"ui", "--port", "70000"}, "error: --port must be between 0 and 65535\n", "usage: gst ui")
	e.expectError(exitUsage, []string{"ui", "--port", "-1"}, "error: --port must be between 0 and 65535\n")
	e.expectError(exitUsage, []string{"ui", "extra"}, `error: unexpected argument "extra"`)

	// gst ui serves on the gst network only.
	e.expectError(exitFailure, []string{"ui"}, "error: gst is not running on this machine\n", "hint: start it with `gst up`\n")
	e.mustGst("up")
	e.back.net = &wire.NetState{Error: "the network helper is not running", Hint: "run `gst up` on this machine"}
	e.expectError(exitFailure, []string{"ui"}, "error: this machine is not on the gst network: the network helper is not running\n", "hint: run `gst up` on this machine\n")
	onNet(e)

	// A port in use is a failure with a hint.
	var stdout syncBuffer
	a := e.app(&stdout, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- a.execute(ctx, []string{"--config", e.config, "ui"}) }()
	m := regexp.MustCompile(`laptop\.gst:(\d+)`)
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
	e.expectError(exitFailure, []string{"ui", "--port", port}, "error: cannot listen on port "+port+" of the gst IP", "hint: choose another port with --port, or 0 for any free one\n")
	cancel()
	<-done
}
