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

func validTunnel() *Tunnel {
	return &Tunnel{
		Destination: "myhost",
		Forwards: []Forward{
			{Direction: Remote, Listen: "0.0.0.0:2222", Target: "127.0.0.1:22"},
			{Direction: Local, Listen: "0.0.0.0:2222", Target: "db.internal:5432"},
		},
	}
}

func TestUpdateAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gst", "config.json")
	f, err := Load(path)
	if err != nil || len(f.Tunnels) != 0 {
		t.Fatalf("missing file must load as empty: %v", err)
	}

	err = Update(path, func(f *File) error {
		f.Tunnels["myhost"] = validTunnel()
		f.Tunnels["bare"] = &Tunnel{Destination: "ssh://u@192.0.2.1:2200"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config not private: %v %v", info.Mode(), err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"forwards": []`) {
		t.Fatalf("forwards of a bare tunnel must be written as []:\n%s", data)
	}
	f, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Tunnels["myhost"], validTunnel()) || len(f.Tunnels) != 2 {
		t.Fatalf("round trip changed the config: %+v", f.Tunnels)
	}

	before, _ := os.ReadFile(path)
	if err := Update(path, func(f *File) error { return fmt.Errorf("refused") }); err == nil {
		t.Fatal("fn error ignored")
	}
	if err := Update(path, func(f *File) error {
		f.Tunnels["Bad"] = validTunnel()
		return nil
	}); err == nil {
		t.Fatal("invalid result written")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("failed update changed the file")
	}
}

func TestConcurrentUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			if err := Update(path, func(f *File) error {
				f.Tunnels[fmt.Sprintf("t%d", i)] = &Tunnel{Destination: "myhost"}
				return nil
			}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	f, err := Load(path)
	if err != nil || len(f.Tunnels) != 20 {
		t.Fatalf("lost updates: %d tunnels, %v", len(f.Tunnels), err)
	}
}

func TestParseInvalid(t *testing.T) {
	const tunnel = `{"destination":"myhost","forwards":[{"direction":"remote","listen":"0.0.0.0:2222","target":"127.0.0.1:22"}]}`
	doc := func(name, tunnel string) string { return `{"tunnels":{"` + name + `":` + tunnel + `}}` }
	replace := func(old, new string) string { return doc("myhost", strings.Replace(tunnel, old, new, 1)) }
	tests := map[string]string{
		"unknown field":       replace(`"destination"`, `"extra":1,"destination"`),
		"trailing data":       doc("myhost", tunnel) + `{}`,
		"null tunnel":         doc("myhost", "null"),
		"uppercase name":      doc("MyHost", tunnel),
		"long name":           doc(strings.Repeat("a", 33), tunnel),
		"name with dot":       doc("a.b", tunnel),
		"empty destination":   replace(`"myhost"`, `""`),
		"option destination":  replace(`"myhost"`, `"-oProxyCommand=x"`),
		"space destination":   replace(`"myhost"`, `"a b"`),
		"relative identity":   replace(`"destination"`, `"identity_file":"key","destination"`),
		"negative timeout":    replace(`"destination"`, `"connect_timeout_seconds":-1,"destination"`),
		"huge interval":       replace(`"destination"`, `"keepalive_interval_seconds":9223372036,"destination"`),
		"huge count":          replace(`"destination"`, `"keepalive_count_max":1001,"destination"`),
		"bad direction":       replace(`"remote"`, `"both"`),
		"hostname listener":   replace(`0.0.0.0:2222`, `localhost:2222`),
		"bad target":          replace(`127.0.0.1:22`, `127.0.0.1:ssh`),
		"duplicate listener":  replace(`}]`, `},{"direction":"remote","listen":"0.0.0.0:02222","target":"h:1"}]`),
		"duplicate mapped v4": replace(`}]`, `},{"direction":"remote","listen":"[::ffff:0.0.0.0]:2222","target":"h:1"}]`),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parse([]byte(data)); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
	if _, err := parse([]byte(doc("myhost", tunnel))); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestParseForward(t *testing.T) {
	tests := map[string]Forward{
		"2222:localhost:22":           {Remote, "127.0.0.1:2222", "localhost:22"},
		"localhost:2222:host:22":      {Remote, "127.0.0.1:2222", "host:22"},
		"*:8080:localhost:3000":       {Remote, "0.0.0.0:8080", "localhost:3000"},
		":8080:localhost:3000":        {Remote, "0.0.0.0:8080", "localhost:3000"},
		"0.0.0.0:2222:127.0.0.1:22":   {Remote, "0.0.0.0:2222", "127.0.0.1:22"},
		"[::1]:2222:[::1]:22":         {Remote, "[::1]:2222", "[::1]:22"},
		"[::]:2222:db.example.com:22": {Remote, "[::]:2222", "db.example.com:22"},
	}
	for spec, want := range tests {
		got, err := ParseForward(Remote, spec)
		if err != nil || got != want {
			t.Errorf("ParseForward(%q) = %+v, %v; want %+v", spec, got, err, want)
			continue
		}
		again, err := ParseForward(Remote, strings.TrimPrefix(got.Spec(), "-R "))
		if err != nil || again != got {
			t.Errorf("Spec %q does not round-trip: %+v, %v", got.Spec(), again, err)
		}
	}
	for _, spec := range []string{"2222", "2222:host", "a:1:b:2:c", "host:2222:h:22", "0:h:22", "65536:h:22", "2222:h:ssh", "2222:bad host:22", "2222::22", "[fe80::1%en0]:2222:h:22"} {
		if f, err := ParseForward(Local, spec); err == nil {
			t.Errorf("ParseForward(%q) accepted: %+v", spec, f)
		}
	}
}

func TestParseListener(t *testing.T) {
	tests := map[string]Forward{
		"2222":                      {Local, "127.0.0.1:2222", ""},
		"*:2222":                    {Local, "0.0.0.0:2222", ""},
		"[::]:2222":                 {Local, "[::]:2222", ""},
		"0.0.0.0:2222:127.0.0.1:22": {Local, "0.0.0.0:2222", "127.0.0.1:22"},
	}
	for spec, want := range tests {
		if got, err := ParseListener(Local, spec); err != nil || got != want {
			t.Errorf("ParseListener(%q) = %+v, %v; want %+v", spec, got, err, want)
		}
	}
	if _, err := ParseListener(Local, "host:2222"); err == nil {
		t.Error("hostname listener accepted")
	}

	mapped := Forward{Direction: Local, Listen: "[::ffff:127.0.0.1]:2222"}
	if !mapped.SameListener(tests["2222"]) || mapped.SameListener(Forward{Direction: Remote, Listen: "127.0.0.1:2222"}) {
		t.Error("SameListener must unmap addresses and compare directions")
	}
}

func TestPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if p, err := DefaultPath(); err != nil || p != filepath.Join(home, ".config/gst/config.json") {
		t.Errorf("DefaultPath() = %q, %v", p, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if p, _ := DefaultPath(); p != "/xdg/gst/config.json" {
		t.Errorf("DefaultPath() = %q with XDG_CONFIG_HOME", p)
	}

	wd, _ := os.Getwd()
	for path, want := range map[string]string{
		"~":        home,
		"~/a/b":    filepath.Join(home, "a/b"),
		"/abs/key": "/abs/key",
		"rel/key":  filepath.Join(wd, "rel/key"),
	} {
		if got, err := AbsPath(path); err != nil || got != want {
			t.Errorf("AbsPath(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	for _, path := range []string{"", "~other/key", "a\x00b"} {
		if _, err := AbsPath(path); err == nil {
			t.Errorf("AbsPath(%q) accepted", path)
		}
	}
}
