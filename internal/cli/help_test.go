package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := &app{stdout: &stdout, stderr: &stderr}
	if code := a.execute(context.Background(), []string{"version"}); code != exitOK {
		t.Fatalf("version exited %d: %s", code, &stderr)
	}
	if got, want := stdout.String(), "gst version "+version+"\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", &stderr)
	}
}

func TestVersionRejectsArgumentsAndFlags(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{{"version", "extra"}, {"version", "--json"}, {"-v"}, {"--version"}} {
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
	for _, args := range [][]string{{"--help"}, {"help", "add"}, {"forward"}, {"forward", "add", "--help"}, {"version", "--help"}} {
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
