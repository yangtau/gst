package service

import (
	"errors"
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

func TestSystemd(t *testing.T) {
	fake := &fakeRunner{respond: func(call string) (string, error) {
		if strings.HasPrefix(call, "loginctl") {
			return "no\n", nil
		}
		return "", nil
	}}
	var notes strings.Builder
	s := &systemd{dir: t.TempDir(), run: fake.run, notes: &notes}

	if err := s.Uninstall(); err != nil || len(fake.calls) != 0 {
		t.Fatalf("uninstalling a missing service ran %v: %v", fake.calls, err)
	}

	command := []string{"/opt/my bin/gst", "--config", `/c/100%$HOME"x`, "daemon"}
	if err := s.Install(command, false); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(filepath.Join(s.dir, "gst.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), `ExecStart="/opt/my bin/gst" "--config" "/c/100%%$$HOME\"x" "daemon"`) {
		t.Fatalf("unexpected unit:\n%s", unit)
	}
	wantCalls := []string{"systemctl --user daemon-reload", "systemctl --user enable gst.service", "systemctl --user restart gst.service"}
	if !slices.Equal(fake.calls[:3], wantCalls) || !strings.Contains(notes.String(), "loginctl enable-linger") {
		t.Fatalf("unexpected calls %v, notes %q", fake.calls, notes.String())
	}

	// An unchanged unit only makes sure the service runs.
	fake.calls = nil
	if err := s.Install(command, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.calls[:2], []string{"systemctl --user enable gst.service", "systemctl --user start gst.service"}) {
		t.Fatalf("unexpected calls %v", fake.calls)
	}
	fake.calls = nil
	if err := s.Install(command, true); err != nil || fake.calls[1] != "systemctl --user restart gst.service" {
		t.Fatalf("forced restart ran %v: %v", fake.calls, err)
	}

	fake.calls = nil
	if err := s.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.calls, []string{"systemctl --user disable --now gst.service", "systemctl --user daemon-reload"}) {
		t.Fatalf("unexpected calls %v", fake.calls)
	}
}

func TestLaunchd(t *testing.T) {
	home := t.TempDir()
	l := newLaunchd(home, 501)
	l.poll = 0
	loaded := map[string]bool{}
	printsLeft := 0 // loaded prints still answered after bootout
	fake := &fakeRunner{respond: func(call string) (string, error) {
		fields := strings.Fields(call)
		target := fields[len(fields)-1]
		switch {
		case strings.HasPrefix(call, "launchctl print"):
			if !loaded[target] {
				return "", errors.New("not loaded")
			}
			if printsLeft--; printsLeft == 0 {
				delete(loaded, target)
			}
			return "state = running\n", nil
		case strings.HasPrefix(call, "launchctl bootstrap"):
			label := strings.TrimSuffix(filepath.Base(target), ".plist")
			loaded["gui/501/"+label] = true
			printsLeft = -1
		case strings.HasPrefix(call, "launchctl bootout"):
			printsLeft = 2
		}
		return "", nil
	}}
	l.run = fake.run

	if err := l.Install([]string{"/bin/gst", "--config", "a<b&c", "daemon"}, false); err != nil {
		t.Fatal(err)
	}
	plistPath := filepath.Join(home, "Library/LaunchAgents/local.gst.plist")
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(home, "Library/Logs/gst-daemon.log")
	if !strings.Contains(string(plist), "<string>a&lt;b&amp;c</string>\n\t\t<string>daemon</string>\n\t\t<string>--log-file</string>\n\t\t<string>"+logPath+"</string>\n\t</array>") {
		t.Fatalf("unexpected plist:\n%s", plist)
	}
	want := []string{"launchctl enable gui/501/local.gst", "launchctl print gui/501/local.gst", "launchctl bootstrap gui/501 " + plistPath}
	if !slices.Equal(fake.calls, want) {
		t.Fatalf("unexpected calls %v", fake.calls)
	}

	// An unchanged, loaded agent stays as it is unless restart is set.
	fake.calls = nil
	if err := l.Install([]string{"/bin/gst", "--config", "a<b&c", "daemon"}, false); err != nil || len(fake.calls) != 1 {
		t.Fatalf("unchanged install ran %v: %v", fake.calls, err)
	}
	fake.calls = nil
	if err := l.Install([]string{"/bin/gst", "--config", "a<b&c", "daemon"}, true); err != nil || fake.calls[1] != "launchctl kickstart -k gui/501/local.gst" {
		t.Fatalf("forced restart ran %v: %v", fake.calls, err)
	}

	// A changed command reloads the agent and waits until it is gone.
	fake.calls = nil
	if err := l.Install([]string{"/usr/bin/gst", "daemon"}, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(fake.calls, "launchctl bootout gui/501/local.gst") || !strings.HasPrefix(fake.calls[len(fake.calls)-1], "launchctl bootstrap") {
		t.Fatalf("unexpected calls %v", fake.calls)
	}

	printsLeft = 1
	if err := l.Uninstall(); err != nil || fileExists(plistPath) {
		t.Fatalf("agent remains after uninstall: %v", err)
	}
}
