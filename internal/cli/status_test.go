package cli

import (
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// devcnStatus is the status of the machine devcn, with what every table can show.
func devcnStatus() wire.Status {
	peer := peerOf("bastion")
	return wire.Status{
		Software: "gst 0.3.0",
		Node:     wire.NodeInfo{ID: nodeID("bbbb2222"), Name: "devcn", Exit: true},
		Proxy:    wire.Listener{Listen: "127.0.0.1:1080"},
		Links: []wire.LinkState{
			{Name: "bastion", Destination: "bastion.example.com", State: wire.StateRunning, Since: time.Now(), Peer: &peer},
			{Name: "flaky", Destination: "myhost", State: wire.StateDown, Error: "connection refused"},
			{Name: "off", Destination: "db", State: wire.StateStopped},
		},
		Maps: []wire.MapState{
			{Listener: wire.Listener{Listen: "127.0.0.1:15432"}, Machine: "bastion", Target: "localhost:5432"},
			{Listener: wire.Listener{Listen: "/run/app.sock", Error: "socket in use"}, Machine: "far", Target: "/run/x.sock"},
		},
		Routes: []wire.Route{{Match: "example.com", Via: "bastion"}, {Match: "default", Via: "far"}},
	}
}

func TestStatusOfAnotherMachine(t *testing.T) {
	e := newEnv(t)
	e.back.nodeStatus = func(node string) (wire.Status, error) { return devcnStatus(), nil }

	out := e.mustGst("status", "devcn")
	header, rest, _ := strings.Cut(out, "\n")
	if want := "devcn (bbbb2222), gst 0.3.0, an exit"; header != want {
		t.Fatalf("header %q, want %q", header, want)
	}
	proxy, tables, _ := strings.Cut(rest, "\n\n")
	if proxy != "Proxy: 127.0.0.1:1080 (listening)" {
		t.Fatalf("proxy line %q", proxy)
	}
	links, tables, _ := strings.Cut(tables, "\n\n")
	maps, routes, _ := strings.Cut(tables, "\n\n")
	for _, tt := range []struct {
		name string
		got  string
		want [][]string
	}{
		{"links", links, [][]string{
			{"LINK", "STATUS", "DESTINATION", "PEER"},
			{"bastion", "running", "bastion.example.com", "bastion", "(" + shortID(peerOf("bastion").ID) + ")"},
			{"flaky", "down", "(last", "error:", "connection", "refused)", "myhost", "-"},
			{"off", "stopped", "db", "-"},
		}},
		{"maps", maps, [][]string{
			{"MAP", "TARGET", "STATUS"},
			{"127.0.0.1:15432", "localhost:5432", "on", "bastion", "listening"},
			{"/run/app.sock", "/run/x.sock", "on", "far", "failed:", "socket", "in", "use"},
		}},
		{"routes", routes, [][]string{
			{"ROUTE", "VIA"},
			{"example.com", "bastion"},
			{"default", "far"},
		}},
	} {
		if got := rows(tt.got); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s\n got %q\nwant %q", tt.name, got, tt.want)
		}
	}

	// The same status as JSON, with the stable field names.
	got := decode[wire.Status](t, e.mustGst("status", "devcn", "--json"))
	if want := devcnStatus(); got.Node != want.Node || !reflect.DeepEqual(got.Maps, want.Maps) || !reflect.DeepEqual(got.Routes, want.Routes) || len(got.Links) != 3 {
		t.Fatalf("json = %+v", got)
	}
	contains(t, e.mustGst("status", "devcn", "--json"), `"destination": "bastion.example.com"`, `"state": "stopped"`, `"machine": "bastion"`, `"via": "far"`)
}

func TestStatusOfAMachineWithNothingConfigured(t *testing.T) {
	e := newEnv(t)
	e.back.nodeStatus = func(string) (wire.Status, error) {
		return wire.Status{Software: "gst 0.3.0", Node: wire.NodeInfo{ID: nodeID("cccc3333"), Name: "far"}}, nil
	}
	want := "far (cccc3333), gst 0.3.0, not an exit\nProxy: off\n\nNo links.\n\nNo maps.\n\nNo routes.\n"
	if out := e.mustGst("status", "far"); out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	// The lists are empty in JSON too, not null, when the machine says so.
	e.back.nodeStatus = func(string) (wire.Status, error) {
		return wire.Status{Links: []wire.LinkState{}, Maps: []wire.MapState{}, Routes: []wire.Route{}}, nil
	}
	contains(t, e.mustGst("status", "far", "--json"), `"links": []`, `"maps": []`, `"routes": []`)
}

func TestStatusMachineReference(t *testing.T) {
	e := newEnv(t)
	e.back.nodeStatus = func(string) (wire.Status, error) { return wire.Status{}, nil }
	for _, tc := range []struct{ arg, node string }{
		{"devcn", "devcn"},
		{"DevCN.GST", "devcn"},
		{"cccc3333", "cccc3333"},
	} {
		e.resetCalls()
		e.mustGst("status", tc.arg)
		if want := []string{"node-status " + tc.node}; !slices.Equal(e.back.calls, want) {
			t.Errorf("gst status %s: calls %v, want %v", tc.arg, e.back.calls, want)
		}
	}
	for _, args := range [][]string{{"status", "Bad_Name"}, {"status", "dev cn"}, {"status", "a", "b"}} {
		e.resetCalls()
		if stderr := e.expectCode(exitUsage, args...); !strings.Contains(stderr, "usage: gst status [machine]") {
			t.Errorf("gst %v: %q", args, stderr)
		}
		if len(e.back.calls) != 0 {
			t.Errorf("gst %v reached the daemon: %v", args, e.back.calls)
		}
	}
}

func TestStatusOfAnotherMachineErrors(t *testing.T) {
	e := newEnv(t)
	e.back.nodeStatus = func(string) (wire.Status, error) {
		return wire.Status{}, errors.New(`no machine "nowhere" in the mesh`)
	}
	for _, args := range [][]string{{"status", "nowhere"}, {"status", "nowhere", "--json"}} {
		e.expectError(exitFailure, args, `error: no machine "nowhere" in the mesh`, "hint: see the machines and the way to them with `gst nodes`\n")
	}
}

func TestStatusOfThisMachine(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "web", "myhost")
	e.mustGst("map", "add", "15432", "devcn:5432")
	e.back.listenErrors["127.0.0.1:15432"] = "address in use"
	e.mustGst("route", "add", "example.com", "devcn")
	e.mustGst("name", "laptop")
	e.resetCalls()

	out := e.mustGst("status")
	if len(e.back.calls) != 0 {
		t.Fatalf("calls %v; this machine is asked directly", e.back.calls)
	}
	header, _, _ := strings.Cut(out, "\n")
	if !strings.HasPrefix(header, "laptop (") || !strings.HasSuffix(header, "), "+software+", not an exit") {
		t.Fatalf("header %q", header)
	}
	contains(t, out, "Proxy: 127.0.0.1:1080 (listening)\n", "LINK", "web", "running", "MAP", "127.0.0.1:15432", "localhost:5432 on devcn", "failed: address in use")

	st := decode[wire.Status](t, e.mustGst("status", "--json"))
	if st.Node.Name != "laptop" || len(st.Links) != 1 || st.Links[0].Name != "web" || len(st.Maps) != 1 || st.Maps[0].Machine != "devcn" || st.Maps[0].Error != "address in use" {
		t.Fatalf("json = %+v", st)
	}

	e.mustGst("proxy", "off")
	contains(t, e.mustGst("status"), "Proxy: off\n")
}

func TestStatusWithoutDaemon(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{{"status"}, {"status", "--json"}} {
		e.expectError(exitFailure, args, "error: the gst daemon does not answer\n", "hint: start it with `gst up`\n")
	}
}

func TestUINodeStatus(t *testing.T) {
	e := newEnv(t)
	var asked []string
	e.back.nodeStatus = func(node string) (wire.Status, error) {
		asked = append(asked, node)
		if node == "nowhere" {
			return wire.Status{}, errors.New(`no machine "nowhere" in the mesh`)
		}
		return devcnStatus(), nil
	}
	c := newUIClient(t, e)

	for _, auth := range []string{"", "Bearer wrong"} {
		if res := c.do("GET", "/api/nodes/devcn", auth); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("with %q: %s", auth, res.Status)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("a request without the token reached the mesh: %v", asked)
	}

	res := c.do("GET", "/api/nodes/devcn", "Bearer secret")
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%s %q: %s", res.Status, res.Header.Get("Content-Type"), body)
	}
	if st := decode[wire.Status](t, string(body)); st.Node.Name != "devcn" || len(st.Links) != 3 || len(st.Maps) != 2 || len(st.Routes) != 2 || st.Proxy.Listen != "127.0.0.1:1080" {
		t.Fatalf("status = %+v", st)
	}
	if !slices.Equal(asked, []string{"devcn"}) {
		t.Fatalf("asked %v", asked)
	}

	res = c.do("GET", "/api/nodes/nowhere", "Bearer secret")
	body, _ = io.ReadAll(res.Body)
	if res.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), `no machine "nowhere"`) {
		t.Fatalf("error: %s %q", res.Status, body)
	}
	if res := c.do("POST", "/api/nodes/devcn", "Bearer secret"); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: %s", res.Status)
	}
}
