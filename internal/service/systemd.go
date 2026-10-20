package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

type systemd struct {
	dir    string // systemd user unit directory
	run    runner
	stream streamer
	notes  io.Writer
}

func newSystemd(home string, notes io.Writer) *systemd {
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" {
		config = filepath.Join(home, ".config")
	}
	return &systemd{dir: filepath.Join(config, "systemd", "user"), run: execRun, stream: execStream, notes: notes}
}

func unitName(name string) string { return "gst-" + name + ".service" }

func (s *systemd) unitPath(name string) string { return filepath.Join(s.dir, unitName(name)) }

func (s *systemd) systemctl(args ...string) (string, error) {
	return s.run("systemctl", append([]string{"--user"}, args...)...)
}

func (s *systemd) Start(name string, command []string) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.unitPath(name), []byte(systemdUnit(name, command)), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", unitName(name)}, {"restart", unitName(name)}} {
		if _, err := s.systemctl(args...); err != nil {
			return err
		}
	}
	s.noteLinger()
	return nil
}

// noteLinger warns that without lingering, user services stop at logout.
func (s *systemd) noteLinger() {
	u, err := user.Current()
	if err != nil {
		return
	}
	out, err := s.run("loginctl", "show-user", u.Username, "--property=Linger", "--value")
	if err == nil && strings.TrimSpace(out) == "no" {
		fmt.Fprintf(s.notes, "note: lingering is off for %s, so tunnels stop when you log out. Keep them running with: loginctl enable-linger %s\n", u.Username, u.Username)
	}
}

func (s *systemd) Stop(name string) error {
	if !fileExists(s.unitPath(name)) {
		return nil
	}
	if _, err := s.systemctl("disable", "--now", unitName(name)); err != nil {
		return err
	}
	if err := os.Remove(s.unitPath(name)); err != nil {
		return err
	}
	_, err := s.systemctl("daemon-reload")
	return err
}

func (s *systemd) Status(name string) (Status, error) {
	if !fileExists(s.unitPath(name)) {
		return Stopped, nil
	}
	out, err := s.systemctl("show", unitName(name), "--property=ActiveState", "--value")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "active" {
		return Running, nil
	}
	return Down, nil
}

// RemoveLogs does nothing: journald owns the log and expires it.
func (s *systemd) RemoveLogs(string) error { return nil }

func (s *systemd) Logs(ctx context.Context, name string, lines int, follow bool, w io.Writer) error {
	args := []string{"--user", "--unit", unitName(name), "--lines", strconv.Itoa(lines), "--no-pager", "--output", "short-iso"}
	if follow {
		args = append(args, "--follow")
	}
	return s.stream(ctx, w, "journalctl", args...)
}

// systemdUnit restarts the tunnel 5 s after every exit, without a limit,
// and gives it 15 s to exit after SIGTERM.
func systemdUnit(name string, command []string) string {
	quoted := make([]string, len(command))
	for i, arg := range command {
		quoted[i] = systemdQuote(arg)
	}
	return fmt.Sprintf(`[Unit]
Description=gst tunnel %s
StartLimitIntervalSec=0

[Service]
ExecStart=%s
Restart=always
RestartSec=5
TimeoutStopSec=15
KillSignal=SIGTERM

[Install]
WantedBy=default.target
`, name, strings.Join(quoted, " "))
}

var systemdEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$")

// systemdQuote quotes arg for ExecStart, escaping the characters that
// systemd would otherwise interpret.
func systemdQuote(arg string) string {
	return `"` + systemdEscaper.Replace(arg) + `"`
}
