package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/tunnel"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// fakeTunnels stands in for tunnel.Run: destination "fail" fails at once;
// others become ready, log a line, and run until cancelled.
type fakeTunnels struct {
	mu     sync.Mutex
	starts map[string]int
	active map[string]int
}

func (f *fakeTunnels) run(ctx context.Context, opts tunnel.Options) error {
	f.mu.Lock()
	f.starts[opts.Server]++
	f.active[opts.Server]++
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active[opts.Server]--
		f.mu.Unlock()
	}()
	if opts.Server == "fail" {
		return errors.New("SSH keepalive timed out")
	}
	opts.Logger.Print("hello from " + opts.Server)
	opts.OnReady()
	<-ctx.Done()
	return nil
}

func (f *fakeTunnels) count(m map[string]int, name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return m[name]
}

type env struct {
	t       *testing.T
	cfg     Config
	fake    *fakeTunnels
	cancel  context.CancelFunc
	served  chan error
	tunnels map[string]*config.Tunnel
}

func newEnv(t *testing.T) *env {
	// Unix socket paths are short on macOS, so avoid t.TempDir's long names.
	dir, err := os.MkdirTemp("", "gst")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	fake := &fakeTunnels{starts: map[string]int{}, active: map[string]int{}}
	e := &env{t: t, fake: fake, tunnels: map[string]*config.Tunnel{}}
	e.cfg = Config{
		ConfigPath: filepath.Join(dir, "config.json"),
		Socket:     filepath.Join(dir, "run", "gst.sock"),
		LogDir:     filepath.Join(dir, "logs"),
		Software:   "gst test",
		Build:      "sha256:test",
		Resolve: func(_ context.Context, t *config.Tunnel) (tunnel.Options, error) {
			if t.Destination == "unresolvable" {
				return tunnel.Options{}, errors.New("ssh -G unresolvable: secret.example.com")
			}
			return tunnel.Options{Server: t.Destination}, nil
		},
		Run:        fake.run,
		RetryDelay: 10 * time.Millisecond,
	}
	return e
}

func (e *env) write(stopped map[string]bool, names ...string) {
	e.t.Helper()
	f := config.File{Version: config.Version, Tunnels: map[string]*config.Tunnel{}}
	for _, name := range names {
		f.Tunnels[name] = &config.Tunnel{Destination: name, Forwards: []config.Forward{}, Stopped: stopped[name]}
	}
	data, _ := json.Marshal(f)
	if err := os.WriteFile(e.cfg.ConfigPath, data, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) start() *Client {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.served = make(chan error, 1)
	d := New(e.cfg)
	go func() { e.served <- d.Serve(ctx) }()
	e.t.Cleanup(func() { cancel(); <-e.served })
	return e.dial()
}

func (e *env) dial() *Client {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := DialWait(ctx, e.cfg.Socket, "gst cli", "")
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { c.Close() })
	return c
}

// waitFor polls the daemon's status until cond holds.
func waitFor(t *testing.T, c *Client, msg string, cond func(map[string]wire.TunnelState) bool) map[string]wire.TunnelState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err := c.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		states := map[string]wire.TunnelState{}
		for _, s := range status.Tunnels {
			states[s.Name] = s
		}
		if cond(states) {
			return states
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s; states %+v", msg, states)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDaemonRunsStartedTunnels(t *testing.T) {
	e := newEnv(t)
	e.write(map[string]bool{"db": true}, "myhost", "db")
	c := e.start()
	if c.Daemon.Build != "sha256:test" || c.Daemon.Software != "gst test" {
		t.Fatalf("unexpected hello %+v", c.Daemon)
	}
	waitFor(t, c, "myhost not running", func(s map[string]wire.TunnelState) bool {
		_, db := s["db"]
		return s["myhost"].State == wire.StateRunning && !db
	})
	logged, _ := os.ReadFile(filepath.Join(e.cfg.LogDir, "myhost.log"))
	if !strings.Contains(string(logged), "hello from myhost") {
		t.Fatalf("tunnel log %q", logged)
	}

	// Reload starts db and stops myhost.
	e.write(map[string]bool{"myhost": true}, "myhost", "db")
	status, err := c.Reload(context.Background())
	if err != nil || len(status.Tunnels) != 1 || status.Tunnels[0].Name != "db" || e.fake.count(e.fake.active, "myhost") != 0 {
		t.Fatalf("reload gave %+v, %v; myhost active %d", status, err, e.fake.count(e.fake.active, "myhost"))
	}

	// A reload leaves running tunnels alone; a restart applies changes.
	waitFor(t, c, "db not running", func(s map[string]wire.TunnelState) bool { return s["db"].State == wire.StateRunning })
	if _, err := c.Reload(context.Background()); err != nil || e.fake.count(e.fake.starts, "db") != 1 {
		t.Fatalf("reload restarted db: %v", err)
	}
	if _, err := c.Restart(context.Background(), []string{"db"}); err != nil || e.fake.count(e.fake.starts, "db") != 2 || e.fake.count(e.fake.active, "db") != 1 {
		t.Fatalf("restart: %v, starts %d", err, e.fake.count(e.fake.starts, "db"))
	}
	var werr *wire.Error
	if _, err := c.Restart(context.Background(), []string{"myhost"}); !errors.As(err, &werr) || werr.Code != wire.CodeNotFound {
		t.Fatalf("restarting a stopped tunnel gave %v", err)
	}

	// Shutting down stops every tunnel.
	e.cancel()
	if err := <-e.served; err != nil {
		t.Fatal(err)
	}
	e.served <- nil
	if e.fake.count(e.fake.active, "db") != 0 {
		t.Fatal("db still runs after shutdown")
	}
}

func TestDaemonRetriesFailures(t *testing.T) {
	e := newEnv(t)
	e.write(nil, "fail", "unresolvable")
	c := e.start()
	states := waitFor(t, c, "no reconnects", func(s map[string]wire.TunnelState) bool {
		return s["fail"].Reconnects >= 2 && s["unresolvable"].Reconnects >= 2
	})
	if s := states["fail"]; s.Error != "SSH keepalive timed out" || s.State == wire.StateRunning {
		t.Fatalf("unexpected state %+v", s)
	}
	// Errors that reach the log never quote the ssh configuration.
	if s := states["unresolvable"]; s.Error != "ssh config resolution failed" {
		t.Fatalf("unexpected state %+v", s)
	}
	logged, _ := os.ReadFile(filepath.Join(e.cfg.LogDir, "unresolvable.log"))
	if strings.Contains(string(logged), "example.com") || !strings.Contains(string(logged), "ssh config resolution failed") {
		t.Fatalf("unexpected log %q", logged)
	}
}

func TestDaemonIsSingleAndPrivate(t *testing.T) {
	e := newEnv(t)
	e.write(nil)
	e.start()
	if err := New(e.cfg).Serve(context.Background()); err == nil || !strings.Contains(err.Error(), "another gst daemon") {
		t.Fatalf("second daemon gave %v", err)
	}
	for path, want := range map[string]os.FileMode{e.cfg.Socket: 0o600, filepath.Dir(e.cfg.Socket): 0o700} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: mode %v, %v; want %v", path, info.Mode().Perm(), err, want)
		}
	}

	conn, err := net.Dial("unix", e.cfg.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var werr *wire.Error
	if _, err := wire.Dial(conn, wire.Hello{Role: wire.RolePeer}); !errors.As(err, &werr) || werr.Code != wire.CodeUnsupportedRole {
		t.Fatalf("peer role gave %v", err)
	}
}

func TestDaemonReplacesStaleSocket(t *testing.T) {
	e := newEnv(t)
	e.write(nil)
	if err := os.MkdirAll(filepath.Dir(e.cfg.Socket), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", e.cfg.Socket)
	if err != nil {
		t.Fatal(err)
	}
	// Closing a listener removes its file; keep it to simulate a crash.
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	c := e.start()
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
}
