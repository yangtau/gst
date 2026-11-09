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

// gst ui serves the machine's state to holders of the token it prints, and
// changes nothing.
func TestUI(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client := p.client
	client.mustGst("name", "cli")
	client.mustGst("link", p.destination())
	client.waitLink(p.destination(), "running")
	ui := client.startUI()

	if code, _ := ui.request("GET", "api/state", false); code != "401" {
		t.Fatalf("state without the token: %s", code)
	}
	code, body := ui.request("GET", "api/state", true)
	var state struct {
		Status   *wire.Status   `json:"status"`
		Topology *wire.Topology `json:"topology"`
	}
	if code != "200" || json.Unmarshal([]byte(body), &state) != nil {
		t.Fatalf("state: %s %q", code, body)
	}
	if state.Status == nil || len(state.Status.Links) != 1 || state.Status.Links[0].Destination != p.destination() || state.Status.Links[0].State != wire.StateRunning ||
		state.Topology == nil || len(state.Topology.Nodes) != 2 {
		t.Fatalf("gst ui shows %+v", state)
	}

	// The page cannot act on links: the actions it once had are gone.
	for _, action := range []string{"restart", "stop", "start"} {
		if code, _ := ui.request("POST", "api/links/srv/"+action, true); code == "200" {
			t.Fatalf("%s from gst ui: %s", action, code)
		}
	}
	if state := client.links()[p.destination()].State; state != wire.StateRunning {
		t.Fatalf("link is %s after the actions", state)
	}
}
