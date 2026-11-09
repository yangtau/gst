package cli

import (
	"encoding/json"
	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
	"strings"
	"testing"
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
	if code, _, err := e.gst("expose", "9090", "--port", "5999"); code != 1 || !strings.Contains(err, "gst expose remove 5999") {
		t.Fatalf("conflict: %d %s", code, err)
	}
	code, out, errText := e.gst("expose", "list", "--json")
	var states []wire.ExposeState
	if code != 0 || json.Unmarshal([]byte(out), &states) != nil || len(states) != 3 {
		t.Fatalf("list: %d %s %s", code, out, errText)
	}
	if code, _, err := e.gst("expose", "remove", "5999", "--udp"); code != 0 {
		t.Fatalf("remove: %d %s", code, err)
	}
	f, _ = config.Load(e.config)
	if len(f.Exposes) != 2 || f.Exposes[0].Protocol != "tcp" {
		t.Fatal(f.Exposes)
	}
	if code, _, err := e.gst("down"); code != 0 {
		t.Fatalf("down: %d %s", code, err)
	}
	if code, out, err := e.gst("expose", "list"); code != 0 || !strings.Contains(out, "stopped") {
		t.Fatalf("offline list: %d %s %s", code, out, err)
	}
	if code, _, err := e.gst("expose", "remove", "5999"); code != 0 {
		t.Fatalf("offline remove: %d %s", code, err)
	}
	if len(e.rootRuns) != 0 {
		t.Fatal("expose requested root")
	}
}

func TestExposeUsage(t *testing.T) {
	for _, args := range [][]string{
		{"expose"}, {"expose", "0"}, {"expose", "8080", "--port", "0"},
		{"expose", "unix:///tmp/socket"}, {"expose", "unix:///tmp/socket", "--port", "5999", "--udp"},
		{"expose", "remove"}, {"expose", "remove", "65536"},
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
	if out := e.mustGst("status"); !strings.Contains(out, "expose tcp/5999: failed") {
		t.Fatal(out)
	}
}
