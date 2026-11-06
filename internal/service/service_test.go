package service

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
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

func TestSocket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	got, err := Socket()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".local", "state", "gst", "gst.sock")
	if runtime.GOOS == "darwin" {
		want = filepath.Join(home, "Library", "Application Support", "gst", "gst.sock")
	} else {
		state := t.TempDir()
		t.Setenv("XDG_STATE_HOME", state)
		if got, err := Socket(); err != nil || got != filepath.Join(state, "gst", "gst.sock") {
			t.Fatalf("with XDG_STATE_HOME: %q, %v", got, err)
		}
	}
	if got != want {
		t.Fatalf("Socket() = %q, want %q", got, want)
	}
}

func TestHelperOnSystemd(t *testing.T) {
	fake := &fakeRunner{}
	dir := t.TempDir()
	exe := filepath.Join(dir, "gst")
	if err := os.WriteFile(exe, []byte("program"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := &systemd{dir: filepath.Join(dir, "system"), system: true, run: fake.run}
	h := &Helper{binary: filepath.Join(dir, "libexec", "gst-net"), manager: systemdHelper{s}}

	if err := h.Install(exe, "alice"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(h.binary)
	if err != nil || string(got) != "program" {
		t.Fatalf("copy: %q, %v", got, err)
	}
	info, err := os.Stat(h.binary)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("copy: %v, %v", info, err)
	}
	unit, err := os.ReadFile(filepath.Join(s.dir, helperUnit))
	if err != nil {
		t.Fatal(err)
	}
	want := `ExecStart="` + h.binary + `" "net-helper" "--user" "alice"`
	if !strings.Contains(string(unit), want) || !strings.Contains(string(unit), "WantedBy=multi-user.target") {
		t.Fatalf("unexpected unit:\n%s", unit)
	}
	wantCalls := []string{"systemctl daemon-reload", "systemctl enable gst-net.service", "systemctl restart gst-net.service"}
	if !slices.Equal(fake.calls, wantCalls) {
		t.Fatalf("calls: %q", fake.calls)
	}

	// Installing a newer gst replaces the copy, and restarts the service
	// with it even though the unit stays the same.
	if err := os.WriteFile(exe, []byte("newer"), 0o700); err != nil {
		t.Fatal(err)
	}
	fake.calls = nil
	if err := h.Install(exe, "alice"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(h.binary); string(got) != "newer" {
		t.Fatalf("copy: %q", got)
	}
	if !slices.Equal(fake.calls, []string{"systemctl enable gst-net.service", "systemctl restart gst-net.service"}) {
		t.Fatalf("calls: %q", fake.calls)
	}

	fake.calls = nil
	if err := h.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if fileExists(h.binary) || fileExists(filepath.Join(s.dir, helperUnit)) {
		t.Fatal("files are left")
	}
	if !slices.Equal(fake.calls, []string{"systemctl disable --now gst-net.service", "systemctl daemon-reload"}) {
		t.Fatalf("calls: %q", fake.calls)
	}
	if err := h.Uninstall(); err != nil {
		t.Fatalf("uninstalling again: %v", err)
	}
}

func TestHelperOnLaunchd(t *testing.T) {
	loaded := false
	fake := &fakeRunner{respond: func(call string) (string, error) {
		switch {
		case strings.HasPrefix(call, "launchctl print"):
			if !loaded {
				return "", errors.New("not loaded")
			}
		case strings.HasPrefix(call, "launchctl bootstrap"):
			loaded = true
		case strings.HasPrefix(call, "launchctl bootout"):
			loaded = false
		}
		return "", nil
	}}
	dir := t.TempDir()
	exe := filepath.Join(dir, "gst")
	if err := os.WriteFile(exe, []byte("program"), 0o700); err != nil {
		t.Fatal(err)
	}
	l := &launchd{agentsDir: filepath.Join(dir, "LaunchDaemons"), logPath: filepath.Join(dir, "gst-net.log"), domain: "system", run: fake.run}
	h := &Helper{binary: filepath.Join(dir, "tools", helperLabel), manager: launchdHelper{l}}
	if err := h.Install(exe, "alice"); err != nil {
		t.Fatal(err)
	}
	plist, err := os.ReadFile(filepath.Join(l.agentsDir, helperLabel+".plist"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>" + h.binary + "</string>", "<string>net-helper</string>", "<string>alice</string>", "<key>KeepAlive</key>"} {
		if !strings.Contains(string(plist), want) {
			t.Fatalf("plist lacks %q:\n%s", want, plist)
		}
	}
	if !slices.Contains(fake.calls, "launchctl bootstrap system "+l.plistPath(helperLabel)) {
		t.Fatalf("calls: %q", fake.calls)
	}

	fake.calls = nil
	if err := h.Install(exe, "alice"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(fake.calls, "launchctl kickstart -k system/"+helperLabel) {
		t.Fatalf("calls: %q", fake.calls)
	}

	if err := h.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if fileExists(h.binary) || fileExists(l.plistPath(helperLabel)) {
		t.Fatal("files are left")
	}
}

func TestHelperCurrent(t *testing.T) {
	dir := t.TempDir()
	h := &Helper{binary: filepath.Join(dir, "gst-net")}
	socket := filepath.Join(dir, "socket")
	if h.Installed() || h.Current(2, socket) {
		t.Fatal("a missing helper is installed or current")
	}
	script := func(body string) {
		t.Helper()
		if err := os.WriteFile(h.binary, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script("echo 2")
	if !h.Installed() {
		t.Fatal("not installed")
	}
	if h.Current(2, socket) {
		t.Fatal("a helper that does not listen is current")
	}
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !h.Current(2, socket) {
		t.Fatal("the same version is not current")
	}
	if h.Current(3, socket) {
		t.Fatal("another version is current")
	}
	script("exit 1") // a helper from before versions
	if h.Current(2, socket) {
		t.Fatal("a helper that cannot say is current")
	}
}

func TestHelperDetach(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	run := func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return "", nil
	}
	for _, tt := range []struct {
		name    string
		manager helperManager
		files   []string
		want    []string
	}{
		{"systemd", systemdHelper{&systemd{dir: dir, system: true, run: run, notes: io.Discard}},
			[]string{helperUnit}, []string{"systemctl disable " + helperUnit, "systemctl daemon-reload", "systemctl stop --no-block " + helperUnit}},
		{"launchd", launchdHelper{&launchd{agentsDir: dir, logPath: filepath.Join(dir, "log"), domain: "system", run: run}},
			[]string{helperLabel + ".plist", "log"}, []string{"launchctl bootout system/" + helperLabel}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls = nil
			h := &Helper{binary: filepath.Join(dir, "gst-net"), manager: tt.manager}
			for _, name := range append(tt.files, "gst-net") {
				if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			stop, err := h.Detach()
			if err != nil {
				t.Fatal(err)
			}
			// Everything is gone before the stop, which ends the helper.
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("left over: %v", entries)
			}
			stop()
			if !slices.Equal(calls, tt.want) {
				t.Fatalf("ran %v, want %v", calls, tt.want)
			}
		})
	}
}
