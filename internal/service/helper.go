package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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
	// detach is remove from inside the service: stopping a service ends
	// what it runs, so it deletes everything first and returns the stop.
	detach() (stop func(), err error)
}

type systemdHelper struct{ *systemd }

func (s systemdHelper) install(command []string) error {
	return s.systemd.install(helperUnit, systemdUnit("gst virtual network helper", command, "multi-user.target"), true)
}

func (s systemdHelper) remove() error { return s.systemd.remove(helperUnit) }

func (s systemdHelper) detach() (func(), error) {
	if _, err := s.systemctl("disable", helperUnit); err != nil {
		return nil, err
	}
	if err := os.Remove(filepath.Join(s.dir, helperUnit)); err != nil {
		return nil, err
	}
	if _, err := s.systemctl("daemon-reload"); err != nil {
		return nil, err
	}
	return func() { s.systemctl("stop", "--no-block", helperUnit) }, nil
}

type launchdHelper struct{ *launchd }

func (l launchdHelper) install(command []string) error {
	return l.launchd.install(helperLabel, command, true)
}

func (l launchdHelper) remove() error {
	if err := l.launchd.remove(helperLabel); err != nil {
		return err
	}
	return removeFile(l.logPath)
}

func (l launchdHelper) detach() (func(), error) {
	if err := os.Remove(l.plistPath(helperLabel)); err != nil {
		return nil, err
	}
	if err := removeFile(l.logPath); err != nil {
		return nil, err
	}
	return func() { l.run("launchctl", "bootout", l.target(helperLabel)) }, nil
}

// Helper installs the helper of the virtual network on this machine.
type Helper struct {
	binary  string // where root's copy of gst goes
	manager helperManager
	service string // the name and file of the service, for people to read
}

// NewHelper returns the installer for this platform.
func NewHelper() (*Helper, error) {
	switch runtime.GOOS {
	case "linux":
		return &Helper{"/usr/local/libexec/gst-net", systemdHelper{&systemd{
			dir: "/etc/systemd/system", system: true, run: execRun, notes: io.Discard,
		}}, helperUnit + " (/etc/systemd/system/" + helperUnit + ")"}, nil
	case "darwin":
		return &Helper{"/Library/PrivilegedHelperTools/" + helperLabel, launchdHelper{&launchd{
			agentsDir: "/Library/LaunchDaemons", logPath: "/var/log/gst-net.log", domain: "system",
			run: execRun, poll: 250 * time.Millisecond,
		}}, helperLabel + " (/Library/LaunchDaemons/" + helperLabel + ".plist)"}, nil
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

// Installed reports whether the helper's copy of gst is on this machine.
func (h *Helper) Installed() bool {
	_, err := os.Stat(h.binary)
	return err == nil
}

// Current reports whether the helper is of version and listens on socket.
// Asking root's copy of gst needs no root. A copy that cannot say is older
// than any version.
func (h *Helper) Current(version int, socket string) bool {
	if _, err := os.Stat(socket); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, h.binary, "helper-version").Output()
	if err != nil {
		return false
	}
	installed, err := strconv.Atoi(strings.TrimSpace(string(out)))
	return err == nil && installed == version
}

// Installs says what Install does to this machine, for people to read.
func (h *Helper) Installs() []string {
	return []string{
		"copies this gst to " + h.binary + ", owned by root",
		"adds the system service " + h.service + ", which runs that copy as root, now and at every boot",
	}
}

// Removes says what Uninstall does to this machine, for people to read.
func (h *Helper) Removes() []string {
	return []string{
		"stops and removes the system service " + h.service,
		"deletes " + h.binary,
	}
}

// Uninstall stops the service and deletes it and its copy of gst.
func (h *Helper) Uninstall() error {
	if err := h.manager.remove(); err != nil {
		return err
	}
	return removeFile(h.binary)
}

// Detach is Uninstall for the helper itself, which goes on until stop.
func (h *Helper) Detach() (stop func(), err error) {
	if stop, err = h.manager.detach(); err != nil {
		return nil, err
	}
	return stop, removeFile(h.binary)
}

// removeFile deletes the file at path, if there is one.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
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
