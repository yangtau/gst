package cli

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func nodeID(prefix string) string { return prefix + strings.Repeat("0", 32-len(prefix)) }

// meshTopology is a mesh as seen from laptop: devbox is linked to it, far
// and dup1 are behind devbox, dup2 shares dup1's name and is out of reach.
func meshTopology() wire.Topology {
	self, devbox, far, dup1, dup2 := nodeID("aaaa1111"), nodeID("bbbb2222"), nodeID("cccc3333"), nodeID("dddd4444"), nodeID("eeee5555")
	return wire.Topology{
		Self: self,
		Nodes: []wire.TopoNode{
			{ID: self, Name: "laptop", Software: "gst 0.4.0", Reachable: true, Path: []string{self}},
			{ID: devbox, Name: "devbox", Software: "gst 0.4.0", Reachable: true, Path: []string{self, devbox}},
			{ID: far, Name: "far", Reachable: true, Path: []string{self, devbox, far}},
			{ID: dup1, Name: "dup", Software: "gst 0.4.0", Reachable: true, Path: []string{self, devbox, dup1}},
			{ID: dup2, Name: "dup", Software: "gst 0.4.0"},
		},
		Edges: []wire.TopoEdge{
			{From: devbox, To: dup1, Link: "d1"},
			{From: self, To: devbox, Link: "devbox"},
			{From: devbox, To: far, Link: "far"},
		},
	}
}

// --- name ---

func TestName(t *testing.T) {
	e := newEnv(t)
	idPattern := regexp.MustCompile(`^This machine is ([a-z0-9-]+) \(([0-9a-f]{32})\)\.\n$`)

	out := e.mustGst("name")
	m := idPattern.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("output %q", out)
	}
	id := m[2]
	if len(e.back.calls) != 0 {
		t.Fatalf("showing the name reached the daemon: %v", e.back.calls)
	}

	out = e.mustGst("name", "laptop")
	if want := "This machine is laptop (" + id + ").\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if got := e.load().Node; got.Name != "laptop" || got.ID != id {
		t.Fatalf("node %+v", got)
	}
	if !slices.Equal(e.back.calls, []string{"sync"}) {
		t.Fatalf("a change must sync: %v", e.back.calls)
	}

	e.resetCalls()
	e.mustGst("name", "laptop")
	e.mustGst("name")
	if len(e.back.calls) != 0 {
		t.Fatalf("an unchanged name reached the daemon: %v", e.back.calls)
	}

	for _, args := range [][]string{{"name", "Laptop"}, {"name", "my laptop"}, {"name", "a", "b"}} {
		e.expectCode(exitUsage, args...)
	}
	if e.load().Node.Name != "laptop" {
		t.Fatal("a rejected name was saved")
	}
}

// --- up and down ---

func TestUpDown(t *testing.T) {
	e := newEnv(t)
	out := e.mustGst("up")
	if !regexp.MustCompile(`^gst is up; this machine is [a-z0-9-]+ \([0-9a-f]{8}\)\.\n$`).MatchString(out) {
		t.Fatalf("output %q", out)
	}
	if !e.back.installed || !slices.Equal(e.back.command, []string{e.exe, "--config", e.config, "daemon"}) {
		t.Fatalf("installed %v, command %v", e.back.installed, e.back.command)
	}
	if !slices.Equal(e.back.calls, []string{"sync"}) || e.load().Down {
		t.Fatalf("calls %v, down %v", e.back.calls, e.load().Down)
	}

	e.resetCalls()
	if out := e.mustGst("down"); out != "gst is down; run gst up to start it again.\n" {
		t.Fatalf("output %q", out)
	}
	if e.back.installed || e.back.command != nil || !e.load().Down || !slices.Equal(e.back.calls, []string{"sync (uninstall)"}) {
		t.Fatalf("installed %v, command %v, down %v, calls %v", e.back.installed, e.back.command, e.load().Down, e.back.calls)
	}
	// Down is idempotent.
	e.mustGst("down")

	e.mustGst("up")
	if !e.back.installed || e.load().Down {
		t.Fatalf("up did not bring the daemon back: installed %v, down %v", e.back.installed, e.load().Down)
	}
	e.expectCode(exitUsage, "up", "now")
	e.expectCode(exitUsage, "down", "now")
}

func TestChangesWhileDown(t *testing.T) {
	e := newEnv(t)
	e.mustGst("down")
	e.resetCalls()

	code, stdout, stderr := e.gst("add", "myhost")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if want := "note: gst is down on this machine; changes apply after `gst up`\n"; stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
	contains(t, stdout, "Added link myhost")
	if e.back.installed || !slices.Equal(e.back.calls, []string{"sync (uninstall)"}) {
		t.Fatalf("a down machine must not run the daemon: installed %v, calls %v", e.back.installed, e.back.calls)
	}
	if e.link("myhost") == nil {
		t.Fatal("the change was not saved")
	}

	_, _, stderr = e.gst("name", "laptop")
	contains(t, stderr, "note: gst is down on this machine")

	e.mustGst("up")
	if !e.back.installed || e.load().Down {
		t.Fatal("up did not start the daemon")
	}
	// Only the changes made while down print the note.
	if _, _, stderr := e.gst("name", "devbox"); strings.Contains(stderr, "gst is down") {
		t.Fatalf("stderr %q", stderr)
	}
}

func TestUpFailureReportsDaemonError(t *testing.T) {
	e := newEnv(t)
	e.back.syncErr = errors.New("service manager unavailable")
	e.expectError(exitFailure, []string{"up"}, "error: service manager unavailable\n")
}
