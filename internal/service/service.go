// Package service installs the gst daemon as a per-user background
// service, a systemd user unit on Linux and a launchd agent on macOS, which
// starts at login and restarts the daemon whenever it exits.
package service

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Manager controls the daemon's background service.
type Manager interface {
	// Install writes the service definition to run command, enables it at
	// login or boot, and makes sure it runs. A running service restarts
	// when its definition changed or restart is set.
	Install(command []string, restart bool) error
	// Uninstall stops the service and deletes its definition. Uninstalling
	// a missing service does nothing.
	Uninstall() error
	// LogHint tells the user where the daemon's own log is.
	LogHint() string
}

// Paths are where the daemon keeps its files on this platform.
type Paths struct {
	Socket string // control socket
	LogDir string // link logs, <name>.log
}

// DefaultPaths returns the paths for this platform: on Linux, under
// $XDG_STATE_HOME/gst (~/.local/state/gst); on macOS, the socket under
// ~/Library/Application Support/gst and the logs in ~/Library/Logs/gst.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("home directory lookup failed: %w", err)
	}
	if runtime.GOOS == "darwin" {
		return Paths{
			Socket: filepath.Join(home, "Library", "Application Support", "gst", "gst.sock"),
			LogDir: filepath.Join(home, "Library", "Logs", "gst"),
		}, nil
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		state = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(state, "gst")
	return Paths{Socket: filepath.Join(dir, "gst.sock"), LogDir: filepath.Join(dir, "logs")}, nil
}

// New returns the manager for this platform. Advice for the user, such as
// enabling systemd lingering, is written to notes.
func New(notes io.Writer) (Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home directory lookup failed: %w", err)
	}
	switch runtime.GOOS {
	case "linux":
		return newSystemd(home, notes), nil
	case "darwin":
		return newLaunchd(home, os.Getuid()), nil
	default:
		return nil, fmt.Errorf("background tunnels are not supported on %s", runtime.GOOS)
	}
}

// runner runs a command and returns its standard output. Errors include
// the command's standard error.
type runner func(name string, args ...string) (string, error)

func execRun(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeIfChanged writes data to path unless path already holds it, and
// reports whether it wrote.
func writeIfChanged(path string, data []byte) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	return true, os.WriteFile(path, data, 0o644)
}
