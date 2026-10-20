package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const validConfig = `{"server":"ssh.example.com:22","user":"tunnel","identity_file":"~/.ssh/id_ed25519","known_hosts_file":"~/.ssh/known_hosts","connect_timeout_seconds":15,"keepalive_interval_seconds":15,"keepalive_count_max":3,"cleanup_listeners":false,"forwards":[{"direction":"remote","listen":"0.0.0.0:2222","target":"127.0.0.1:22"}]}`

func replace(old, new string) string { return strings.Replace(validConfig, old, new, 1) }

func TestParseValid(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Server:                   "ssh.example.com:22",
		User:                     "tunnel",
		IdentityFile:             filepath.Join(home, ".ssh/id_ed25519"),
		KnownHostsFile:           filepath.Join(home, ".ssh/known_hosts"),
		ConnectTimeoutSeconds:    15,
		KeepaliveIntervalSeconds: 15,
		KeepaliveCountMax:        3,
		Forwards:                 []Forward{{Direction: Remote, Listen: "0.0.0.0:2222", Target: "127.0.0.1:22"}},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
	if cfg.ConnectTimeout() != 15*time.Second || cfg.KeepaliveInterval() != 15*time.Second || cfg.KeepaliveTimeout() != 45*time.Second {
		t.Fatal("wrong durations")
	}
	if cfg.Forwards[0].ListenAddr() != netip.MustParseAddrPort("0.0.0.0:2222") {
		t.Fatal("wrong listen address")
	}

	cfg, err = Parse([]byte(replace(`"cleanup_listeners":false,`, ``)))
	if err != nil || cfg.CleanupListeners {
		t.Errorf("missing cleanup_listeners must default to false: %v", err)
	}

	for _, listen := range []string{"[::]:2222", "127.0.0.1:2222"} {
		if _, err := Parse([]byte(replace("0.0.0.0:2222", listen))); err != nil {
			t.Errorf("listen %q: %v", listen, err)
		}
	}

	cfg, err = Parse([]byte(replace("0.0.0.0:2222", "0.0.0.0:02222")))
	if err != nil || cfg.Forwards[0].ListenAddr().Port() != 2222 {
		t.Errorf("leading zero port: %v", err)
	}

	// The two directions bind on different machines, so they may share an address.
	cfg, err = Parse([]byte(replace(`}]`, `},{"direction":"local","listen":"0.0.0.0:2222","target":"db.internal:5432"}]`)))
	if err != nil || cfg.Forwards[1].Direction != Local {
		t.Errorf("local forward: %v", err)
	}
}

func TestParseInvalid(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tests := map[string]string{
		"unknown":               replace(`"user"`, `"extra":true,"user"`),
		"missing":               replace(`"user":"tunnel",`, ``),
		"null":                  replace(`"server":"ssh.example.com:22"`, `"server":null`),
		"wrong type":            replace(`"cleanup_listeners":false`, `"cleanup_listeners":"false"`),
		"empty user":            replace(`"tunnel"`, `""`),
		"user control":          replace(`"tunnel"`, `"tun\nnel"`),
		"zero timeout":          replace(`"connect_timeout_seconds":15`, `"connect_timeout_seconds":0`),
		"negative interval":     replace(`"keepalive_interval_seconds":15`, `"keepalive_interval_seconds":-1`),
		"zero count":            replace(`"keepalive_count_max":3`, `"keepalive_count_max":0`),
		"overflow duration":     replace(`"connect_timeout_seconds":15`, `"connect_timeout_seconds":9223372036854775807`),
		"overflow product":      replace(`"keepalive_count_max":3`, `"keepalive_count_max":9223372036854775807`),
		"float":                 replace(`"connect_timeout_seconds":15`, `"connect_timeout_seconds":1.5`),
		"empty path":            replace(`"~/.ssh/id_ed25519"`, `""`),
		"other home":            replace(`"~/.ssh/id_ed25519"`, `"~other/key"`),
		"nul path":              replace(`"~/.ssh/id_ed25519"`, `"key\u0000"`),
		"missing server port":   replace(`ssh.example.com:22`, `ssh.example.com`),
		"hostname listener":     replace(`0.0.0.0:2222`, `localhost:2222`),
		"empty listener host":   replace(`0.0.0.0:2222`, `:2222`),
		"wildcard listener":     replace(`0.0.0.0:2222`, `*:2222`),
		"zoned listener":        replace(`0.0.0.0:2222`, `[fe80::1%eth0]:2222`),
		"zero port":             replace(`0.0.0.0:2222`, `0.0.0.0:0`),
		"port overflow":         replace(`0.0.0.0:2222`, `0.0.0.0:65536`),
		"signed port":           replace(`0.0.0.0:2222`, `0.0.0.0:+22`),
		"port injection":        replace(`0.0.0.0:2222`, `0.0.0.0:22;id`),
		"target service":        replace(`127.0.0.1:22`, `127.0.0.1:ssh`),
		"target whitespace":     replace(`127.0.0.1:22`, `bad host:22`),
		"target null":           replace(`"127.0.0.1:22"`, `null`),
		"mapping unknown":       replace(`"target"`, `"options":[],"target"`),
		"mapping null":          replace(`}]`, `},null]`),
		"duplicate mapping":     replace(`}]`, `},{"direction":"remote","listen":"0.0.0.0:02222","target":"localhost:80"}]`),
		"duplicate mapped ipv4": replace(`}]`, `},{"direction":"remote","listen":"[::ffff:0.0.0.0]:2222","target":"localhost:80"}]`),
		"missing direction":     replace(`"direction":"remote",`, ``),
		"unknown direction":     replace(`"remote"`, `"both"`),
		"duplicate local":       strings.ReplaceAll(replace(`}]`, `},{"direction":"remote","listen":"0.0.0.0:2222","target":"localhost:80"}]`), `"remote"`, `"local"`),
		"empty mappings":        replace(`[{"direction":"remote","listen":"0.0.0.0:2222","target":"127.0.0.1:22"}]`, `[]`),
		"trailing":              validConfig + `{}`,
		"truncated":             validConfig[:len(validConfig)-1],
		"root null":             `null`,
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(data)); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}

func TestExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tests := map[string]string{
		"~":             home,
		"~/":            home,
		"~/a/b":         filepath.Join(home, "a/b"),
		"relative/key":  "relative/key",
		"/absolute/key": "/absolute/key",
	}
	for path, want := range tests {
		if got, err := expandHome(path); err != nil || got != want {
			t.Errorf("expandHome(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
}

func TestLoadExample(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := Load("../../tunnel.example.json"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	if _, err := Load(path); err == nil || strings.Contains(err.Error(), path) {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}
