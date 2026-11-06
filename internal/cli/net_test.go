package cli

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func runningNet() *wire.NetState {
	return &wire.NetState{
		Subnet: "10.99.0.0/16", Address: "10.99.1.2", Interface: "gst0",
		Peers: []wire.NetPeer{
			{Node: peerOf("devbox").ID, Name: "devbox", Address: "10.99.3.4", Path: wire.PathDirect, Endpoint: "192.0.2.2:6000", Handshake: time.Now().Add(-time.Minute), Received: 2048, Sent: 5},
			{Node: peerOf("homelab").ID, Name: "homelab", Address: "10.99.5.6", Path: wire.PathSSH},
		},
	}
}

func TestSubnetShows(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	e.back.net = runningNet()
	if out := e.mustGst("subnet"); out != "10.99.0.0/16\n" {
		t.Fatalf("output %q", out)
	}
	e.back.net = nil
	e.expectError(exitFailure, []string{"subnet"}, "no subnet yet", "gst status")
	e.mustGst("down")
	e.expectError(exitFailure, []string{"subnet"}, "does not answer", "gst up")
}

func TestSubnetChanges(t *testing.T) {
	e := newEnv(t)
	e.back.net = runningNet()
	contains(t, e.mustGst("subnet", "10.99.7.0/16"), "10.99.1.2", "gst0", "10.99.0.0/16")
	n := e.load().Net
	if n == nil || n.Subnet != "10.99.0.0/16" || n.Since == 0 {
		t.Fatalf("config: %+v", n)
	}
	if !e.back.installed {
		t.Fatal("gst subnet did not start the daemon")
	}
}

func TestSubnetRejects(t *testing.T) {
	e := newEnv(t)
	for _, subnet := range []string{"8.8.8.0/24", "10.0.0.0/7", "nonsense"} {
		e.expectError(exitUsage, []string{"subnet", subnet}, "error:", "usage:")
	}
	if e.load().Net != nil {
		t.Fatal("a rejected subnet was saved")
	}
}

func TestSubnetReportsWhatToDo(t *testing.T) {
	e := newEnv(t)
	e.back.net = &wire.NetState{Error: "the helper of the virtual network does not run", Hint: "install it with `gst up`"}
	e.expectError(exitFailure, []string{"subnet", "10.99.0.0/16"}, "error: the helper", "hint: install it with `gst up`")
	if e.load().Net == nil {
		t.Fatal("the choice should apply once the helper is there")
	}
}

func TestStatusShowsTheNetwork(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	e.back.net = runningNet()
	self, peer := peerOf("laptop"), peerOf("devbox")
	e.back.topology = wire.Topology{Self: self.ID, Nodes: []wire.TopoNode{
		{ID: self.ID, Name: "laptop", Reachable: true, Net: &wire.NetInfo{Address: "10.99.1.2"}},
		{ID: peer.ID, Name: "devbox", Reachable: true, Path: []string{self.ID, peer.ID}},
	}}
	out := e.mustGst("status")
	contains(t, out, "10.99.1.2")
	if lines := strings.Split(out, "\n"); !strings.Contains(lines[1], " - ") {
		t.Fatalf("no placeholder for the machine off the network:\n%s", out)
	}

	all := e.mustGst("status", "--all")
	contains(t, all, "MACHINE", "1m0s ago", "2.0 KiB", "5 B", "never")
	found := false
	for _, row := range rows(all) {
		if len(row) > 2 && row[0] == "devbox" && row[1] == "10.99.3.4" {
			found = row[2] == "direct" && row[3] == "192.0.2.2:6000"
		}
	}
	if !found {
		t.Fatalf("no line of devbox:\n%s", all)
	}
	v := decode[meshStatus](t, e.mustGst("status", "--json"))
	if v.Self.Net == nil || v.Self.Net.Address != "10.99.1.2" || len(v.Self.Net.Peers) != 2 {
		t.Fatalf("view: %+v", v.Self.Net)
	}

	e.back.net = &wire.NetState{Error: "the helper does not run", Hint: "install it with `gst up`"}
	contains(t, e.mustGst("status"), "# Problems:", "virtual network: the helper does not run; install it with `gst up`")

	e.back.net = runningNet()
	contains(t, e.mustGst("status", "devbox"), "Address: 10.99.1.2 (subnet 10.99.0.0/16)")
}

// A path that outlives the SSH links still counts as online.
func TestStatusOfAMachineWithOnlyAUDPPath(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	a, b := nodeID("aaaa1111"), nodeID("bbbb2222")
	e.back.topology = wire.Topology{Self: a, Nodes: []wire.TopoNode{
		{ID: a, Name: "a", Reachable: true, Path: []string{a}},
		{ID: b, Name: "b", Direct: true},
	}}
	contains(t, e.mustGst("status"), "online; direct")
}

func TestInstallNeedsRoot(t *testing.T) {
	e := newEnv(t)
	e.expectError(exitFailure, []string{"install"}, "needs root", "gst up")
	e.expectError(exitFailure, []string{"uninstall"}, "needs root", "gst down")
}

func TestUpLeavesACurrentHelperAlone(t *testing.T) {
	e := newEnv(t)
	e.terminal = true
	code, _, stderr := e.gst("up")
	if code != exitOK || stderr != "" || len(e.rootRuns) != 0 {
		t.Fatalf("exit %d, stderr %q, ran %v", code, stderr, e.rootRuns)
	}
}

func TestUpAsksBeforeSudo(t *testing.T) {
	for _, tt := range []struct {
		name, answer string
		runs         bool
	}{
		{"yes", "y\n", true},
		{"word", "YES\n", true},
		{"no", "n\n", false},
		{"enter", "\n", false},
		{"end of input", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.helper = fakeHelper{}
			e.terminal, e.stdin = true, tt.answer
			code, stdout, stderr := e.gst("up")
			if code != exitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			contains(t, stdout, "gst is up")
			contains(t, stderr, "needs root to put this machine", "sudo /usr/local/bin/gst install --user ", "copies gst",
				"The service then does only this", "gst never sees it", "Run it? [y/N]")
			if tt.runs != (len(e.rootRuns) == 1) {
				t.Fatalf("ran %v", e.rootRuns)
			}
			if tt.runs && !strings.HasPrefix(e.rootRuns[0], "sudo /usr/local/bin/gst install --user ") {
				t.Fatalf("ran %v", e.rootRuns)
			}
			if !tt.runs {
				contains(t, stderr, "nothing was run")
			}
		})
	}
}

func TestUpUpdatesAnOldHelper(t *testing.T) {
	e := newEnv(t)
	e.helper = fakeHelper{installed: true}
	e.terminal, e.stdin = true, "y\n"
	_, _, stderr := e.gst("up")
	contains(t, stderr, "needs an update")
	if len(e.rootRuns) != 1 {
		t.Fatalf("ran %v", e.rootRuns)
	}
}

func TestUpWithoutATerminalDoesNotRunSudo(t *testing.T) {
	e := newEnv(t)
	e.helper = fakeHelper{}
	code, _, stderr := e.gst("up")
	if code != exitOK || len(e.rootRuns) != 0 {
		t.Fatalf("exit %d, ran %v", code, e.rootRuns)
	}
	contains(t, stderr, "gst asks first", "gst up --yes")
}

func TestUpYesStillSaysWhatItRuns(t *testing.T) {
	e := newEnv(t)
	e.helper = fakeHelper{}
	code, _, stderr := e.gst("up", "--yes")
	if code != exitOK || len(e.rootRuns) != 1 {
		t.Fatalf("exit %d, ran %v", code, e.rootRuns)
	}
	contains(t, stderr, "sudo /usr/local/bin/gst install --user ", "copies gst")
	if strings.Contains(stderr, "[y/N]") {
		t.Fatalf("asked: %s", stderr)
	}
}

func TestUpAsRootNeedsNoSudo(t *testing.T) {
	e := newEnv(t)
	e.helper, e.euid = fakeHelper{}, 0
	e.gst("up", "-y")
	if len(e.rootRuns) != 1 || !strings.HasPrefix(e.rootRuns[0], "/usr/local/bin/gst install --user ") {
		t.Fatalf("ran %v", e.rootRuns)
	}
}

func TestUpReportsAFailedSudo(t *testing.T) {
	e := newEnv(t)
	e.helper = fakeHelper{}
	e.rootErr = errors.New("exit status 1")
	code, stdout, stderr := e.gst("up", "--yes")
	if code != exitFailure {
		t.Fatalf("exit %d", code)
	}
	contains(t, stdout, "gst is up") // the links run all the same
	contains(t, stderr, "failed", "try again with `gst up`")
}

func TestUpTwiceChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.terminal = true
	first := e.mustGst("up")
	if second := e.mustGst("up"); second != first || len(e.rootRuns) != 0 {
		t.Fatalf("%q then %q, ran %v", first, second, e.rootRuns)
	}
}

func TestDownRemovesTheHelperWithoutRoot(t *testing.T) {
	e := newEnv(t)
	contains(t, e.mustGst("down"), "gst is down", "helper of the virtual network is removed")
	if e.helper.installed || len(e.rootRuns) != 0 {
		t.Fatalf("helper %+v, ran %v", e.helper, e.rootRuns)
	}
	// There is nothing left to remove, and gst up asks again.
	if out := e.mustGst("down"); strings.Contains(out, "removed") {
		t.Fatalf("gst down again: %q", out)
	}
	e.terminal, e.stdin = true, "y\n"
	if _, _, stderr := e.gst("up"); !strings.Contains(stderr, "Run it? [y/N]") || !strings.Contains(stderr, "gst down removes all of this") {
		t.Fatalf("gst up after gst down: %q", stderr)
	}
}

func TestDownAsksWhenTheHelperDoesNotAnswer(t *testing.T) {
	e := newEnv(t)
	e.removeErr = errors.New("no answer")
	e.expectError(exitFailure, []string{"down"}, "takes root", "gst down --yes")
	if len(e.rootRuns) != 0 {
		t.Fatalf("ran %v", e.rootRuns)
	}

	e.terminal, e.stdin = true, "n\n"
	e.expectError(exitFailure, []string{"down"}, "still installed", "Run it? [y/N]")
	if len(e.rootRuns) != 0 {
		t.Fatalf("ran %v", e.rootRuns)
	}

	e.stdin = "y\n"
	e.mustGst("down")
	if want := []string{"sudo /usr/local/bin/gst uninstall"}; !slices.Equal(e.rootRuns, want) {
		t.Fatalf("ran %v", e.rootRuns)
	}
}

func TestAddTellsWhenTheOtherMachineIsNotUp(t *testing.T) {
	e := newEnv(t)
	e.back.linkStates["myhost"] = wire.LinkState{Name: "myhost", State: wire.StateDown, Error: "error: " + notUp + "; run `gst up` there"}
	e.expectError(exitFailure, []string{"add", "myhost", "--remote-gst", "/opt/gst"},
		"gst is not up on that machine", "`ssh -t myhost /opt/gst up`", "connects by itself")
	if len(e.rootRuns) != 0 {
		t.Fatalf("ran %v", e.rootRuns)
	}
	f, err := config.Load(e.config)
	if err != nil || f.Links["myhost"] == nil {
		t.Fatalf("the link is gone: %+v, %v", f, err)
	}
}

func TestAddReturnsOnOtherFailures(t *testing.T) {
	e := newEnv(t)
	e.back.linkStates["myhost"] = wire.LinkState{Name: "myhost", State: wire.StateDown, Error: "connection refused"}
	contains(t, e.mustGst("add", "myhost"), "Added link myhost")
}
