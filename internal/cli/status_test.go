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

// devboxStatus is the status of the machine devbox, with what every table can show.
func devboxStatus() wire.Status {
	peer := peerOf("bastion")
	return wire.Status{
		Software: "gst 0.4.0",
		Node:     wire.NodeInfo{ID: nodeID("bbbb2222"), Name: "devbox"},
		Links: []wire.LinkState{
			{Name: "bastion", Destination: "bastion.example.com", State: wire.StateRunning, Since: time.Now(), Peer: &peer},
			{Name: "flaky", Destination: "myhost", State: wire.StateDown, Error: "connection refused"},
			{Name: "off", Destination: "db", State: wire.StateConnecting},
		},
	}
}

func TestStatusOfAnotherMachine(t *testing.T) {
	e := newEnv(t)
	e.back.nodeStatus = func(node string) (wire.Status, error) { return devboxStatus(), nil }

	out := e.mustGst("status", "devbox")
	header, links, _ := strings.Cut(out, "\n\n")
	if want := "devbox (bbbb2222), gst 0.4.0"; header != want {
		t.Fatalf("header %q, want %q", header, want)
	}
	want := [][]string{
		{"LINK", "STATUS", "DESTINATION"},
		{"devbox->bastion", "running", "bastion.example.com"},
		{"devbox->flaky", "down", "(last", "error:", "connection", "refused)", "myhost"},
		{"devbox->off", "connecting", "db"},
	}
	if got := rows(links); !reflect.DeepEqual(got, want) {
		t.Errorf("links\n got %q\nwant %q", got, want)
	}

	// The same status as JSON, with the stable field names.
	got := decode[wire.Status](t, e.mustGst("status", "devbox", "--json"))
	if want := devboxStatus(); got.Node != want.Node || len(got.Links) != 3 {
		t.Fatalf("json = %+v", got)
	}
	contains(t, e.mustGst("status", "devbox", "--json"), `"destination": "bastion.example.com"`, `"state": "connecting"`)
}

func TestStatusOfAMachineWithNothingConfigured(t *testing.T) {
	e := newEnv(t)
	e.back.nodeStatus = func(string) (wire.Status, error) {
		return wire.Status{Software: "gst 0.4.0", Node: wire.NodeInfo{ID: nodeID("cccc3333"), Name: "far"}}, nil
	}
	want := "far (cccc3333), gst 0.4.0\n\nNo links.\n"
	if out := e.mustGst("status", "far"); out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	// The lists are empty in JSON too, not null, when the machine says so.
	e.back.nodeStatus = func(string) (wire.Status, error) {
		return wire.Status{Links: []wire.LinkState{}}, nil
	}
	contains(t, e.mustGst("status", "far", "--json"), `"links": []`)
}

func TestStatusMachineReference(t *testing.T) {
	e := newEnv(t)
	e.back.nodeStatus = func(string) (wire.Status, error) { return wire.Status{}, nil }
	for _, tc := range []struct{ arg, node string }{
		{"devbox", "devbox"},
		{"DevBox.GST", "devbox"},
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
	e.mustGst("add", "devbox")
	e.back.topology = meshTopology()
	e.resetCalls()

	out := e.mustGst("status")
	if want := []string{"nodes"}; !slices.Equal(e.back.calls, want) {
		t.Fatalf("calls %v, want %v; this machine is asked directly", e.back.calls, want)
	}
	// One line per machine, this one first, without a header or an edge table.
	want := []string{
		"aaaa1111 laptop gst 0.4.0 -",
		"bbbb2222 devbox gst 0.4.0 online",
		"cccc3333 far - online; via devbox",
		"dddd4444 dup (dddd4444) gst 0.4.0 online; via devbox",
		"eeee5555 dup (eeee5555) gst 0.4.0 offline",
	}
	if got := lines(out); !slices.Equal(got, want) {
		t.Fatalf("status\n got %q\nwant %q\noutput:\n%s", got, want, out)
	}
	if strings.Contains(out, "# Problems") || strings.Contains(out, "No links yet") {
		t.Fatalf("a healthy mesh reports no problems or missing links:\n%s", out)
	}
}

func TestStatusAllListsTheLinks(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "devbox")
	e.back.topology = meshTopology()

	got := lines(e.mustGst("status", "--all"))
	want := []string{"", "LINK", "devbox->dup (dddd4444) (d1)", "devbox->far", "laptop->devbox"}
	if len(got) != 5+len(want) || !slices.Equal(got[5:], want) {
		t.Fatalf("status --all\n got %q\nwant 5 machines and %q", got, want)
	}
	if code, _, stderr := e.gst("status", "--all", "devbox"); code != exitUsage || !strings.Contains(stderr, "--all") {
		t.Fatalf("--all with a machine: exit %d, stderr %q", code, stderr)
	}
}

func TestStatusMarksOfflineMachines(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	self, gw, far := nodeID("aaaa1111"), nodeID("bbbb2222"), nodeID("cccc3333")
	e.back.topology = wire.Topology{Self: self, Nodes: []wire.TopoNode{
		{ID: self, Name: "laptop", Software: "gst 0.4.0", Reachable: true, Path: []string{self}},
		{ID: gw, Name: "gw", Software: "gst 0.4.0"},
		{ID: far, Name: "far", Software: "gst 0.4.0", Reachable: true, Path: []string{self, gw, far}},
	}}
	want := []string{
		"aaaa1111 laptop gst 0.4.0 -",
		"bbbb2222 gw gst 0.4.0 offline",
		"cccc3333 far gst 0.4.0 online; via gw",
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

func TestStatusShowsUDPPaths(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	a, b, c := nodeID("aaaa1111"), nodeID("bbbb2222"), nodeID("cccc3333")
	e.back.topology = wire.Topology{Self: a, Nodes: []wire.TopoNode{
		{ID: a, Name: "a", Reachable: true, Path: []string{a}},
		{ID: b, Name: "b", Reachable: true, Path: []string{a, b}},
		{ID: c, Name: "c", Reachable: true, Direct: true, Path: []string{a, b, c}},
	}}
	want := []string{"aaaa1111 a - -", "bbbb2222 b - online", "cccc3333 c - online; direct"}
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
	want := []string{"aaaa1111 laptop - -", "", "No links yet. Link to another machine with: gst add <name> [destination]"}
	if got := lines(out); !slices.Equal(got, want) {
		t.Fatalf("status\n got %q\nwant %q", got, want)
	}

	// A link that has not found anything yet is not "no links yet".
	e.mustGst("add", "devbox")
	if out := e.mustGst("status"); strings.Contains(out, "No links yet") {
		t.Fatalf("a configured link was ignored:\n%s", out)
	}
}

func TestStatusListsProblems(t *testing.T) {
	e := newEnv(t)
	e.mustGst("name", "laptop")
	e.mustGst("add", "web", "myhost")
	e.mustGst("add", "db")
	e.mustGst("add", "cache", "devbox")
	e.back.topology = meshTopology()
	e.back.linkStates["web"] = wire.LinkState{Name: "web", State: wire.StateDown, Error: "SSH keepalive timed out"}
	e.back.linkStates["db"] = wire.LinkState{Name: "db", State: wire.StateConnecting}

	out := e.mustGst("status")
	machines, problems, found := strings.Cut(out, "\n# Problems:\n")
	if !found {
		t.Fatalf("no problems section:\n%s", out)
	}
	if n := len(lines(machines)); n != 5 {
		t.Errorf("%d lines before the problems, want 5 machines:\n%s", n, machines)
	}
	want := []string{
		"#   - link laptop->db is connecting",
		"#   - link laptop->web is down (last error: SSH keepalive timed out)",
	}
	if got := strings.Split(strings.TrimRight(problems, "\n"), "\n"); !slices.Equal(got, want) {
		t.Fatalf("problems\n got %q\nwant %q", got, want)
	}
	// The running link is no problem.
	if strings.Contains(problems, "cache") {
		t.Fatalf("problems list what works:\n%s", problems)
	}
}

func TestStatusJSON(t *testing.T) {
	e := newEnv(t)
	e.mustGst("name", "laptop")
	e.mustGst("add", "web", "myhost")
	e.back.topology = meshTopology()
	e.resetCalls()

	out := e.mustGst("status", "--json")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil || len(raw) != 2 {
		t.Fatalf("json = %q, %v; want the keys self and mesh", out, err)
	}
	got := decode[meshStatus](t, out)
	if got.Self.Node.Name != "laptop" || len(got.Self.Links) != 1 || got.Self.Links[0].Name != "web" {
		t.Fatalf("self = %+v", got.Self)
	}
	if !reflect.DeepEqual(got.Mesh, e.back.topology) {
		t.Fatalf("mesh = %+v\nwant %+v", got.Mesh, e.back.topology)
	}
	contains(t, out, `"self": {`, `"mesh": {`, `"reachable": true`, `"link": "devbox"`, `"name": "web"`)
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
	devbox := peerOf("devbox")

	out := e.mustGst("ping", "devbox", "-c", "1")
	if want := "pong from devbox (" + shortID(devbox.ID) + ") over SSH in 1.5 ms\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if want := []string{"ping devbox"}; !slices.Equal(e.back.calls, want) {
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

	// Machines are named by any of their names.
	e.resetCalls()
	e.mustGst("ping", "DevBox.GST", "-c", "1")
	e.mustGst("ping", "cccc3333", "-c", "1")
	if want := []string{"ping devbox", "ping cccc3333"}; !slices.Equal(e.back.calls, want) {
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
		{[]string{"ping", "devbox", "-c", "0"}, "error: -c must be at least 1\n"},
		{[]string{"ping", "devbox", "-c", "-2"}, "error: -c must be at least 1\n"},
		{[]string{"ping", "devbox", "-c", "x"}, "error: invalid argument"},
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
		return devboxStatus(), nil
	}
	c := newUIClient(t, e)

	for _, auth := range []string{"", "Bearer wrong"} {
		if res := c.do("GET", "/api/nodes/devbox", auth); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("with %q: %s", auth, res.Status)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("a request without the token reached the mesh: %v", asked)
	}

	res := c.do("GET", "/api/nodes/devbox", "Bearer secret")
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%s %q: %s", res.Status, res.Header.Get("Content-Type"), body)
	}
	if st := decode[wire.Status](t, string(body)); st.Node.Name != "devbox" || len(st.Links) != 3 {
		t.Fatalf("status = %+v", st)
	}
	if !slices.Equal(asked, []string{"devbox"}) {
		t.Fatalf("asked %v", asked)
	}

	res = c.do("GET", "/api/nodes/nowhere", "Bearer secret")
	body, _ = io.ReadAll(res.Body)
	if res.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), `no machine "nowhere"`) {
		t.Fatalf("error: %s %q", res.Status, body)
	}
	if res := c.do("POST", "/api/nodes/devbox", "Bearer secret"); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: %s", res.Status)
	}
}

func TestPingVerbose(t *testing.T) {
	e := newEnv(t)
	start := time.Now()
	event := func(after time.Duration, text string) wire.DirectEvent {
		return wire.DirectEvent{Time: start.Add(after), Text: text}
	}
	asked := 0
	e.back.ping = func(node string) (wire.Ping, error) {
		return wire.Ping{Node: peerOf(node), Via: []wire.NodeInfo{}, TimeMS: 1.5, Direct: true}, nil
	}
	e.back.direct = func(node string) (wire.DirectReport, error) {
		asked++
		return wire.DirectReport{
			Node:   peerOf(node),
			Trying: asked == 1, // A try still running is waited for.
			Path:   &wire.DirectPath{Local: "192.0.2.1:5000", Remote: "192.0.2.2:6000"},
			Local:  wire.DirectTrace{Events: []wire.DirectEvent{event(0, "offering"), event(1500*time.Millisecond, "up")}},
			Peer:   wire.DirectTrace{},
		}, nil
	}
	out := e.mustGst("ping", "devbox", "-c", "1", "-v")
	if asked != 2 {
		t.Fatalf("asked %d times, want 2", asked)
	}
	for _, want := range []string{
		"over SSH in 1.5 ms; packets go over UDP\n",
		"UDP path to devbox: up, 192.0.2.1:5000 -> 192.0.2.2:6000\n",
		"This machine:\n    0.000s  offering\n    1.500s  up\n",
		"devbox:\n  no try yet\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q lacks %q", out, want)
		}
	}

	// Without a path, it says when the next try is.
	e.back.direct = func(node string) (wire.DirectReport, error) {
		return wire.DirectReport{Node: peerOf(node), Next: time.Now().Add(time.Minute), PeerError: "no way"}, nil
	}
	out = e.mustGst("ping", "devbox", "-c", "1", "-v")
	for _, want := range []string{"UDP path to devbox: none, next try in 1m0s\n", "no answer: no way\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q lacks %q", out, want)
		}
	}

	var got verbosePing
	if err := json.Unmarshal([]byte(e.mustGst("ping", "devbox", "-c", "1", "-v", "--json")), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Pings) != 1 || got.Direct.Node.Name != "devbox" {
		t.Fatalf("--json: %+v", got)
	}
}
