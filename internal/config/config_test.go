package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const nodeID = "0123456789abcdef0123456789abcdef"

func TestParseValidation(t *testing.T) {
	const ok = `"version": 4`
	tests := []struct {
		name string
		body string
		want string // substring of the error; empty when valid
	}{
		{"empty", ok, ""},
		{"node", ok + `, "node": {"id": "` + nodeID + `", "name": "laptop"}`, ""},
		{"node bad id", ok + `, "node": {"id": "xyz", "name": "laptop"}`, "invalid node"},
		{"node bad name", ok + `, "node": {"id": "` + nodeID + `", "name": "Laptop"}`, "invalid node"},
		{"link", ok + `, "links": {"myhost": {"destination": "user@myhost", "remote_gst": "/opt/gst"}}`, ""},
		{"link removed field", ok + `, "links": {"myhost": {"destination": "myhost", "identity_file": "/k"}}`, "unknown field"},
		{"link control in remote gst", ok + `, "links": {"myhost": {"destination": "myhost", "remote_gst": "a\nb"}}`, "remote gst"},
		{"link bad name", ok + `, "links": {"My_Host": {"destination": "myhost"}}`, "invalid name"},
		{"link null", ok + `, "links": {"myhost": null}`, "null"},
		{"link option destination", ok + `, "links": {"myhost": {"destination": "-oProxyCommand=x"}}`, "link myhost"},
		{"subnet", ok + `, "net": {"subnet": "10.99.0.0/16", "since": 1}`, ""},
		{"bad subnet", ok + `, "net": {"subnet": "8.8.8.0/24", "since": 1}`, "invalid subnet"},
		{"no subnet", ok + `, "net": {}`, "invalid subnet"},
		{"proxy is gone", ok + `, "proxy": "127.0.0.1:1"`, "unknown field"},
		{"unknown field", ok + `, "bogus": 1`, "unknown field"},
		{"future version", `"version": 5`, "unsupported config version"},
		{"old version", `"version": 2`, "too old"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, notes, err := parse([]byte("{" + tt.body + "}"))
			if tt.want == "" {
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				if notes != nil {
					t.Errorf("notes = %v for a current file", notes)
				}
				if f.Links == nil {
					t.Errorf("links not initialized: %+v", f)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("parse error = %v, want %q", err, tt.want)
			}
		})
	}
	if _, _, err := parse([]byte("not json")); err == nil {
		t.Error("invalid JSON accepted")
	}
}

func TestLoadMissing(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "gst", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Version != Version || f.Node != nil || len(f.Links) != 0 {
		t.Fatalf("missing file loaded as %+v", f)
	}
}

func TestLoadErrorNamesPathButNotValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"version": 4, "links": {"myhost": {"destination": "secret host"}}}`), 0o600)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "link myhost") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestUpdateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gst", "config.json")
	err := Update(path, func(f *File) error {
		f.Links["myhost"] = &Link{Destination: "ssh://user@ssh.example.com:2200", RemoteGst: "/opt/gst"}
		f.Net = &Net{Subnet: "10.99.0.0/16", Since: 7}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, %v; want 0600", info, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(data), "}\n") {
		t.Errorf("file does not end with a newline: %q", data)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Node == nil || !ValidNodeID(f.Node.ID) || ValidateName(f.Node.Name) != nil {
		t.Fatalf("Update did not ensure a node identity: %+v", f.Node)
	}
	want := &File{
		Version: Version, Node: f.Node,
		Links: map[string]*Link{"myhost": {Destination: "ssh://user@ssh.example.com:2200", RemoteGst: "/opt/gst"}},
		Net:   &Net{Subnet: "10.99.0.0/16", Since: 7},
	}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("round trip = %+v, want %+v", f, want)
	}

	// The identity stays across later updates.
	id := *f.Node
	if err := Update(path, func(f *File) error { f.Down = true; return nil }); err != nil {
		t.Fatal(err)
	}
	f, _ = Load(path)
	if *f.Node != id || !f.Down {
		t.Fatalf("second update changed the node: %+v", f)
	}
}

func TestUpdateFailureWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Update(path, func(*File) error { return nil }); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	for name, fn := range map[string]func(*File) error{
		"fn error": func(*File) error { return fmt.Errorf("refused") },
		"invalid":  func(f *File) error { f.Links["Bad Name"] = &Link{Destination: "h"}; return nil },
	} {
		if err := Update(path, fn); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a failed update changed the file")
	}
	if entries, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".config-*")); len(entries) != 0 {
		t.Fatalf("temporary files left: %v", entries)
	}
}

func TestConcurrentUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			err := Update(path, func(f *File) error {
				f.Links[fmt.Sprintf("l%d", i)] = &Link{Destination: "myhost"}
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if f, err := Load(path); err != nil || len(f.Links) != 20 {
		t.Fatalf("lost updates: %d links, %v", len(f.Links), err)
	}
}

func TestApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	Update(path, func(f *File) error { f.Down = true; return nil })
	var saw bool
	if err := Apply(path, func(f *File) error { saw = f.Down; return nil }); err != nil || !saw {
		t.Fatalf("Apply: saw=%v err=%v", saw, err)
	}
	if err := Apply(path, func(*File) error { return fmt.Errorf("boom") }); err == nil {
		t.Fatal("Apply dropped fn's error")
	}
}

const legacyConfig = `{
  "version": 3,
  "node": {"id": "` + nodeID + `", "name": "laptop", "exit": true},
  "proxy": "127.0.0.1:1080",
  "links": {"myhost": {"destination": "myhost", "remote_gst": "/opt/gst"}},
  "maps": [{"listen": "127.0.0.1:5432", "machine": "myhost", "target": "db.internal:5432"}],
  "routes": [{"match": "default", "via": "myhost"}],
  "net": {}
}`

func TestUpgrade(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing.json")
	if up, notes, err := Upgrade(missing); up || notes != nil || err != nil {
		t.Errorf("missing file: %v %v %v", up, notes, err)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("Upgrade created a missing file")
	}

	current := filepath.Join(dir, "current.json")
	os.WriteFile(current, []byte(`{"version": 4}`), 0o600)
	if up, notes, err := Upgrade(current); up || notes != nil || err != nil {
		t.Errorf("current file: %v %v %v", up, notes, err)
	}
	if data, _ := os.ReadFile(current); string(data) != `{"version": 4}` {
		t.Errorf("Upgrade rewrote a current file: %s", data)
	}

	invalid := filepath.Join(dir, "invalid.json")
	os.WriteFile(invalid, []byte(`{"version": 3, "links": {"Bad Name": {"destination": "h"}}}`), 0o600)
	if up, _, err := Upgrade(invalid); up || err == nil {
		t.Errorf("invalid file: %v %v", up, err)
	}

	legacy := filepath.Join(dir, "legacy.json")
	os.WriteFile(legacy, []byte(legacyConfig), 0o600)
	// Load converts without writing.
	f, err := Load(legacy)
	if err != nil || len(f.Links) != 1 {
		t.Fatalf("Load legacy = %+v, %v", f, err)
	}
	if data, _ := os.ReadFile(legacy); string(data) != legacyConfig {
		t.Fatal("Load wrote the legacy file")
	}
	up, notes, err := Upgrade(legacy)
	want := []string{"proxy 127.0.0.1:1080", "map 127.0.0.1:5432 to db.internal:5432 on myhost", "route default via myhost"}
	if err != nil || !up || !reflect.DeepEqual(notes, want) {
		t.Fatalf("Upgrade = %v %v %v", up, notes, err)
	}
	f, err = Load(legacy)
	if err != nil || f.Version != Version || f.Node == nil || f.Node.ID != nodeID || f.Links["myhost"].RemoteGst != "/opt/gst" || f.Net != nil {
		t.Fatalf("upgraded file = %+v, %v", f, err)
	}
	if up, notes, err := Upgrade(legacy); up || notes != nil || err != nil {
		t.Errorf("second Upgrade = %v %v %v", up, notes, err)
	}

	// A file with nothing to drop still counts as upgraded, and keeps the
	// subnet that the user chose.
	bare := filepath.Join(dir, "bare.json")
	os.WriteFile(bare, []byte(`{"version": 3, "links": {}, "net": {"subnet": "10.99.0.0/16", "since": 1}}`), 0o600)
	if up, notes, err := Upgrade(bare); !up || len(notes) != 0 || err != nil {
		t.Errorf("bare Upgrade = %v %v %v", up, notes, err)
	}
	if f, err := Load(bare); err != nil || f.Net == nil || f.Net.Subnet != "10.99.0.0/16" {
		t.Errorf("bare file = %+v, %v", f, err)
	}
}

func TestUpdateConvertsLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(legacyConfig), 0o600)
	if err := Update(path, func(f *File) error { f.Down = true; return nil }); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil || f.Version != Version || !f.Down || len(f.Links) != 1 || f.Node == nil {
		t.Fatalf("Update of a legacy file gave %+v, %v", f, err)
	}
}

func TestValidateName(t *testing.T) {
	for name, ok := range map[string]bool{
		"a": true, "my-host": true, "0box": true, strings.Repeat("a", 32): true,
		"": false, "-a": false, "A": false, "a.b": false, "a_b": false, strings.Repeat("a", 33): false,
	} {
		if (ValidateName(name) == nil) != ok {
			t.Errorf("ValidateName(%q) ok = %v, want %v", name, !ok, ok)
		}
	}
}

func TestNodeIdentity(t *testing.T) {
	a, b := NewNodeID(), NewNodeID()
	if !ValidNodeID(a) || a == b {
		t.Errorf("NewNodeID gave %q and %q", a, b)
	}
	for _, id := range []string{"", "ABCDEF0123456789abcdef0123456789", nodeID + "0", "g123456789abcdef0123456789abcdef"} {
		if ValidNodeID(id) {
			t.Errorf("ValidNodeID(%q) = true", id)
		}
	}
	if err := ValidateName(DefaultNodeName()); err != nil {
		t.Errorf("DefaultNodeName: %v", err)
	}
}

func TestValidateDestination(t *testing.T) {
	for dest, ok := range map[string]bool{
		"myhost": true, "user@myhost": true, "ssh://user@ssh.example.com:2200": true,
		"": false, "-oProxyCommand=x": false, "my host": false, "my\nhost": false,
	} {
		if (ValidateDestination(dest) == nil) != ok {
			t.Errorf("ValidateDestination(%q) ok = %v, want %v", dest, !ok, ok)
		}
	}
}

func TestAbsPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for in, want := range map[string]string{
		"~":          home,
		"~/.ssh/key": filepath.Join(home, ".ssh/key"),
		"/a/b/../c":  "/a/c",
	} {
		if got, err := AbsPath(in); err != nil || got != want {
			t.Errorf("AbsPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if got, err := AbsPath("rel"); err != nil || !filepath.IsAbs(got) {
		t.Errorf("AbsPath(rel) = %q, %v", got, err)
	}
	for _, in := range []string{"", "~other/x", "a\x00b"} {
		if _, err := AbsPath(in); err == nil {
			t.Errorf("AbsPath(%q) succeeded", in)
		}
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if p, err := DefaultPath(); err != nil || p != "/xdg/gst/config.json" {
		t.Errorf("DefaultPath = %q, %v", p, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/me")
	if p, err := DefaultPath(); err != nil || p != "/home/me/.config/gst/config.json" {
		t.Errorf("DefaultPath = %q, %v", p, err)
	}
}
