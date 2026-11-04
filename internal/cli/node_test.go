package cli

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func nodeID(prefix string) string { return prefix + strings.Repeat("0", 32-len(prefix)) }

// meshTopology is a mesh as seen from laptop: devcn is linked to it, far
// and dup1 are behind devcn, dup2 shares dup1's name and is out of reach.
func meshTopology() wire.Topology {
	self, devcn, far, dup1, dup2 := nodeID("aaaa1111"), nodeID("bbbb2222"), nodeID("cccc3333"), nodeID("dddd4444"), nodeID("eeee5555")
	return wire.Topology{
		Self: self,
		Nodes: []wire.TopoNode{
			{ID: self, Name: "laptop", Software: "gst 0.3.0", Reachable: true, Path: []string{self}},
			{ID: devcn, Name: "devcn", Software: "gst 0.3.0", Exit: true, Reachable: true, Path: []string{self, devcn}},
			{ID: far, Name: "far", Reachable: true, Path: []string{self, devcn, far}},
			{ID: dup1, Name: "dup", Software: "gst 0.3.0", Reachable: true, Path: []string{self, devcn, dup1}},
			{ID: dup2, Name: "dup", Software: "gst 0.3.0"},
		},
		Edges: []wire.TopoEdge{
			{From: devcn, To: dup1, Link: "d1"},
			{From: self, To: devcn, Link: "devcn"},
			{From: devcn, To: far, Link: "far"},
		},
	}
}

// --- connect ---

func TestConnect(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		args []string
		call string
	}{
		{[]string{"devcn", "5432"}, "connect devcn tcp localhost:5432"},
		{[]string{"devcn.gst", "5432"}, "connect devcn tcp localhost:5432"},
		{[]string{"DevCN.GST", "db.internal:5432"}, "connect devcn tcp db.internal:5432"},
		{[]string{"devcn", "127.0.0.1:22"}, "connect devcn tcp 127.0.0.1:22"},
		{[]string{"devcn", "/run/app.sock"}, "connect devcn unix /run/app.sock"},
		{[]string{"cccc3333", "80"}, "connect cccc3333 tcp localhost:80"},
	} {
		e.resetCalls()
		e.back.sent.Reset()
		e.stdin = "request for " + tc.args[0]
		out := e.mustGst(append([]string{"connect"}, tc.args...)...)
		if want := "hello from " + strings.Split(strings.ToLower(tc.call), " ")[1]; out != want {
			t.Errorf("gst connect %v: output %q, want %q", tc.args, out, want)
		}
		if !slices.Equal(e.back.calls, []string{tc.call}) {
			t.Errorf("gst connect %v: calls %v, want %q", tc.args, e.back.calls, tc.call)
		}
		if got := e.back.sent.String(); got != e.stdin {
			t.Errorf("gst connect %v: sent %q, want %q", tc.args, got, e.stdin)
		}
	}
}

func TestConnectWithoutStdin(t *testing.T) {
	e := newEnv(t)
	var stdout, stderr strings.Builder
	a := e.app(&stdout, &stderr)
	a.stdin = nil
	if code := a.execute(context.Background(), []string{"--config", e.config, "connect", "devcn", "22"}); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if stdout.String() != "hello from devcn" {
		t.Fatalf("output %q", stdout.String())
	}
}

func TestConnectErrors(t *testing.T) {
	e := newEnv(t)
	e.expectError(exitFailure, []string{"connect", "nowhere", "22"}, `error: no machine "nowhere" in the mesh`)
	for _, args := range [][]string{
		{"connect", "Bad_Name", "22"},
		{"connect", "dev cn", "22"},
		{"connect", "devcn", "0"},
		{"connect", "devcn", "http"},
		{"connect", "devcn", "relative.sock/x"},
		{"connect", "devcn", "22", "extra"},
	} {
		if stderr := e.expectCode(exitUsage, args...); !strings.Contains(stderr, "usage: gst connect") {
			t.Errorf("gst %v: %q", args, stderr)
		}
	}
	e.expectError(exitUsage, []string{"connect"}, "error: missing argument <machine>\n")
}

// --- name and exit ---

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

func TestExit(t *testing.T) {
	e := newEnv(t)
	out := e.mustGst("exit")
	contains(t, out, " is not an exit: other machines connect through it only to its own ports and sockets.\n")
	if len(e.back.calls) != 0 {
		t.Fatalf("showing reached the daemon: %v", e.back.calls)
	}
	e.mustGst("name", "laptop")

	e.resetCalls()
	if out := e.mustGst("exit", "on"); out != "laptop is an exit: other machines can connect through it to any address.\n" {
		t.Fatalf("output %q", out)
	}
	if !e.load().Node.Exit || !slices.Equal(e.back.calls, []string{"sync"}) {
		t.Fatalf("exit %v, calls %v", e.load().Node.Exit, e.back.calls)
	}
	e.resetCalls()
	contains(t, e.mustGst("exit", "on"), "laptop is an exit")
	contains(t, e.mustGst("exit"), "laptop is an exit")
	if len(e.back.calls) != 0 {
		t.Fatalf("no change must not sync: %v", e.back.calls)
	}
	if out := e.mustGst("exit", "off"); out != "laptop is not an exit: other machines connect through it only to its own ports and sockets.\n" {
		t.Fatalf("output %q", out)
	}
	if e.load().Node.Exit || !slices.Equal(e.back.calls, []string{"sync"}) {
		t.Fatalf("exit %v, calls %v", e.load().Node.Exit, e.back.calls)
	}
	e.expectError(exitUsage, []string{"exit", "yes"}, `error: invalid argument "yes": use on or off`)
	e.expectCode(exitUsage, "exit", "on", "off")
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

	code, stdout, stderr := e.gst("link", "add", "myhost")
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

	_, _, stderr = e.gst("map", "add", "15432", "devcn:5432")
	contains(t, stderr, "note: gst is down on this machine")

	e.mustGst("up")
	if !e.back.installed || e.load().Down {
		t.Fatal("up did not start the daemon")
	}
	// Only the changes made while down print the note.
	if _, _, stderr := e.gst("route", "add", "default", "devcn"); strings.Contains(stderr, "gst is down") {
		t.Fatalf("stderr %q", stderr)
	}
}

func TestUpFailureReportsDaemonError(t *testing.T) {
	e := newEnv(t)
	e.back.syncErr = errors.New("service manager unavailable")
	e.expectError(exitFailure, []string{"up"}, "error: service manager unavailable\n")
}
