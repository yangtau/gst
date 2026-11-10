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
	const ok = `"version": 6`
	other := strings.Replace(nodeID, "0", "1", 1)
	tests := []struct {
		name string
		body string
		want string // substring of the error; empty when valid
	}{
		{"empty", ok, ""},
		{"node", ok + `, "node": {"id": "` + nodeID + `", "name": "laptop", "generation": 2}`, ""},
		{"node bad id", ok + `, "node": {"id": "xyz", "name": "laptop"}`, "invalid node"},
		{"node bad name", ok + `, "node": {"id": "` + nodeID + `", "name": "Laptop"}`, "invalid node"},
		{"node bad generation", ok + `, "node": {"id": "` + nodeID + `", "name": "laptop", "generation": -1}`, "invalid node"},
		{"link", ok + `, "links": [{"destination": "user@myhost", "ssh_args": ["-p", "2222"], "target": "` + nodeID + `", "admit": true}]`, ""},
		{"link removed field", ok + `, "links": [{"destination": "myhost", "remote_gst": "/opt/gst"}]`, "unknown field"},
		{"link empty ssh argument", ok + `, "links": [{"destination": "myhost", "ssh_args": [""]}]`, "ssh argument"},
		{"link control in ssh argument", ok + `, "links": [{"destination": "myhost", "ssh_args": ["a\nb"]}]`, "ssh argument"},
		{"link bad target", ok + `, "links": [{"destination": "myhost", "target": "xyz"}]`, "target"},
		{"links to one machine", ok + `, "links": [{"destination": "a", "target": "` + nodeID + `"}, {"destination": "b", "target": "` + nodeID + `"}]`, "same machine"},
		{"links to two machines", ok + `, "links": [{"destination": "a", "target": "` + nodeID + `"}, {"destination": "b", "target": "` + other + `"}, {"destination": "c"}, {"destination": "d"}]`, ""},
		{"link option destination", ok + `, "links": [{"destination": "-oProxyCommand=x"}]`, "link 1"},
		{"version 5", `"version": 5, "links": [{"destination": "myhost", "remote_gst": "/opt/gst"}], "net": {"subnet": "10.99.0.0/16", "since": 1}`, ""},
		{"version 5 link", `"version": 5, "links": [{"destination": "-oProxyCommand=x"}]`, "link 1"},
		{"subnet is the network's", ok + `, "net": {"subnet": "10.99.0.0/16", "since": 1}`, "unknown field"},
		{"proxy is gone", ok + `, "proxy": "127.0.0.1:1"`, "unknown field"},
		{"unknown field", ok + `, "bogus": 1`, "unknown field"},
		{"future version", `"version": 7`, "unsupported config version"},
		{"old version", `"version": 4`, "too old"},
		{"no version", ``, "too old"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := parse([]byte("{" + tt.body + "}"))
			if tt.want == "" {
				if err != nil {
					t.Fatalf("parse: %v", err)
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
	if _, err := parse([]byte("not json")); err == nil {
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
	os.WriteFile(path, []byte(`{"version": 6, "links": [{"destination": "myhost"}, {"destination": "secret host"}]}`), 0o600)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "link 2") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestLoadDuplicateDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"version": 6, "links": [{"destination": "myhost"}, {"destination": "myhost"}]}`), 0o600)
	if _, err := Load(path); err == nil || strings.Contains(err.Error(), "myhost\"") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestLoadVersion5(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"version": 5, "down": true, "links": [
		{"destination": "deploy@ssh.example.com", "remote_gst": "/opt/gst"},
		{"destination": "myhost"}
	], "net": {"subnet": "10.99.0.0/16", "since": 1}}`), 0o600)
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []Link{{Destination: "deploy@ssh.example.com"}, {Destination: "myhost"}}
	if f.Version != Version || !f.Down || !reflect.DeepEqual(f.Links, want) {
		t.Fatalf("Load = %+v, want version %d, down, and links %+v", f, Version, want)
	}
}

func TestLinkID(t *testing.T) {
	a, b := LinkID("myhost"), LinkID("user@myhost")
	if !ValidLinkID(a) || !ValidLinkID(b) || a == b || a != LinkID("myhost") || strings.Contains(a, "myhost") {
		t.Fatalf("LinkID = %q, %q", a, b)
	}
}

func TestUpdateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gst", "config.json")
	err := Update(path, func(f *File) error {
		f.Links = append(f.Links, Link{Destination: "ssh://user@ssh.example.com:2200", SSHArgs: []string{"-i", "/keys/gst"}, Target: nodeID, Admit: true})
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
		Links: []Link{{Destination: "ssh://user@ssh.example.com:2200", SSHArgs: []string{"-i", "/keys/gst"}, Target: nodeID, Admit: true}},
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
		"invalid":  func(f *File) error { f.Links = append(f.Links, Link{Destination: "-oProxyCommand=x"}); return nil },
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
				f.Links = append(f.Links, Link{Destination: fmt.Sprintf("host%d", i)})
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
	for text, want := range map[string]string{"Mac-Mini": "mac-mini", "My iPhone": "my-iphone", "Tablet (2)": "tablet-2", "": "node", "手机": "node"} {
		if got := NodeName(text); got != want {
			t.Errorf("NodeName(%q) = %q, want %q", text, got, want)
		}
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
