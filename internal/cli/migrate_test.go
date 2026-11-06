package cli

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

const legacyID = "0123456789abcdef0123456789abcdef"

// legacyConfig is a configuration of gst 0.3 (version 3), with a proxy, a
// map, and a route.
const legacyConfig = `{
  "version": 3,
  "node": {"id": "` + legacyID + `", "name": "laptop", "exit": true},
  "proxy": "127.0.0.1:1081",
  "links": {"myhost": {"destination": "myhost"}},
  "maps": [{"listen": "127.0.0.1:15432", "machine": "myhost", "target": "/run/app.sock"}],
  "routes": [{"match": "default", "via": "myhost"}]
}
`

func TestMigrationUpgradesOnFirstCommand(t *testing.T) {
	e := newEnv(t)
	write(t, e.config, legacyConfig)

	code, stdout, stderr := e.gst("status", "--json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, w := range []string{
		"note: gst 0.4 reaches machines over the virtual network instead: <machine>.gst from any program (see gst install)\n",
		"note: dropped proxy 127.0.0.1:1081\n",
		"note: dropped map 127.0.0.1:15432 to /run/app.sock on myhost\n",
		"note: dropped route default via myhost\n",
	} {
		if !strings.Contains(stderr, w) {
			t.Errorf("stderr lacks %q:\n%s", w, stderr)
		}
	}
	if self := decode[meshStatus](t, stdout).Self; len(self.Links) != 1 || self.Links[0].Name != "myhost" {
		t.Fatalf("the upgrade keeps the links:\n%s", stdout)
	}

	f := e.load()
	if f.Version != config.Version || f.Node == nil || f.Node.ID != legacyID || f.Node.Name != "laptop" {
		t.Fatalf("file %+v node %+v", f, f.Node)
	}
	if l := f.Links["myhost"]; l == nil || *l != (config.Link{Destination: "myhost"}) || len(f.Links) != 1 {
		t.Fatalf("links %+v", f.Links)
	}
	if len(e.back.calls) == 0 || e.back.calls[0] != "sync" || !e.back.installed {
		t.Fatalf("the upgraded configuration was not applied: calls %v", e.back.calls)
	}
}

func TestMigrationHappensOnce(t *testing.T) {
	e := newEnv(t)
	write(t, e.config, legacyConfig)
	e.mustGst("name")
	upgraded, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(e.config)
	if err != nil {
		t.Fatal(err)
	}

	e.resetCalls()
	for _, args := range [][]string{{"status"}, {"status", "--json"}} {
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
	current := `{"version":4,"links":{}}` + "\n"
	write(t, e.config, current)
	_, _, stderr := e.gst("status")
	if strings.Contains(stderr, "note:") {
		t.Fatalf("stderr %q", stderr)
	}
	if got, _ := os.ReadFile(e.config); string(got) != current {
		t.Fatalf("a current configuration was rewritten: %s", got)
	}
	if slices.Contains(e.back.calls, "sync") {
		t.Fatalf("calls %v", e.back.calls)
	}
}

func TestMigrationSkipsCommandsWithoutConfiguration(t *testing.T) {
	e := newEnv(t)
	write(t, e.config, legacyConfig)
	for _, args := range [][]string{{"version"}, {"--help"}, {"help", "add"}, {"add", "--help"}} {
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
	e.expectError(exitFailure, []string{"name"}, "error: apply the upgraded configuration: ", "hint: retry with `gst up`\n")
	// The file is upgraded all the same, so the daemon is applied by gst up.
	if e.load().Version != config.Version {
		t.Fatal("the configuration was not upgraded")
	}
}
