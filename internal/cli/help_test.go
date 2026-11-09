package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/yangtau/ssh-tunnel/internal/tun"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := &app{stdout: &stdout, stderr: &stderr}
	if code := a.execute(context.Background(), []string{"version"}); code != exitOK {
		t.Fatalf("version exited %d: %s", code, &stderr)
	}
	var want bytes.Buffer
	built.write(&want)
	if got := stdout.String(); got != want.String() || !strings.HasPrefix(got, "gst "+version+"\n") {
		t.Fatalf("version output = %q, want %q", got, &want)
	}
	if !strings.HasPrefix(software, "gst "+version) {
		t.Fatalf("software = %q, want the release %s", software, version)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", &stderr)
	}

	stdout.Reset()
	if code := a.execute(context.Background(), []string{"version", "--json"}); code != exitOK {
		t.Fatalf("version --json exited %d: %s", code, &stderr)
	}
	var got buildInfo
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil || got != built {
		t.Fatalf("version --json = %s (%v), want %+v", &stdout, err, built)
	}
}

func TestBuildInfo(t *testing.T) {
	const rev = "a2ffc87e0123456789abcdef0123456789abcdef"
	const at = "2026-11-09T08:12:00Z"
	for _, tt := range []struct {
		commit   string
		settings map[string]string
		short    string
		text     string
	}{
		{"", nil, version, ""},
		{"", map[string]string{"vcs.revision": rev, "vcs.modified": "false", "vcs.time": at},
			version + "+a2ffc87", "commit: " + rev + "\ncommitted: " + at + "\n"},
		{"", map[string]string{"vcs.revision": rev, "vcs.modified": "true", "vcs.time": at},
			version + "+a2ffc87.dirty", "commit: " + rev + " (dirty)\ncommitted: " + at + "\n"},
		{rev + "-dirty", map[string]string{"vcs.revision": "ffffffffff"},
			version + "+a2ffc87.dirty", "commit: " + rev + " (dirty)\ncommitted: " + at + "\n"},
	} {
		b := readBuildInfo(tt.commit, at, tt.settings)
		if got := b.short(); got != tt.short {
			t.Errorf("short(%q, %v) = %q, want %q", tt.commit, tt.settings, got, tt.short)
		}
		var out bytes.Buffer
		b.write(&out)
		want := fmt.Sprintf("gst %s\n%sprotocol: %d\nhelper: %d\n", version, tt.text, wire.Protocol, tun.HelperVersion)
		if out.String() != want {
			t.Errorf("write(%q, %v) = %q, want %q", tt.commit, tt.settings, &out, want)
		}
	}
}

func TestVersionRejectsArgumentsAndFlags(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{{"version", "extra"}, {"-v"}, {"--version"}} {
		e.expectCode(exitUsage, args...)
	}
}

func TestColorHelp(t *testing.T) {
	plain := "Description.\n\nInspect tunnels:\n  version     Show the gst version\n\nFlags:\n  -h, --help   help for gst\n      --config string   configuration file\n      wrapped description\n"
	want := "Description.\n\n\x1b[1mInspect tunnels:\x1b[0m\n  \x1b[36mversion\x1b[0m     Show the gst version\n\n\x1b[1mFlags:\x1b[0m\n  \x1b[36m-h, --help\x1b[0m   help for gst\n      \x1b[36m--config string\x1b[0m   configuration file\n      wrapped description\n"
	if got := colorHelp(plain); got != want {
		t.Fatalf("colored help = %q, want %q", got, want)
	}
}

func TestHelpColorPreservesLayout(t *testing.T) {
	e := newEnv(t)
	strip := strings.NewReplacer("\x1b[1m", "", "\x1b[36m", "", "\x1b[0m", "")
	for _, args := range [][]string{{"--help"}, {"help", "add"}, {"subnet", "--help"}, {"add", "--help"}, {"version", "--help"}} {
		plain := e.mustGst(args...)
		if strings.Contains(plain, "\x1b[") {
			t.Fatalf("gst %v: redirected help contains escape codes", args)
		}
		colored := colorHelp(plain)
		if !strings.Contains(colored, "\x1b[1mUsage:\x1b[0m") || !strings.Contains(colored, "\x1b[36m-h, --help\x1b[0m") {
			t.Errorf("gst %v: missing help styles: %q", args, colored)
		}
		if strip.Replace(colored) != plain {
			t.Errorf("gst %v: coloring changed the help layout", args)
		}
	}
}

func TestHelpUsesColorForNonTerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	if helpUsesColor(&bytes.Buffer{}) {
		t.Fatal("color enabled for a buffer")
	}
	file, err := os.CreateTemp(t.TempDir(), "help")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if helpUsesColor(file) {
		t.Fatal("color enabled for a regular file")
	}
}

func TestHelpColorDisabledByEnvironment(t *testing.T) {
	for _, env := range []string{"NO_COLOR", "TERM"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("NO_COLOR", "")
			t.Setenv("TERM", "xterm-256color")
			value := "1"
			if env == "TERM" {
				value = "dumb"
			}
			t.Setenv(env, value)
			if helpUsesColor(os.Stdout) {
				t.Fatal("color enabled despite " + env)
			}
		})
	}
}
