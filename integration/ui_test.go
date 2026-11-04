//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// uiServer is gst ui running on a node.
type uiServer struct {
	n           *node
	base, token string
}

// startUI starts gst ui on n and reads its address.
func (n *node) startUI() *uiServer {
	n.t.Helper()
	n.mustSh(`setsid gst ui --no-open --port 8370 </dev/null >~/ui.out 2>&1 &`)
	var url string
	eventually(n.t, 10*time.Second, "gst ui printed no address", func() bool {
		out, _ := n.sh(`cat ~/ui.out`)
		_, rest, ok := strings.Cut(out, "gst ui: ")
		url, _, _ = strings.Cut(rest, "\n")
		return ok && strings.Contains(url, "#")
	})
	base, token, _ := strings.Cut(url, "#")
	return &uiServer{n, base, token}
}

// request sends an HTTP request to gst ui, with its token or without, and
// returns the status code and body.
func (u *uiServer) request(method, path string, withToken bool) (string, string) {
	u.n.t.Helper()
	args := []string{"http", method, u.base + path}
	if withToken {
		args = append(args, u.token)
	}
	out, err := u.n.helper(args...)
	if err != nil {
		u.n.t.Fatalf("gst ui %s %s: %v", method, path, err)
	}
	code, body, _ := strings.Cut(out, "\n")
	return code, body
}

// gst ui serves the machine's state and link actions to holders of the
// token it prints.
func TestUI(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client := p.client
	client.mustGst("name", "cli")
	client.mustGst(append([]string{"link", "add", "srv"}, p.flags()...)...)
	client.waitLink("srv", "running")
	ui := client.startUI()

	if code, _ := ui.request("GET", "api/state", false); code != "401" {
		t.Fatalf("state without the token: %s", code)
	}
	code, body := ui.request("GET", "api/state", true)
	var state struct {
		Daemon   bool           `json:"daemon"`
		Links    []linkInfo     `json:"links"`
		Topology *wire.Topology `json:"topology"`
	}
	if code != "200" || json.Unmarshal([]byte(body), &state) != nil {
		t.Fatalf("state: %s %q", code, body)
	}
	if !state.Daemon || len(state.Links) != 1 || state.Links[0].Name != "srv" || state.Links[0].Status != "running" || state.Topology == nil || len(state.Topology.Nodes) != 2 {
		t.Fatalf("gst ui shows %+v", state)
	}

	// Actions are the link commands.
	if code, _ := ui.request("POST", "api/links/srv/restart", false); code != "401" {
		t.Fatalf("restart without the token: %s", code)
	}
	if code, body := ui.request("POST", "api/links/srv/restart", true); code != "200" || !strings.Contains(body, "Restarted link srv") {
		t.Fatalf("restart from gst ui: %s %q", code, body)
	}
	client.waitLink("srv", "running")
	if code, body := ui.request("POST", "api/links/srv/stop", true); code != "200" || !strings.Contains(body, "Stopped link srv") {
		t.Fatalf("stop from gst ui: %s %q", code, body)
	}
	if status := client.links()["srv"].Status; status != "stopped" {
		t.Fatalf("link is %s after stop from gst ui", status)
	}
	if code, body := ui.request("POST", "api/links/srv/start", true); code != "200" || !strings.Contains(body, "Started link srv") {
		t.Fatalf("start from gst ui: %s %q", code, body)
	}
	client.waitLink("srv", "running")
	if code, _ := ui.request("POST", "api/links/nowhere/restart", true); code != "409" {
		t.Fatalf("restart of an unknown link: %s", code)
	}
}
