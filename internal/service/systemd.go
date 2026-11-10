package service

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

type systemd struct {
	dir    string // systemd unit directory
	system bool   // uses the system's service manager, not the user's
	run    runner
	notes  io.Writer
}

func newSystemd(home string, notes io.Writer) *systemd {
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" {
		config = filepath.Join(home, ".config")
	}
	return &systemd{dir: filepath.Join(config, "systemd", "user"), run: execRun, notes: notes}
}

const unitName = "gst.service"

func (s *systemd) systemctl(args ...string) (string, error) {
	if !s.system {
		args = append([]string{"--user"}, args...)
	}
	return s.run("systemctl", args...)
}

func (s *systemd) Install(command []string, restart bool) error {
	if err := s.install(unitName, systemdUnit("gst daemon", command, "default.target"), restart); err != nil {
		return err
	}
	s.noteLinger()
	return nil
}

// install writes unit and makes sure it runs.
func (s *systemd) install(unit, text string, restart bool) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	changed, err := writeIfChanged(filepath.Join(s.dir, unit), []byte(text))
	if err != nil {
		return err
	}
	action := "start"
	if changed || restart {
		action = "restart"
	}
	steps := [][]string{{"enable", unit}, {action, unit}}
	if changed {
		steps = append([][]string{{"daemon-reload"}}, steps...)
	}
	for _, args := range steps {
		if _, err := s.systemctl(args...); err != nil {
			return err
		}
	}
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
		fmt.Fprintf(s.notes, "note: lingering is off for %s, so gst stops when you log out. Keep it running with: loginctl enable-linger %s\n", u.Username, u.Username)
	}
}

func (s *systemd) Uninstall() error { return s.remove(unitName) }

func (s *systemd) LogHint() string {
	return "see gst's log with `journalctl --user -u " + strings.TrimSuffix(unitName, ".service") + "`"
}

func (s *systemd) remove(unit string) error {
	path := filepath.Join(s.dir, unit)
	if !fileExists(path) {
		return nil
	}
	if _, err := s.systemctl("disable", "--now", unit); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	_, err := s.systemctl("daemon-reload")
	return err
}

// systemdUnit restarts the program 5 s after every exit, without a limit,
// and gives it 15 s to exit after SIGTERM. Its log goes to journald.
func systemdUnit(description string, command []string, wantedBy string) string {
	quoted := make([]string, len(command))
	for i, arg := range command {
		quoted[i] = systemdQuote(arg)
	}
	return fmt.Sprintf(`[Unit]
Description=%s
StartLimitIntervalSec=0

[Service]
ExecStart=%s
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=5
TimeoutStopSec=15
KillSignal=SIGTERM

[Install]
WantedBy=%s
`, description, strings.Join(quoted, " "), wantedBy)
}

var systemdEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$")

// systemdQuote quotes arg for ExecStart, escaping the characters that
// systemd would otherwise interpret.
func systemdQuote(arg string) string {
	return `"` + systemdEscaper.Replace(arg) + `"`
}
