package cli

import (
	"strings"
	"testing"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func TestExposeCommands(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{
		{"expose", "8080", "--port", "5999"},
		{"expose", "8080", "--port", "5999"},
		{"expose", "5353", "--port", "5999", "--udp"},
		{"expose", "unix:///tmp/service.sock", "--port", "6000"},
	} {
		if code, out, err := e.gst(args...); code != 0 {
			t.Fatalf("%v: %d %s %s", args, code, out, err)
		}
	}
	f, err := config.Load(e.config)
	if err != nil || len(f.Exposes) != 3 {
		t.Fatalf("%+v %v", f, err)
	}
	if code, _, err := e.gst("expose", "9090", "--port", "5999"); code != 1 || !strings.Contains(err, "hint: unexpose it first with `gst unexpose 5999`") {
		t.Fatalf("conflict: %d %s", code, err)
	}
	code, out, errText := e.gst("expose")
	if code != 0 || len(rows(out)) != 4 || !strings.Contains(out, "unix:///tmp/service.sock") {
		t.Fatalf("list: %d %s %s", code, out, errText)
	}
	if out := e.mustGst("unexpose", "5999", "--udp"); out != "Unexposed udp/5999.\n" {
		t.Fatalf("unexpose: %q", out)
	}
	f, _ = config.Load(e.config)
	if len(f.Exposes) != 2 || f.Exposes[0].Protocol != "tcp" {
		t.Fatal(f.Exposes)
	}
	if code, _, err := e.gst("down"); code != 0 {
		t.Fatalf("down: %d %s", code, err)
	}
	if code, out, err := e.gst("expose"); code != 0 || !strings.Contains(out, "stopped") {
		t.Fatalf("offline list: %d %s %s", code, out, err)
	}
	if code, _, err := e.gst("unexpose", "5999"); code != 0 {
		t.Fatalf("offline remove: %d %s", code, err)
	}
	if len(e.rootRuns) != 0 {
		t.Fatal("expose requested root")
	}
	e.expectError(exitFailure, []string{"unexpose", "5999"}, "error: tcp/5999 is not exposed\n", "hint: see the exposed ports with `gst expose`\n")
	if out := e.mustGst("unexpose", "6000"); out != "Unexposed tcp/6000.\n" {
		t.Fatalf("unexpose: %q", out)
	}
	if out := e.mustGst("expose"); out != "No exposed ports. Expose one with `gst expose <target>`.\n" {
		t.Fatalf("empty list: %q", out)
	}
}

func TestExposeUsage(t *testing.T) {
	for _, args := range [][]string{
		{"expose", "--udp"}, {"expose", "--port", "5999"}, {"expose", "0"}, {"expose", "8080", "--port", "0"},
		{"expose", "unix:///tmp/socket"}, {"expose", "unix:///tmp/socket", "--port", "5999", "--udp"},
		{"expose", "8080", "9090"}, {"unexpose"}, {"unexpose", "65536"}, {"unexpose", "5999", "6000"},
	} {
		e := newEnv(t)
		if code, _, err := e.gst(args...); code != 2 || !strings.Contains(err, "usage:") {
			t.Fatalf("%v: %d %s", args, code, err)
		}
		if len(e.back.calls) != 0 {
			t.Fatal("invalid command touched daemon")
		}
	}
}

func TestExposeBindFailureIsReportedAndRetained(t *testing.T) {
	e := newEnv(t)
	e.back.exposes = []wire.ExposeState{{Protocol: "tcp", Port: 5999, Target: "tcp://127.0.0.1:8080", State: "failed", Error: "address already in use"}}
	code, _, err := e.gst("expose", "8080", "--port", "5999")
	if code != 1 || !strings.Contains(err, "address already in use") {
		t.Fatalf("%d %s", code, err)
	}
	if len(e.load().Exposes) != 1 {
		t.Fatal("failed rule was not retained")
	}
	if out := e.mustGst("status"); !strings.Contains(out, "exposed tcp/5999: failed") {
		t.Fatal(out)
	}
}
