package cli

import (
	"encoding/json"
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
			{Name: "off", Destination: "db", State: wire.StateConnecting},
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
			{"off", "connecting", "db", "-"},
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
	contains(t, e.mustGst("status", "devcn", "--json"), `"destination": "bastion.example.com"`, `"state": "connecting"`, `"machine": "bastion"`, `"via": "far"`)
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
		e.expectError(exitFailure, args, `error: no machine "nowhere" in the mesh`, "hint: see the machines of the mesh with `gst status`\n")
	}
}

// lines splits output into its lines with runs of spaces collapsed.
func lines(out string) []string {
	var ls []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		ls = append(ls, strings.Join(strings.Fields(line), " "))
	}
	return ls
}

func TestStatusListsTheMachines(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "devcn")
	e.back.topology = meshTopology()
	e.resetCalls()

	out := e.mustGst("status")
	if want := []string{"nodes"}; !slices.Equal(e.back.calls, want) {
		t.Fatalf("calls %v, want %v; this machine is asked directly", e.back.calls, want)
	}
	// One line per machine, this one first, without a header or an edge table.
	want := []string{
		"aaaa1111 laptop gst 0.3.0 -",
		"bbbb2222 devcn gst 0.3.0 online; exit",
		"cccc3333 far - online; via devcn",
		"dddd4444 dup (dddd4444) gst 0.3.0 online; via devcn",
		"eeee5555 dup (eeee5555) gst 0.3.0 offline",
	}
	if got := lines(out); !slices.Equal(got, want) {
		t.Fatalf("status\n got %q\nwant %q\noutput:\n%s", got, want, out)
	}
	if strings.Contains(out, "# Problems") || strings.Contains(out, "No links yet") {
		t.Fatalf("a healthy mesh reports no problems or missing links:\n%s", out)
	}
}

func TestStatusExitMarksOfflineMachines(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	self, gw, far := nodeID("aaaa1111"), nodeID("bbbb2222"), nodeID("cccc3333")
	e.back.topology = wire.Topology{Self: self, Nodes: []wire.TopoNode{
		{ID: self, Name: "laptop", Software: "gst 0.3.0", Exit: true, Reachable: true, Path: []string{self}},
		{ID: gw, Name: "gw", Software: "gst 0.3.0", Exit: true},
		{ID: far, Name: "far", Software: "gst 0.3.0", Reachable: true, Path: []string{self, gw, far}},
	}}
	want := []string{
		"aaaa1111 laptop gst 0.3.0 -; exit",
		"bbbb2222 gw gst 0.3.0 offline; exit",
		"cccc3333 far gst 0.3.0 online; via gw",
	}
	if got := lines(e.mustGst("status")); !slices.Equal(got, want) {
		t.Fatalf("status\n got %q\nwant %q", got, want)
	}
}

func TestStatusViaListsEveryMachineOnTheWay(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	ids := []string{nodeID("aaaa1111"), nodeID("bbbb2222"), nodeID("cccc3333"), nodeID("dddd4444")}
	e.back.topology = wire.Topology{Self: ids[0], Nodes: []wire.TopoNode{
		{ID: ids[0], Name: "a", Reachable: true, Path: ids[:1]},
		{ID: ids[1], Name: "b", Reachable: true, Path: ids[:2]},
		{ID: ids[2], Name: "c", Reachable: true, Path: ids[:3]},
		{ID: ids[3], Name: "d", Reachable: true, Path: ids},
	}}
	want := []string{"aaaa1111 a - -", "bbbb2222 b - online", "cccc3333 c - online; via b", "dddd4444 d - online; via b, c"}
	if got := lines(e.mustGst("status")); !slices.Equal(got, want) {
		t.Fatalf("status\n got %q\nwant %q", got, want)
	}
}

func TestStatusWithoutLinks(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	self := nodeID("aaaa1111")
	e.back.topology = wire.Topology{Self: self, Nodes: []wire.TopoNode{{ID: self, Name: "laptop", Reachable: true, Path: []string{self}}}}
	out := e.mustGst("status")
	want := []string{"aaaa1111 laptop - -", "", "No links yet. Link to another machine with: gst link add <name> [destination]"}
	if got := lines(out); !slices.Equal(got, want) {
		t.Fatalf("status\n got %q\nwant %q", got, want)
	}

	// A link that has not found anything yet is not "no links yet".
	e.mustGst("link", "add", "devcn")
	if out := e.mustGst("status"); strings.Contains(out, "No links yet") {
		t.Fatalf("a configured link was ignored:\n%s", out)
	}
}

func TestStatusListsProblems(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "web", "myhost")
	e.mustGst("link", "add", "db")
	e.mustGst("link", "add", "cache", "devcn")
	e.mustGst("proxy", "1081")
	e.mustGst("map", "add", "15432", "devcn:5432")
	e.mustGst("map", "add", "15433", "devcn:5433")
	e.back.topology = meshTopology()
	e.back.linkStates["web"] = wire.LinkState{Name: "web", State: wire.StateDown, Error: "SSH keepalive timed out"}
	e.back.linkStates["db"] = wire.LinkState{Name: "db", State: wire.StateConnecting}
	e.back.listenErrors["127.0.0.1:1081"] = "address already in use"
	e.back.listenErrors["127.0.0.1:15432"] = "permission denied"

	out := e.mustGst("status")
	machines, problems, found := strings.Cut(out, "\n# Problems:\n")
	if !found {
		t.Fatalf("no problems section:\n%s", out)
	}
	if n := len(lines(machines)); n != 5 {
		t.Errorf("%d lines before the problems, want 5 machines:\n%s", n, machines)
	}
	want := []string{
		"#   - link db is connecting",
		"#   - link web is down (last error: SSH keepalive timed out)",
		"#   - proxy 127.0.0.1:1081 failed: address already in use",
		"#   - map 127.0.0.1:15432 failed: permission denied",
	}
	if got := strings.Split(strings.TrimRight(problems, "\n"), "\n"); !slices.Equal(got, want) {
		t.Fatalf("problems\n got %q\nwant %q", got, want)
	}
	// The running link and the map that listens are not problems.
	if strings.Contains(problems, "cache") || strings.Contains(problems, "15433") {
		t.Fatalf("problems list what works:\n%s", problems)
	}
}

func TestStatusJSON(t *testing.T) {
	e := newEnv(t)
	e.mustGst("name", "laptop")
	e.mustGst("link", "add", "web", "myhost")
	e.mustGst("map", "add", "15432", "devcn:5432")
	e.back.listenErrors["127.0.0.1:15432"] = "address in use"
	e.back.topology = meshTopology()
	e.resetCalls()

	out := e.mustGst("status", "--json")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil || len(raw) != 2 {
		t.Fatalf("json = %q, %v; want the keys self and mesh", out, err)
	}
	got := decode[meshStatus](t, out)
	if got.Self.Node.Name != "laptop" || len(got.Self.Links) != 1 || got.Self.Links[0].Name != "web" ||
		len(got.Self.Maps) != 1 || got.Self.Maps[0].Machine != "devcn" || got.Self.Maps[0].Error != "address in use" {
		t.Fatalf("self = %+v", got.Self)
	}
	if !reflect.DeepEqual(got.Mesh, e.back.topology) {
		t.Fatalf("mesh = %+v\nwant %+v", got.Mesh, e.back.topology)
	}
	contains(t, out, `"self": {`, `"mesh": {`, `"reachable": true`, `"link": "devcn"`, `"name": "web"`)
	if slices.Contains(e.back.calls, "node-status laptop") {
		t.Fatalf("calls %v; this machine is asked directly", e.back.calls)
	}
}

func TestStatusWithoutDaemon(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{{"status"}, {"status", "--json"}} {
		e.expectError(exitFailure, args, "error: the gst daemon does not answer\n", "hint: start it with `gst up`\n")
	}
	e.mustGst("up")
	e.back.nodesErr = errors.New("timed out")
	for _, args := range [][]string{{"status"}, {"status", "--json"}} {
		e.expectError(exitFailure, args, "error: the gst daemon does not answer: timed out\n", "hint: start it with `gst up`\n")
	}
}

// --- ping ---

func TestPing(t *testing.T) {
	e := newEnv(t)
	devcn := peerOf("devcn")

	out := e.mustGst("ping", "devcn", "-c", "1")
	if want := "pong from devcn (" + shortID(devcn.ID) + ") directly in 1.5 ms\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if want := []string{"ping devcn"}; !slices.Equal(e.back.calls, want) {
		t.Fatalf("calls %v, want %v", e.back.calls, want)
	}

	// Machines on the way are named in order.
	e.back.ping = func(node string) (wire.Ping, error) {
		return wire.Ping{Node: peerOf(node), Via: []wire.NodeInfo{peerOf("a"), peerOf("b")}, TimeMS: 12.345}, nil
	}
	out = e.mustGst("ping", "far", "--count", "1")
	if want := "pong from far (" + shortID(peerOf("far").ID) + ") via a, b in 12.3 ms\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}

	// Machines are named like for gst connect.
	e.resetCalls()
	e.mustGst("ping", "DevCN.GST", "-c", "1")
	e.mustGst("ping", "cccc3333", "-c", "1")
	if want := []string{"ping devcn", "ping cccc3333"}; !slices.Equal(e.back.calls, want) {
		t.Fatalf("calls %v, want %v", e.back.calls, want)
	}
}

func TestPingJSON(t *testing.T) {
	e := newEnv(t)
	e.back.ping = func(node string) (wire.Ping, error) {
		return wire.Ping{Node: peerOf(node), Via: []wire.NodeInfo{peerOf("gw")}, TimeMS: 4.25}, nil
	}
	out := e.mustGst("ping", "far", "-c", "1", "--json")
	got := decode[[]wire.Ping](t, out)
	if len(got) != 1 || got[0].Node != peerOf("far") || len(got[0].Via) != 1 || got[0].Via[0] != peerOf("gw") || got[0].TimeMS != 4.25 {
		t.Fatalf("json = %+v", got)
	}
	contains(t, out, `"node": {`, `"via": [`, `"time_ms": 4.25`)
	if strings.Contains(out, "pong") {
		t.Fatalf("--json printed text:\n%s", out)
	}

	// With no answer, the list is empty and the command fails.
	e.back.ping = func(string) (wire.Ping, error) { return wire.Ping{}, errors.New("timed out") }
	code, stdout, stderr := e.gst("ping", "far", "-c", "1", "--json")
	if code != exitFailure || strings.TrimSpace(stdout) != "[]" || !strings.Contains(stderr, "error: far did not answer: timed out\n") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestPingNoAnswer(t *testing.T) {
	e := newEnv(t)
	e.back.ping = func(node string) (wire.Ping, error) {
		return wire.Ping{}, errors.New(`no machine "` + node + `" in the mesh; see gst status`)
	}
	code, stdout, stderr := e.gst("ping", "nowhere", "-c", "1")
	if code != exitFailure {
		t.Fatalf("code %d", code)
	}
	if want := "no answer: no machine \"nowhere\" in the mesh; see gst status\n"; stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
	}
	if want := "error: nowhere did not answer: no machine \"nowhere\" in the mesh; see gst status\nhint: see the machines of the mesh with `gst status`\n"; stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
}

func TestPingRejects(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"ping"}, "error: missing argument <machine>\n"},
		{[]string{"ping", "a", "b"}, `error: unexpected argument "b"`},
		{[]string{"ping", "Bad_Name"}, "error: invalid machine"},
		{[]string{"ping", "dev cn"}, "error: invalid machine"},
		{[]string{"ping", "devcn", "-c", "0"}, "error: -c must be at least 1\n"},
		{[]string{"ping", "devcn", "-c", "-2"}, "error: -c must be at least 1\n"},
		{[]string{"ping", "devcn", "-c", "x"}, "error: invalid argument"},
	} {
		e.resetCalls()
		stderr := e.expectCode(exitUsage, tc.args...)
		if !strings.Contains(stderr, tc.want) || !strings.Contains(stderr, "usage: gst ping <machine>") {
			t.Errorf("gst %v: stderr %q", tc.args, stderr)
		}
		if len(e.back.calls) != 0 {
			t.Errorf("gst %v reached the daemon: %v", tc.args, e.back.calls)
		}
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
