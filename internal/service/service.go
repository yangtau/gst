// Package service runs each tunnel as a per-user background service, a
// systemd user unit on Linux and a launchd agent on macOS, which restarts
// the tunnel whenever it exits.
package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Status is the state of a tunnel's service.
type Status string

const (
	Stopped Status = "stopped" // no service exists
	Running Status = "running" // the tunnel process is running
	Down    Status = "down"    // the service exists, but its process is not running, as between restarts
)

// Manager controls the background services of tunnels by name.
type Manager interface {
	// Start writes the service definition, enables it at login or boot,
	// and starts it, restarting it if it already runs.
	Start(name string, command []string) error
	// Stop stops the service and deletes its definition. Stopping a
	// tunnel without a service does nothing.
	Stop(name string) error
	Status(name string) (Status, error)
	// Logs writes the last lines of the service's log to w and then, if
	// follow is set, new lines until ctx is done.
	Logs(ctx context.Context, name string, lines int, follow bool, w io.Writer) error
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

// streamer runs a command that writes its output to w until it exits or
// ctx is done.
type streamer func(ctx context.Context, w io.Writer, name string, args ...string) error

func execStream(ctx context.Context, w io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
