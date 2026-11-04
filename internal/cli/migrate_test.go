package cli

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

const legacyID = "0123456789abcdef0123456789abcdef"

// legacyConfig is a configuration of gst 0.2 (version 2): tunnels with
// forwards, one of them through the mesh.
const legacyConfig = `{
  "version": 2,
  "mesh": {"id": "` + legacyID + `", "name": "laptop"},
  "tunnels": {
    "myhost": {
      "destination": "myhost",
      "keepalive_interval_seconds": 30,
      "forwards": [
        {"direction": "local", "listen": "127.0.0.1:15432", "target": "127.0.0.1:5432"},
        {"direction": "remote", "listen": "127.0.0.1:2222", "target": "localhost:22"},
        {"direction": "dynamic", "listen": "127.0.0.1:1081"}
      ]
    },
    "viamesh": {
      "destination": "mesh:devcn",
      "forwards": [{"direction": "local", "listen": "127.0.0.1:8080", "target": "localhost:80"}]
    }
  }
}
`

func TestMigrationUpgradesOnFirstCommand(t *testing.T) {
	e := newEnv(t)
	write(t, e.config, legacyConfig)

	code, stdout, stderr := e.gst("link", "list")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, w := range []string{
		"note: gst 0.3 replaced tunnels with links and their -L forwards with maps (see gst link list and gst map list)\n",
		"note: dropped tunnel myhost: -R 127.0.0.1:2222:localhost:22; recreate it with gst map on the other machine, or with gst route\n",
		"note: dropped tunnel myhost: -D 127.0.0.1:1081; recreate it with gst map on the other machine, or with gst route\n",
	} {
		if !strings.Contains(stderr, w) {
			t.Errorf("stderr lacks %q:\n%s", w, stderr)
		}
	}
	if n := strings.Count(stderr, "note: dropped"); n != 2 {
		t.Errorf("%d notes about dropped forwards, want 2:\n%s", n, stderr)
	}
	if !strings.Contains(stdout, "myhost") || strings.Contains(stdout, "viamesh") {
		t.Fatalf("the upgrade keeps the SSH tunnels as links and tunnels through the mesh as maps:\n%s", stdout)
	}

	f := e.load()
	if f.Version != config.Version || f.Node == nil || f.Node.ID != legacyID || f.Node.Name != "laptop" {
		t.Fatalf("file %+v node %+v", f, f.Node)
	}
	if l := f.Links["myhost"]; l == nil || l.Destination != "myhost" || l.KeepaliveIntervalSeconds != 30 || len(f.Links) != 1 {
		t.Fatalf("links %+v", f.Links)
	}
	wantMaps := []config.Map{
		{Listen: "127.0.0.1:15432", Machine: "myhost", Target: "127.0.0.1:5432"},
		{Listen: "127.0.0.1:8080", Machine: "devcn", Target: "localhost:80"},
	}
	if !slices.Equal(f.Maps, wantMaps) {
		t.Fatalf("maps\n got %+v\nwant %+v", f.Maps, wantMaps)
	}
	if !slices.Equal(e.back.calls, []string{"sync"}) || !e.back.installed {
		t.Fatalf("the upgraded configuration was not applied: calls %v", e.back.calls)
	}
}

func TestMigrationHappensOnce(t *testing.T) {
	e := newEnv(t)
	write(t, e.config, legacyConfig)
	e.mustGst("map", "list")
	upgraded, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(e.config)
	if err != nil {
		t.Fatal(err)
	}

	e.resetCalls()
	for _, args := range [][]string{{"link", "list"}, {"map", "list", "--json"}, {"nodes"}, {"proxy"}, {"route", "list"}} {
		_, _, stderr := e.gst(args...)
		if strings.Contains(stderr, "note:") {
			t.Errorf("gst %v: stderr %q", args, stderr)
		}
	}
	after, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(upgraded) || !info.ModTime().Equal(before.ModTime()) {
		t.Fatalf("a current configuration was rewritten:\n%s", after)
	}
	if slices.Contains(e.back.calls, "sync") {
		t.Fatalf("read-only commands synced the daemon: %v", e.back.calls)
	}
}

func TestMigrationLeavesCurrentConfigAlone(t *testing.T) {
	e := newEnv(t)
	current := `{"version":3,"links":{},"maps":[],"routes":[]}` + "\n"
	write(t, e.config, current)
	_, _, stderr := e.gst("link", "list")
	if stderr != "" {
		t.Fatalf("stderr %q", stderr)
	}
	if got, _ := os.ReadFile(e.config); string(got) != current {
		t.Fatalf("a current configuration was rewritten: %s", got)
	}
	if len(e.back.calls) != 0 {
		t.Fatalf("calls %v", e.back.calls)
	}
}

func TestMigrationSkipsCommandsWithoutConfiguration(t *testing.T) {
	e := newEnv(t)
	write(t, e.config, legacyConfig)
	for _, args := range [][]string{{"version"}, {"--help"}, {"help", "link"}, {"link", "--help"}} {
		e.mustGst(args...)
	}
	if got, _ := os.ReadFile(e.config); string(got) != legacyConfig {
		t.Fatalf("the configuration was upgraded by a command that does not use it: %s", got)
	}
	if len(e.back.calls) != 0 {
		t.Fatalf("calls %v", e.back.calls)
	}
}

func TestMigrationFailureToApply(t *testing.T) {
	e := newEnv(t)
	write(t, e.config, legacyConfig)
	e.back.syncErr = os.ErrPermission
	e.expectError(exitFailure, []string{"link", "list"}, "error: apply the upgraded configuration: ", "hint: retry with `gst up`\n")
	// The file is upgraded all the same, so the daemon is applied by gst up.
	if e.load().Version != config.Version {
		t.Fatal("the configuration was not upgraded")
	}
}
