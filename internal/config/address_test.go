package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseListen(t *testing.T) {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	tests := []struct {
		spec, want string // want is empty when the spec is invalid
	}{
		{"8080", "127.0.0.1:8080"},
		{"localhost:8080", "127.0.0.1:8080"},
		{"127.0.0.2:8080", "127.0.0.2:8080"},
		{"*:8080", "0.0.0.0:8080"},
		{":8080", "0.0.0.0:8080"},
		{"0.0.0.0:8080", "0.0.0.0:8080"},
		{"[::1]:8080", "[::1]:8080"},
		{"[::]:8080", "[::]:8080"},
		{"65535", "127.0.0.1:65535"},
		{"0", ""},
		{"65536", ""},
		{"-1", ""},
		{"", ""},
		{"http", ""},
		{"myhost:8080", ""},
		{"[fe80::1%eth0]:80", ""},
		{"/run/app.sock", "/run/app.sock"},
		{"/run//app/../app.sock", "/run/app.sock"},
		{"~/app.sock", filepath.Join(home, "app.sock")},
		{"rel/app.sock", filepath.Join(cwd, "rel/app.sock")},
		{"/run/\x01.sock", ""},
	}
	for _, tt := range tests {
		got, err := ParseListen(tt.spec)
		if tt.want == "" {
			if err == nil {
				t.Errorf("ParseListen(%q) = %q, want an error", tt.spec, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseListen(%q) = %q, %v; want %q", tt.spec, got, err, tt.want)
		}
	}
}

func TestParseTarget(t *testing.T) {
	tests := []struct {
		spec, want string
	}{
		{"5432", "localhost:5432"},
		{"db.internal:5432", "db.internal:5432"},
		{"DB.Internal.:5432", "DB.Internal.:5432"},
		{"10.0.0.1:80", "10.0.0.1:80"},
		{"[::1]:80", "[::1]:80"},
		{"/run/app.sock", "/run/app.sock"},
		{"/run//app/../app.sock", "/run/app.sock"},
		{"db:0", ""},
		{"db:65536", ""},
		{"db:ssh", ""},
		{":80", ""},
		{"-bad:80", ""},
		{"bad_host:80", ""},
		{"a b:80", ""},
		{"", ""},
		{"abc", ""}, // a bare word is a port, and not a number
		{"rel.sock", ""},
		{"::1:80", ""},
	}
	for _, tt := range tests {
		got, err := ParseTarget(tt.spec)
		if tt.want == "" {
			if err == nil {
				t.Errorf("ParseTarget(%q) = %q, want an error", tt.spec, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseTarget(%q) = %q, %v; want %q", tt.spec, got, err, tt.want)
		}
	}
}

func TestSocketHelpers(t *testing.T) {
	for addr, want := range map[string]bool{"/run/a.sock": true, "127.0.0.1:1": false, "": false, "run/a.sock": false} {
		if IsSocket(addr) != want {
			t.Errorf("IsSocket(%q) = %v", addr, !want)
		}
		wantNet := "tcp"
		if want {
			wantNet = "unix"
		}
		if Network(addr) != wantNet {
			t.Errorf("Network(%q) = %q, want %q", addr, Network(addr), wantNet)
		}
	}
}

func TestValidTarget(t *testing.T) {
	tests := []struct {
		network, address string
		want             bool
	}{
		{"tcp", "db.internal:5432", true},
		{"tcp", "127.0.0.1:1", true},
		{"tcp", "[::1]:1", true},
		{"tcp", "db:1", true},
		{"tcp", "db", false},
		{"tcp", "db:0", false},
		{"tcp", "/run/a.sock", false},
		{"unix", "/run/a.sock", true},
		{"unix", "/run/../a.sock", false}, // not clean
		{"unix", "/run/a.sock/", false},
		{"unix", "a.sock", false},
		{"unix", "/run/\n.sock", false},
		{"unix", "db:1", false},
		{"udp", "db:1", false},
		{"", "db:1", false},
	}
	for _, tt := range tests {
		if got := ValidTarget(tt.network, tt.address); got != tt.want {
			t.Errorf("ValidTarget(%q, %q) = %v, want %v", tt.network, tt.address, got, tt.want)
		}
	}
}
