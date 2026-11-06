package service

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// The helper of the virtual network is a system service, because creating
// an interface and changing routes takes root. Root must never run a file
// that the user can change, so it runs a copy of gst that root owns.

const (
	helperUnit  = "gst-net.service"
	helperLabel = "local.gst.net"
)

// helperManager installs a service that runs as root.
type helperManager interface {
	install(command []string) error
	remove() error
}

type systemdHelper struct{ *systemd }

func (s systemdHelper) install(command []string) error {
	return s.systemd.install(helperUnit, systemdUnit("gst virtual network helper", command, "multi-user.target"), true)
}

func (s systemdHelper) remove() error { return s.systemd.remove(helperUnit) }

type launchdHelper struct{ *launchd }

func (l launchdHelper) install(command []string) error {
	return l.launchd.install(helperLabel, command, true)
}

func (l launchdHelper) remove() error { return l.launchd.remove(helperLabel) }

// Helper installs the helper of the virtual network on this machine.
type Helper struct {
	binary  string // where root's copy of gst goes
	manager helperManager
}

// NewHelper returns the installer for this platform.
func NewHelper() (*Helper, error) {
	switch runtime.GOOS {
	case "linux":
		return &Helper{"/usr/local/libexec/gst-net", systemdHelper{&systemd{
			dir: "/etc/systemd/system", system: true, run: execRun, notes: io.Discard,
		}}}, nil
	case "darwin":
		return &Helper{"/Library/PrivilegedHelperTools/" + helperLabel, launchdHelper{&launchd{
			agentsDir: "/Library/LaunchDaemons", logPath: "/var/log/gst-net.log", domain: "system",
			run: execRun, poll: 250 * time.Millisecond,
		}}}, nil
	default:
		return nil, fmt.Errorf("gst does not run on %s", runtime.GOOS)
	}
}

// Install copies the gst at executable and runs the copy as a service
// that serves user's daemon, restarting one that runs already.
func (h *Helper) Install(executable, user string) error {
	if err := copyExecutable(executable, h.binary); err != nil {
		return err
	}
	return h.manager.install([]string{h.binary, "net-helper", "--user", user})
}

// Uninstall stops the service and deletes it and its copy of gst.
func (h *Helper) Uninstall() error {
	if err := h.manager.remove(); err != nil {
		return err
	}
	if err := os.Remove(h.binary); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// copyExecutable copies from to to, replacing to in one step: a running
// program keeps its file, and writing to it would fail.
func copyExecutable(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := to + ".new"
	os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	_, err = io.Copy(out, in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, to)
}
