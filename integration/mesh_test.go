//go:build integration

package integration

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Three machines a -> b <- c, where a and c each link to b and know
// nothing of each other: a reaches c through b. The subtests share the
// machines and run in order.
func TestMesh(t *testing.T) {
	t.Parallel()
	a, b, c, _, cb := start3(t)
	for _, n := range []*node{a, b, c} {
		n.installHelper()
	}
	a.waitNet(2)

	t.Run("Topology", func(t *testing.T) {
		cNode, topo := a.waitReachable("c")
		if got := path(topo, cNode); got != "a,b,c" {
			t.Fatalf("a reaches c by %s, want a,b,c", got)
		}
		if len(topo.Nodes) != 3 || len(topo.Edges) != 2 {
			t.Fatalf("a sees %+v", topo)
		}
		bNode, _ := find(topo, "b")
		if got := path(topo, bNode); got != "a,b" || !strings.Contains(bNode.Software, "gst") {
			t.Fatalf("a reaches b by %s as %q", got, bNode.Software)
		}
		// gst status lists every machine, with the ones in between.
		out := a.mustGst("status")
		for _, want := range []string{"online; via b", "gst "} {
			if !strings.Contains(out, want) {
				t.Fatalf("gst status lacks %q:\n%s", want, out)
			}
		}
		if all := a.mustGst("status", "--all"); !strings.Contains(all, "LINK") || !strings.Contains(all, "->") {
			t.Fatalf("gst status --all lacks the links:\n%s", all)
		}
		var names []string
		for line := range strings.Lines(out) {
			if f := strings.Fields(line); len(f) > 1 {
				names = append(names, f[1])
			}
		}
		if strings.Join(names, ",") != "a,b,c" {
			t.Fatalf("gst status lists %v, want a (this machine) and then b and c:\n%s", names, out)
		}
		if strings.Contains(out, "# Problems") || strings.Contains(out, "offline") {
			t.Fatalf("gst status reports problems in a healthy mesh:\n%s", out)
		}
		st := a.meshStatus()
		if st.Self.Node.Name != "a" || len(st.Self.Links) != 1 || st.Mesh.Self != st.Self.Node.ID || len(st.Mesh.Nodes) != 3 {
			t.Fatalf("gst status --json: %+v", st)
		}
		// The other end learns of a the same way.
		aNode, topo := c.waitReachable("a")
		if got := path(topo, aNode); got != "c,b,a" {
			t.Fatalf("c reaches a by %s, want c,b,a", got)
		}
	})

	// gst ping asks a machine to answer through the mesh and names the
	// machines on the way.
	t.Run("Ping", func(t *testing.T) {
		a.waitReachable("c")
		pong := regexp.MustCompile(`^pong from (\w+) \([0-9a-f]{8}\) (over SSH|via [\w, ]+) in [0-9]+\.[0-9] ms$`)
		for _, tc := range []struct{ machine, path string }{{"c", "via b"}, {"b", "over SSH"}, {"c.gst", "via b"}} {
			out := a.mustGst("ping", tc.machine, "-c", "1")
			m := pong.FindStringSubmatch(strings.TrimSpace(out))
			if m == nil || m[1] != strings.TrimSuffix(tc.machine, ".gst") || m[2] != tc.path {
				t.Fatalf("gst ping %s: %q, want an answer %s", tc.machine, out, tc.path)
			}
		}

		var pings []wire.Ping
		if err := json.Unmarshal([]byte(a.mustGst("ping", "c", "-c", "1", "--json")), &pings); err != nil {
			t.Fatal(err)
		}
		cNode, _ := find(a.nodes(), "c")
		if len(pings) != 1 || pings[0].Node.ID != cNode.ID || pings[0].Node.Name != "c" ||
			len(pings[0].Via) != 1 || pings[0].Via[0].Name != "b" || pings[0].TimeMS <= 0 {
			t.Fatalf("gst ping c --json: %+v", pings)
		}

		// A machine is named by its link, or by an id prefix.
		for _, machine := range []string{"tob", "C", cNode.ID[:8], cNode.ID[:4] + ".gst"} {
			a.mustGst("ping", machine, "-c", "1")
		}

		// Without an answer, the command says so and fails with a hint.
		code, stdout, stderr := a.gst("ping", "nowhere", "-c", "1")
		if code != 1 || !strings.Contains(stdout, "no answer:") || !strings.Contains(stderr, "error: nowhere did not answer") || !strings.Contains(stderr, "hint:") {
			t.Fatalf("gst ping nowhere: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	})

	// gst status shows another machine's links, asked through the mesh:
	// c's on a, two hops away.
	t.Run("Status", func(t *testing.T) {
		status := func(n *node, machine string) wire.Status {
			t.Helper()
			var st wire.Status
			if err := json.Unmarshal([]byte(n.mustGst("status", machine, "--json")), &st); err != nil {
				t.Fatal(err)
			}
			return st
		}
		c.mustGst("add", "spare", cb.deadEnd())

		var st wire.Status
		eventually(t, 30*time.Second, "a does not see c's links", func() bool {
			st = status(a, "c")
			return len(st.Links) == 2 && st.Links[0].State == wire.StateDown && st.Links[0].Error != ""
		})
		if st.Node.Name != "c" || st.Node.ID == a.meshStatus().Self.Node.ID {
			t.Fatalf("status of c is for %+v", st.Node)
		}
		if len(st.Links) != 2 || st.Links[0].Name != "spare" || st.Links[0].State != wire.StateDown || st.Links[0].Error == "" || st.Links[0].Destination != "dead-"+cb.server.container ||
			st.Links[1].Name != "tob" || st.Links[1].State != wire.StateRunning || st.Links[1].Peer == nil || st.Links[1].Peer.Name != "b" {
			t.Fatalf("c's links = %+v", st.Links)
		}

		// a's own status is not c's.
		own := a.meshStatus().Self
		if own.Node.Name != "a" || len(own.Links) != 1 || own.Links[0].Name != "tob" {
			t.Fatalf("a's status = %+v", own)
		}
		out := a.mustGst("status", "c.gst")
		for _, want := range []string{"LINK", "tob", "running", "spare", "down", "Address: "} {
			if !strings.Contains(out, want) {
				t.Fatalf("gst status c.gst lacks %q:\n%s", want, out)
			}
		}
		// c's own status lists its failed link as a problem; a's does not.
		if out := c.mustGst("status"); !strings.Contains(out, "# Problems:") || !strings.Contains(out, "->spare is down") {
			t.Fatalf("gst status on c:\n%s", out)
		}
		if out := a.mustGst("status"); strings.Contains(out, "# Problems") {
			t.Fatalf("gst status on a:\n%s", out)
		}
		// b only accepts links, so it has none of its own.
		if out := a.mustGst("status", "b"); !strings.Contains(out, "No links.") {
			t.Fatalf("gst status b:\n%s", out)
		}
		if code, _, stderr := a.gst("status", "nowhere"); code != 1 || !strings.Contains(stderr, "hint:") || !strings.Contains(stderr, "gst status") {
			t.Fatalf("gst status nowhere: exit %d, stderr %q", code, stderr)
		}
	})
}
