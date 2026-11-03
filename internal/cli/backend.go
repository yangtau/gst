package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/daemon"
	"github.com/yangtau/ssh-tunnel/internal/service"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// backend runs tunnels in the background. The real one controls the gst
// daemon; tests use a fake.
type backend interface {
	// Sync makes the background match the configuration file. When the
	// daemon is needed, for started tunnels or the mesh, it installs the
	// daemon's service to run command, restarts a daemon that runs another
	// gst, and tells the daemon to reload the configuration; otherwise it
	// removes the service.
	Sync(ctx context.Context, command []string, needed bool) error
	// Restart restarts started tunnels in a daemon that Sync brought up.
	Restart(ctx context.Context, names []string) error
	// States returns the states of the tunnels that the daemon runs, and
	// false when no daemon answers.
	States(ctx context.Context) (map[string]wire.TunnelState, bool)
	// Logs writes the last lines of a tunnel's log to w and then, if
	// follow is set, new lines until ctx is done.
	Logs(ctx context.Context, name string, lines int, follow bool, w io.Writer) error
	RemoveLogs(name string) error
	// Peers returns the daemon's peer links.
	Peers(ctx context.Context) (wire.Peers, error)
	// HasLegacy and RemoveLegacy find and remove gst 0.1's per-tunnel
	// services.
	HasLegacy(name string) bool
	RemoveLegacy(name string) error
}

const software = "gst " + version

// daemonBackend is the real backend.
type daemonBackend struct {
	manager service.Manager
	paths   service.Paths
	build   func() (string, error)
}

func newDaemonBackend(notes io.Writer, executable func() (string, error)) (backend, error) {
	m, err := service.New(notes)
	if err != nil {
		return nil, err
	}
	paths, err := service.DefaultPaths()
	if err != nil {
		return nil, err
	}
	return &daemonBackend{manager: m, paths: paths, build: func() (string, error) {
		exe, err := executable()
		if err != nil {
			return "", err
		}
		return daemon.Build(exe)
	}}, nil
}

// daemonLogHint says where the daemon logs its own problems.
func daemonLogHint() string {
	if runtime.GOOS == "darwin" {
		return "see the daemon log in ~/Library/Logs/gst-daemon.log"
	}
	return "see the daemon log with `journalctl --user -u gst`"
}

func (b *daemonBackend) Sync(ctx context.Context, command []string, needed bool) error {
	if !needed {
		return b.manager.Uninstall()
	}
	build, err := b.build()
	if err != nil {
		return err
	}
	if err := b.manager.Install(command, false); err != nil {
		return err
	}
	c, err := b.connect(ctx, "")
	if err != nil {
		return err
	}
	if c.Daemon.Build != build {
		// The daemon runs another gst, as after an upgrade.
		c.Close()
		if err := b.manager.Install(command, true); err != nil {
			return err
		}
		if c, err = b.connect(ctx, build); err != nil {
			return err
		}
	}
	defer c.Close()
	if _, err := c.Reload(ctx); err != nil {
		return withHint(fmt.Errorf("the gst daemon could not reload: %w", err), daemonLogHint())
	}
	return nil
}

// connect connects to the daemon, waiting for its service to start it.
// With build set, it also waits until a daemon of that build answers, as
// one that is still shutting down may answer for a moment.
func (b *daemonBackend) connect(ctx context.Context, build string) (*daemon.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		c, err := daemon.DialWait(ctx, b.paths.Socket, software, build)
		if err != nil {
			return nil, withHint(fmt.Errorf("the gst daemon does not answer: %w", err), daemonLogHint())
		}
		if build == "" || c.Daemon.Build == build {
			return c, nil
		}
		c.Close()
		select {
		case <-ctx.Done():
			return nil, withHint(errors.New("the gst daemon still runs another gst after a restart"), daemonLogHint())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (b *daemonBackend) Restart(ctx context.Context, names []string) error {
	c, err := b.connect(ctx, "")
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Restart(ctx, names)
	return err
}

// connectOnce connects to a daemon that is already running.
func (b *daemonBackend) connectOnce(ctx context.Context) (*daemon.Client, error) {
	return daemon.Dial(ctx, b.paths.Socket, software, "")
}

func (b *daemonBackend) States(ctx context.Context) (map[string]wire.TunnelState, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c, err := b.connectOnce(ctx)
	if err != nil {
		return nil, false
	}
	defer c.Close()
	status, err := c.Status(ctx)
	if err != nil {
		return nil, false
	}
	states := make(map[string]wire.TunnelState, len(status.Tunnels))
	for _, s := range status.Tunnels {
		states[s.Name] = s
	}
	return states, true
}

func (b *daemonBackend) logPath(name string) string {
	return filepath.Join(b.paths.LogDir, name+".log")
}

func (b *daemonBackend) Logs(ctx context.Context, name string, lines int, follow bool, w io.Writer) error {
	path := b.logPath(name)
	if _, err := os.Stat(path); err != nil && !follow {
		_, err := fmt.Fprintf(w, "No log for tunnel %s yet.\n", name)
		return err
	}
	args := []string{"-n", strconv.Itoa(lines)}
	if follow {
		args = append(args, "-F")
	}
	cmd := exec.CommandContext(ctx, "tail", append(args, path)...)
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("tail: %w", err)
	}
	return nil
}

func (b *daemonBackend) RemoveLogs(name string) error {
	for _, path := range []string{b.logPath(name), b.logPath(name) + ".1"} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (b *daemonBackend) HasLegacy(name string) bool     { return b.manager.HasLegacy(name) }
func (b *daemonBackend) RemoveLegacy(name string) error { return b.manager.RemoveLegacy(name) }
