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

// startUI starts gst ui on n, which must be on the network, and reads its
// address, with n's gst IP for its name: the containers do not resolve
// .gst.
func (n *node) startUI() *uiServer {
	n.t.Helper()
	ip := n.net().Address
	n.mustSh(`setsid gst ui --port 8370 </dev/null >~/ui.out 2>&1 &`)
	var url string
	eventually(n.t, 10*time.Second, "gst ui printed no address", func() bool {
		out, _ := n.sh(`cat ~/ui.out`)
		_, rest, ok := strings.Cut(out, "gst ui: ")
		url, _, _ = strings.Cut(rest, "\n")
		return ok && strings.Contains(url, "#")
	})
	base, token, _ := strings.Cut(url, "#")
	_, port, _ := strings.Cut(strings.TrimPrefix(base, "http://"), ":")
	return &uiServer{n, "http://" + ip + ":" + port, token}
}

// request sends an HTTP request to gst ui from machine from, over the
// network, with its token or without, and returns the status code and body.
func (u *uiServer) request(from *node, method, path string, withToken bool) (string, string) {
	u.n.t.Helper()
	args := []string{"http", method, u.base + path}
	if withToken {
		args = append(args, u.token)
	}
	out, err := from.helper(args...)
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
	client.installHelper()
	p.server.installHelper()
	client.waitNet(1)
	p.server.waitNet(1)
	ui := client.startUI()

	// gst ui serves on the gst IP only, to every machine of the network.
	if code, _ := ui.request(p.server, "GET", "api/state", true); code != "200" {
		t.Fatalf("state from the other machine: %s", code)
	}
	if _, err := client.helper("http", "GET", "http://127.0.0.1:8370/api/state", ui.token); err == nil {
		t.Fatal("gst ui answers on 127.0.0.1")
	}
	if code, _ := ui.request(client, "GET", "api/state", false); code != "401" {
		t.Fatalf("state without the token: %s", code)
	}
	code, body := ui.request(client, "GET", "api/state", true)
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
		if code, _ := ui.request(client, "POST", "api/links/srv/"+action, true); code == "200" {
			t.Fatalf("%s from gst ui: %s", action, code)
		}
	}
	if state := client.links()[p.destination()].State; state != wire.StateRunning {
		t.Fatalf("link is %s after the actions", state)
	}
}
