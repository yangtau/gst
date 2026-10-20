package service

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeRunner records commands and answers them with respond.
type fakeRunner struct {
	calls   []string
	respond func(call string) (string, error)
}

func (f *fakeRunner) run(name string, args ...string) (string, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	if f.respond == nil {
		return "", nil
	}
	return f.respond(call)
}

func (f *fakeRunner) stream(_ context.Context, w io.Writer, name string, args ...string) error {
	f.run(name, args...)
	return nil
}

func TestSystemd(t *testing.T) {
	fake := &fakeRunner{respond: func(call string) (string, error) {
		switch {
		case strings.HasPrefix(call, "loginctl"):
			return "no\n", nil
		case strings.Contains(call, "ActiveState"):
			return "activating\n", nil
		}
		return "", nil
	}}
	var notes strings.Builder
	s := &systemd{dir: t.TempDir(), run: fake.run, stream: fake.stream, notes: &notes}

	if status, _ := s.Status("myhost"); status != Stopped {
		t.Fatalf("status %q before start", status)
	}
	if err := s.Stop("myhost"); err != nil || len(fake.calls) != 0 {
		t.Fatalf("stopping a missing service ran %v: %v", fake.calls, err)
	}

	if err := s.Start("myhost", []string{"/opt/my bin/gst", "--config", `/c/100%$HOME"x`, "run", "myhost"}); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(filepath.Join(s.dir, "gst-myhost.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), `ExecStart="/opt/my bin/gst" "--config" "/c/100%%$$HOME\"x" "run" "myhost"`) {
		t.Fatalf("unexpected unit:\n%s", unit)
	}
	wantCalls := []string{"systemctl --user daemon-reload", "systemctl --user enable gst-myhost.service", "systemctl --user restart gst-myhost.service"}
	if !slices.Equal(fake.calls[:3], wantCalls) || !strings.Contains(notes.String(), "loginctl enable-linger") {
		t.Fatalf("unexpected calls %v, notes %q", fake.calls, notes.String())
	}

	if status, _ := s.Status("myhost"); status != Down {
		t.Fatalf("status %q, want down", status)
	}
	fake.calls = nil
	if err := s.Logs(context.Background(), "myhost", 20, true, io.Discard); err != nil || !strings.Contains(fake.calls[0], "--unit gst-myhost.service --lines 20") || !strings.HasSuffix(fake.calls[0], "--follow") {
		t.Fatalf("unexpected logs call %v: %v", fake.calls, err)
	}

	fake.calls = nil
	if err := s.Stop("myhost"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.calls, []string{"systemctl --user disable --now gst-myhost.service", "systemctl --user daemon-reload"}) {
		t.Fatalf("unexpected calls %v", fake.calls)
	}
	if status, _ := s.Status("myhost"); status != Stopped {
		t.Fatalf("status %q after stop", status)
	}
}

func TestLaunchd(t *testing.T) {
	home := t.TempDir()
	l := newLaunchd(home, 501)
	l.poll = 0
	loaded := false
	printsLeft := 0 // loaded prints still answered after bootout
	fake := &fakeRunner{respond: func(call string) (string, error) {
		switch {
		case strings.HasPrefix(call, "launchctl print"):
			if !loaded {
				return "", errors.New("not loaded")
			}
			if printsLeft--; printsLeft == 0 {
				loaded = false
			}
			return "gui/501/local.gst.myhost = {\n\tstate = running\n\tprogram = /bin/gst\n}\n", nil
		case strings.HasPrefix(call, "launchctl bootstrap"):
			loaded = true
			printsLeft = -1
		case strings.HasPrefix(call, "launchctl bootout"):
			printsLeft = 2
		}
		return "", nil
	}}
	l.run, l.stream = fake.run, fake.stream

	if err := l.Start("myhost", []string{"/bin/gst", "run", "a<b&c"}); err != nil {
		t.Fatal(err)
	}
	plist, err := os.ReadFile(filepath.Join(home, "Library/LaunchAgents/local.gst.myhost.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plist), "<string>a&lt;b&amp;c</string>") || !strings.Contains(string(plist), filepath.Join(home, "Library/Logs/gst/myhost.log")) {
		t.Fatalf("unexpected plist:\n%s", plist)
	}
	want := []string{"launchctl enable gui/501/local.gst.myhost", "launchctl print gui/501/local.gst.myhost", "launchctl bootstrap gui/501 " + filepath.Join(home, "Library/LaunchAgents/local.gst.myhost.plist")}
	if !slices.Equal(fake.calls, want) {
		t.Fatalf("unexpected calls %v", fake.calls)
	}
	if status, _ := l.Status("myhost"); status != Running {
		t.Fatalf("status %q, want running", status)
	}

	// Starting a loaded agent reloads it and waits until it is gone.
	fake.calls = nil
	if err := l.Start("myhost", []string{"/bin/gst", "run", "myhost"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(fake.calls, "launchctl bootout gui/501/local.gst.myhost") || !strings.HasPrefix(fake.calls[len(fake.calls)-1], "launchctl bootstrap") {
		t.Fatalf("unexpected calls %v", fake.calls)
	}

	printsLeft = 1
	if err := l.Stop("myhost"); err != nil {
		t.Fatal(err)
	}
	if status, _ := l.Status("myhost"); status != Stopped {
		t.Fatalf("status %q after stop", status)
	}
	fake.calls = nil
	if err := l.Logs(context.Background(), "myhost", 5, false, io.Discard); err != nil || len(fake.calls) != 0 {
		t.Fatalf("logs without a log file ran %v: %v", fake.calls, err)
	}
}
