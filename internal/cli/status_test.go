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

// devboxStatus is the status of the machine devbox, with what every table can show.
func devboxStatus() wire.Status {
	peer := peerOf("bastion")
	return wire.Status{
		Software: "gst 0.4.0",
		Node:     wire.NodeInfo{ID: nodeID("bbbb2222"), Name: "devbox"},
		Links: []wire.LinkState{
			{Destination: "bastion.example.com", State: wire.StateRunning, Since: time.Now(), Peer: &peer},
			{Destination: "myhost", State: wire.StateDown, Error: "connection refused"},
			{Destination: "db", State: wire.StateConnecting},
		},
	}
}

func TestStatusOfAnotherNode(t *testing.T) {
	e := newEnv(t)
	e.back.nodeStatus = func(node string) (wire.Status, error) { return devboxStatus(), nil }
	e.mustGst("up")
	e.back.topology = meshTopology()

	out := e.mustGst("status", "devbox")
	header, links, _ := strings.Cut(out, "\n\n")
	if want := "devbox (bbbb2222), gst 0.4.0"; header != want {
		t.Fatalf("header %q, want %q", header, want)
	}
	want := [][]string{
		{"LINK", "DESTINATION", "STATE"},
		{"devbox", "->", "bastion", "bastion.example.com", "running"},
		{"devbox", "->", "myhost", "myhost", "down", "(last", "error:", "connection", "refused)"},
		{"devbox", "->", "db", "db", "connecting"},
		{"laptop", "->", "devbox", "-", "running"},
	}
	if got := rows(links); !reflect.DeepEqual(got, want) {
		t.Errorf("links\n got %q\nwant %q", got, want)
	}
}

func TestStatusOfANodeWithNothingConfigured(t *testing.T) {
	e := newEnv(t)
	e.back.nodeStatus = func(string) (wire.Status, error) {
		return wire.Status{Software: "gst 0.4.0", Node: wire.NodeInfo{ID: nodeID("cccc3333"), Name: "far"}}, nil
	}
	want := "far (cccc3333), gst 0.4.0\n\nNo links.\n"
	if out := e.mustGst("status", "far"); out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
}

func TestStatusNodeReference(t *testing.T) {
	e := newEnv(t)
	e.back.nodeStatus = func(string) (wire.Status, error) { return wire.Status{}, nil }
	for _, tc := range []struct{ arg, node string }{
		{"devbox", "devbox"},
		{"DevBox.GST", "devbox"},
		{"cccc3333", "cccc3333"},
	} {
		e.resetCalls()
		e.mustGst("status", tc.arg)
		if want := []string{"node-status " + tc.node, "nodes"}; !slices.Equal(e.back.calls, want) {
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

func TestStatusOfAnotherNodeErrors(t *testing.T) {
	e := newEnv(t)
	e.back.nodeStatus = func(string) (wire.Status, error) {
		return wire.Status{}, errors.New(`no machine "nowhere" in the network`)
	}
	e.expectError(exitFailure, []string{"status", "nowhere"}, `error: no machine "nowhere" in the network`, "hint: see the machines with `gst status`\n")
}

// lines splits output into its lines with runs of spaces collapsed.
func lines(out string) []string {
	var ls []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		ls = append(ls, strings.Join(strings.Fields(line), " "))
	}
	return ls
}

func TestStatusListsTheNodes(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "devbox")
	e.back.topology = meshTopology()
	e.resetCalls()

	out := e.mustGst("status")
	if want := []string{"nodes"}; !slices.Equal(e.back.calls, want) {
		t.Fatalf("calls %v, want %v; this machine is asked directly", e.back.calls, want)
	}
	// One line per machine, this one first, without a header or a table of links.
	want := []string{
		"aaaa1111 laptop gst 0.4.0 -",
		"bbbb2222 devbox gst 0.4.0 relay",
		"cccc3333 far - relay via devbox",
		"dddd4444 dup (dddd4444) gst 0.4.0 relay via devbox",
		"eeee5555 dup (eeee5555) gst 0.4.0 relay via devbox",
	}
	if got := lines(out); !slices.Equal(got, want) {
		t.Fatalf("status\n got %q\nwant %q\noutput:\n%s", got, want, out)
	}
	if strings.Contains(out, "Problems") || strings.Contains(out, "No links yet") {
		t.Fatalf("a healthy network reports no problems or missing links:\n%s", out)
	}
}

func TestStatusVerboseListsTheLinks(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "devbox")
	e.back.topology = meshTopology()

	e.back.linkStates["devbox"] = wire.LinkState{Destination: "devbox", State: wire.StateDown, Error: "connection refused"}
	e.mustGst("add", "db")

	// This machine's links come first, with their destinations and states,
	// down ones too; the others' running links follow.
	got := lines(e.mustGst("status", "-v"))
	want := []string{
		"",
		"LINK DESTINATION STATE",
		"laptop -> devbox devbox down (last error: connection refused)",
		"laptop -> db db running",
		"devbox -> dup (dddd4444) - running",
		"devbox -> far - running",
	}
	if len(got) < 5+len(want) || !slices.Equal(got[5:5+len(want)], want) {
		t.Fatalf("status -v\n got %q\nwant 5 machines and %q", got, want)
	}
	if code, _, stderr := e.gst("status", "-v", "devbox"); code != exitUsage || !strings.Contains(stderr, "-v") {
		t.Fatalf("-v with a machine: exit %d, stderr %q", code, stderr)
	}
}

func TestStatusViaListsEveryNodeOnTheWay(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	ids := []string{nodeID("aaaa1111"), nodeID("bbbb2222"), nodeID("cccc3333"), nodeID("dddd4444")}
	e.back.topology = wire.Topology{Self: ids[0], Nodes: []wire.TopoNode{
		{ID: ids[0], Name: "a", Reachable: true, Route: ids[:1]},
		{ID: ids[1], Name: "b", Reachable: true, Route: ids[:2]},
		{ID: ids[2], Name: "c", Reachable: true, Route: ids[:3]},
		{ID: ids[3], Name: "d", Reachable: true, Route: ids},
	}}
	want := []string{"aaaa1111 a - -", "bbbb2222 b - relay", "cccc3333 c - relay via b", "dddd4444 d - relay via b, c"}
	if got := lines(e.mustGst("status")); !slices.Equal(got, want) {
		t.Fatalf("status\n got %q\nwant %q", got, want)
	}
}

func TestStatusShowsUDPPaths(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	a, b, c := nodeID("aaaa1111"), nodeID("bbbb2222"), nodeID("cccc3333")
	e.back.topology = wire.Topology{Self: a, Nodes: []wire.TopoNode{
		{ID: a, Name: "a", Reachable: true, Route: []string{a}},
		{ID: b, Name: "b", Reachable: true, Route: []string{a, b}},
		{ID: c, Name: "c", Reachable: true, Direct: true, Route: []string{a, b, c}},
	}}
	want := []string{"aaaa1111 a - -", "bbbb2222 b - relay", "cccc3333 c - direct"}
	if got := lines(e.mustGst("status")); !slices.Equal(got, want) {
		t.Fatalf("status\n got %q\nwant %q", got, want)
	}
}

func TestStatusWithoutLinks(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	self := nodeID("aaaa1111")
	e.back.topology = wire.Topology{Self: self, Nodes: []wire.TopoNode{{ID: self, Name: "laptop", Reachable: true, Route: []string{self}}}}
	out := e.mustGst("status")
	want := []string{"aaaa1111 laptop - -", "", "No links yet. Add another machine with `gst add <destination>`."}
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
	e.mustGst("add", "myhost")
	e.mustGst("add", "db")
	e.mustGst("add", "devbox")
	e.back.topology = meshTopology()
	e.back.linkStates["myhost"] = wire.LinkState{Destination: "myhost", State: wire.StateDown, Error: "SSH keepalive timed out"}
	e.back.linkStates["db"] = wire.LinkState{Destination: "db", State: wire.StateConnecting}

	out := e.mustGst("status")
	nodeLines, problems, found := strings.Cut(out, "\nProblems:\n")
	if !found {
		t.Fatalf("no problems section:\n%s", out)
	}
	if n := len(lines(nodeLines)); n != 5 {
		t.Errorf("%d lines before the problems, want 5 machines:\n%s", n, nodeLines)
	}
	want := []string{
		"  - link to myhost: down (last error: SSH keepalive timed out)",
		"  - link to db: connecting",
	}
	if got := strings.Split(strings.TrimRight(problems, "\n"), "\n"); !slices.Equal(got, want) {
		t.Fatalf("problems\n got %q\nwant %q", got, want)
	}
	// The running link is no problem.
	if strings.Contains(problems, "devbox") {
		t.Fatalf("problems list what works:\n%s", problems)
	}
}

func TestStatusWithoutDaemon(t *testing.T) {
	e := newEnv(t)
	e.expectError(exitFailure, []string{"status"}, "error: gst is not running on this machine\n", "hint: start it with `gst up`\n")
	e.mustGst("up")
	e.back.nodesErr = errors.New("timed out")
	e.expectError(exitFailure, []string{"status"}, "error: gst is not running on this machine: timed out\n", "hint: start it with `gst up`\n")
}

// --- ping ---

func TestPing(t *testing.T) {
	e := newEnv(t)

	out := e.mustGst("ping", "devbox", "-c", "1")
	if want := "reply from devbox (10.99.3.4): direct 192.0.2.2:6000, 1.5 ms\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if want := []string{"ping devbox"}; !slices.Equal(e.back.calls, want) {
		t.Fatalf("calls %v, want %v", e.back.calls, want)
	}

	// Machines on the way are named in order.
	e.back.ping = func(node string) (wire.NetPing, error) {
		return wire.NetPing{Node: peerOf(node), Address: "10.99.5.6", Way: wire.WayRelay, Via: []wire.NodeInfo{peerOf("a"), peerOf("b")}, TimeMS: 12.345}, nil
	}
	out = e.mustGst("ping", "far", "--count", "1")
	if want := "reply from far (10.99.5.6): relay via a, b, 12.3 ms\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}

	// This machine has no way to name.
	e.back.ping = func(node string) (wire.NetPing, error) {
		return wire.NetPing{Node: peerOf(node), Address: "10.99.1.2", TimeMS: 0.1}, nil
	}
	if out, want := e.mustGst("ping", "laptop", "-c", "1"), "reply from laptop (10.99.1.2): 0.1 ms\n"; out != want {
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

func TestPingSizeAndSummary(t *testing.T) {
	e := newEnv(t)
	calls := 0
	e.back.ping = func(node string) (wire.NetPing, error) {
		calls++
		if calls == 2 {
			return wire.NetPing{}, errors.New("timed out")
		}
		return wire.NetPing{Node: peerOf(node), Address: "10.99.5.6", TimeMS: float64(calls)}, nil
	}
	out := e.mustGst("ping", "far", "-c", "3", "-s", "1140")
	if e.back.pingSize != 1140 {
		t.Fatalf("size %d", e.back.pingSize)
	}
	contains(t, out, "3 sent, 2 received, 33% lost; 1.0/2.0/3.0 ms min/avg/max\n")
	for _, size := range []string{"15", "65001"} {
		if code, _, _ := e.gst("ping", "far", "-s", size); code != 2 {
			t.Fatalf("-s %s: exit %d", size, code)
		}
	}
}

func TestPingNoAnswer(t *testing.T) {
	e := newEnv(t)
	e.back.ping = func(node string) (wire.NetPing, error) {
		return wire.NetPing{}, errors.New(`no machine "` + node + `" in the network`)
	}
	code, stdout, stderr := e.gst("ping", "nowhere", "-c", "1")
	if code != exitFailure {
		t.Fatalf("code %d", code)
	}
	if want := "no reply: no machine \"nowhere\" in the network\n"; stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
	}
	if want := "error: nowhere did not answer: no machine \"nowhere\" in the network\nhint: see the machines with `gst status`\n"; stderr != want {
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
		"direct 192.0.2.2:6000, 1.5 ms\n",
		"SSH links: devbox answered in 74.1 ms\n",
		"UDP path to devbox: up, 192.0.2.1:5000 -> 192.0.2.2:6000\n",
		"This machine:\n    0.000s  offering\n    1.500s  up\n",
		"devbox:\n  no try yet\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q lacks %q", out, want)
		}
	}

	// The try before comes first, and how it ended.
	e.back.direct = func(node string) (wire.DirectReport, error) {
		return wire.DirectReport{Node: peerOf(node), Peer: wire.DirectTrace{
			Previous: []wire.DirectEvent{event(0, "up"), event(20*time.Second, "fell silent")},
			Events:   []wire.DirectEvent{event(25*time.Second, "offer over SSH")},
		}}, nil
	}
	out = e.mustGst("ping", "devbox", "-c", "1", "-v")
	if want := "devbox:\n  the try before:\n    0.000s  up\n   20.000s  fell silent\n  the latest try:\n   25.000s  offer over SSH\n"; !strings.Contains(out, want) {
		t.Fatalf("output %q lacks %q", out, want)
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

}

// Nodes that are offline show when they were last online.
func TestStatusShowsOfflineNodes(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	a, b := nodeID("aaaa1111"), nodeID("bbbb2222")
	e.back.topology = wire.Topology{Self: a, Nodes: []wire.TopoNode{
		{ID: a, Name: "laptop", Reachable: true, Route: []string{a}},
		{ID: b, Name: "tablet", Seen: time.Now().Add(-3 * time.Hour)},
	}}
	contains(t, e.mustGst("status"), "offline, seen 3h ago")
}

// A machine that was removed says so, and how to join again.
func TestStatusShowsARemoval(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	e.back.topology = wire.Topology{Self: nodeID("aaaa1111"), Nodes: []wire.TopoNode{{ID: nodeID("aaaa1111"), Name: "laptop", Reachable: true}}}
	e.back.removed = &wire.Removed{By: peerOf("devbox"), At: time.Now().Add(-2 * time.Hour)}
	e.back.net = &wire.NetState{Error: "this machine is not in a network"}
	out := e.mustGst("status")
	contains(t, out, "This machine was removed from the network by devbox 2h ago. Join again with `gst add <destination>`")
	// The removal says it already.
	if strings.Contains(out, "not in a network") {
		t.Fatalf("output %q", out)
	}
}
