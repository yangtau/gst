package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/service"
	"github.com/yangtau/ssh-tunnel/internal/sshconfig"
)

// fakeServices is a service manager that only records state.
type fakeServices struct {
	status   map[string]service.Status
	commands map[string][]string // last start command per tunnel
	calls    []string
}

func (f *fakeServices) Start(name string, command []string) error {
	f.calls = append(f.calls, "start "+name)
	f.status[name] = service.Running
	f.commands[name] = command
	return nil
}

func (f *fakeServices) Stop(name string) error {
	f.calls = append(f.calls, "stop "+name)
	delete(f.status, name)
	return nil
}

func (f *fakeServices) Status(name string) (service.Status, error) {
	if s, ok := f.status[name]; ok {
		return s, nil
	}
	return service.Stopped, nil
}

func (f *fakeServices) Logs(_ context.Context, name string, lines int, follow bool, w io.Writer) error {
	f.calls = append(f.calls, "logs "+name)
	return nil
}

type testEnv struct {
	t        *testing.T
	config   string
	services *fakeServices
	hosts    map[string]sshconfig.Host // ssh -G results by destination
	identity string
	exe      string
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{
		t:        t,
		config:   filepath.Join(dir, "gst", "config.json"),
		services: &fakeServices{status: map[string]service.Status{}, commands: map[string][]string{}},
		identity: filepath.Join(dir, "id_ed25519"),
		exe:      "/usr/local/bin/gst",
	}
	write(t, e.identity, string(pem.EncodeToMemory(block)))
	knownHosts := filepath.Join(dir, "known_hosts")
	write(t, knownHosts, knownhosts.Line([]string{"ssh.example.com:22", "127.0.0.1:2"}, signer.PublicKey())+"\n")
	host := sshconfig.Host{Hostname: "ssh.example.com", Port: "22", User: "tunnel", IdentityFiles: []string{e.identity}, KnownHostsFiles: []string{knownHosts}, ServerAliveCountMax: 3}
	unknown := host
	unknown.Hostname = "unknown.example.com"
	closed := host
	closed.Hostname, closed.Port = "127.0.0.1", "2"
	e.hosts = map[string]sshconfig.Host{"myhost": host, "db": host, "unknown": unknown, "closed": closed}
	return e
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// gst runs the command line and returns its exit code, stdout, and stderr.
func (e *testEnv) gst(args ...string) (int, string, string) {
	e.t.Helper()
	var stdout, stderr bytes.Buffer
	a := &app{
		stdout:     &stdout,
		stderr:     &stderr,
		newManager: func() (service.Manager, error) { return e.services, nil },
		resolve: func(_ context.Context, destination string) (sshconfig.Host, error) {
			if h, ok := e.hosts[destination]; ok {
				return h, nil
			}
			return sshconfig.Host{}, errors.New("ssh -G " + destination + ": no such host")
		},
		executable: func() (string, error) { return e.exe, nil },
	}
	code := a.execute(context.Background(), append([]string{"--config", e.config}, args...))
	return code, stdout.String(), stderr.String()
}

func (e *testEnv) mustGst(args ...string) string {
	e.t.Helper()
	code, stdout, stderr := e.gst(args...)
	if code != 0 {
		e.t.Fatalf("gst %v exited %d\nstdout: %s\nstderr: %s", args, code, stdout, stderr)
	}
	return stdout
}

func (e *testEnv) expectCode(want int, args ...string) string {
	e.t.Helper()
	code, stdout, stderr := e.gst(args...)
	if code != want {
		e.t.Fatalf("gst %v exited %d, want %d\nstdout: %s\nstderr: %s", args, code, want, stdout, stderr)
	}
	return stderr
}

func (e *testEnv) tunnel(name string) *config.Tunnel {
	e.t.Helper()
	f, err := config.Load(e.config)
	if err != nil {
		e.t.Fatal(err)
	}
	return f.Tunnels[name]
}

func TestAddStartsTunnel(t *testing.T) {
	e := newEnv(t)
	out := e.mustGst("tunnel", "add", "myhost", "-R", "2222:localhost:22", "-L", "15432:127.0.0.1:5432", "--keepalive-interval", "30s")
	if !strings.Contains(out, "Added tunnel myhost and started it") || !strings.Contains(out, "-R 127.0.0.1:2222:localhost:22") {
		t.Fatalf("unexpected output %q", out)
	}
	want := &config.Tunnel{
		Destination:              "myhost",
		KeepaliveIntervalSeconds: 30,
		Forwards: []config.Forward{
			{Direction: config.Remote, Listen: "127.0.0.1:2222", Target: "localhost:22"},
			{Direction: config.Local, Listen: "127.0.0.1:15432", Target: "127.0.0.1:5432"},
		},
	}
	if got := e.tunnel("myhost"); !equalTunnels(got, want) {
		t.Fatalf("saved %+v, want %+v", got, want)
	}
	if !slices.Equal(e.services.commands["myhost"], []string{e.exe, "--config", e.config, "run", "myhost"}) {
		t.Fatalf("service command %v", e.services.commands["myhost"])
	}

	stderr := e.expectCode(exitFailure, "tunnel", "add", "myhost")
	if !strings.Contains(stderr, "already exists") || !strings.Contains(stderr, "hint:") {
		t.Fatalf("unexpected stderr %q", stderr)
	}

	e.mustGst("tunnel", "add", "home", "db")
	if e.tunnel("home").Destination != "db" || e.tunnel("home").Forwards == nil {
		t.Fatalf("explicit destination not saved: %+v", e.tunnel("home"))
	}
}

func equalTunnels(a, b *config.Tunnel) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestAddRejects(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{
		{"tunnel", "add"},
		{"tunnel", "add", "MyHost"},
		{"tunnel", "add", "myhost", "-oProxyCommand=x"},
		{"tunnel", "add", "myhost", "-R", "2222"},
		{"tunnel", "add", "myhost", "-R", "2222:a:1", "-R", "2222:b:1"},
		{"tunnel", "add", "myhost", "--connect-timeout", "1500ms"},
		{"tunnel", "add", "myhost", "--no-such-flag"},
		{"tunnel", "frobnicate"},
		{"tunnell"},
	} {
		if stderr := e.expectCode(exitUsage, args...); !strings.Contains(stderr, "--help") {
			t.Errorf("gst %v: usage errors must point to help: %q", args, stderr)
		}
	}

	// Preflight failures save nothing and start nothing.
	for _, args := range [][]string{
		{"tunnel", "add", "unknown"},
		{"tunnel", "add", "nowhere"},
		{"tunnel", "add", "myhost", "--identity", "/missing/key"},
	} {
		if stderr := e.expectCode(exitFailure, args...); !strings.Contains(stderr, "hint:") {
			t.Errorf("gst %v: no hint in %q", args, stderr)
		}
	}
	e.exe = "/tmp/go-build123/b001/exe/gst"
	if stderr := e.expectCode(exitFailure, "tunnel", "add", "myhost"); !strings.Contains(stderr, "go install") {
		t.Errorf("no install hint for go run: %q", stderr)
	}
	if f, _ := config.Load(e.config); len(f.Tunnels) != 0 || len(e.services.calls) != 0 {
		t.Fatalf("failed adds changed state: %+v %v", f.Tunnels, e.services.calls)
	}
}

func TestForwards(t *testing.T) {
	e := newEnv(t)
	e.mustGst("tunnel", "add", "myhost")
	e.services.calls = nil

	out := e.mustGst("forward", "add", "myhost", "-R", "*:2222:localhost:22", "-L", "15432:127.0.0.1:5432")
	if !strings.Contains(out, "Added -R 0.0.0.0:2222:localhost:22") || !strings.Contains(out, "Restarted tunnel myhost") {
		t.Fatalf("unexpected output %q", out)
	}
	if !slices.Equal(e.services.calls, []string{"start myhost"}) {
		t.Fatalf("running tunnel not restarted once: %v", e.services.calls)
	}

	e.services.calls = nil
	out = e.mustGst("forward", "add", "myhost", "-R", "0.0.0.0:2222:localhost:22")
	if !strings.Contains(out, "already has") || len(e.services.calls) != 0 {
		t.Fatalf("adding an existing forward must be a no-op: %q %v", out, e.services.calls)
	}
	if stderr := e.expectCode(exitFailure, "forward", "add", "myhost", "-R", "0.0.0.0:2222:localhost:2200"); !strings.Contains(stderr, "gst forward remove myhost -R 0.0.0.0:2222") {
		t.Fatalf("conflict hint missing: %q", stderr)
	}
	e.expectCode(exitUsage, "forward", "add", "myhost")
	e.expectCode(exitFailure, "forward", "add", "nope", "-R", "1:h:1")

	var views []forwardView
	if err := json.Unmarshal([]byte(e.mustGst("forward", "list", "myhost", "--json")), &views); err != nil || len(views) != 2 || views[1].Spec != "-L 127.0.0.1:15432:127.0.0.1:5432" || views[1].Number != 2 {
		t.Fatalf("unexpected forward list %+v: %v", views, err)
	}

	e.mustGst("tunnel", "stop", "myhost")
	e.services.calls = nil
	out = e.mustGst("forward", "rm", "myhost", "-L", "15432")
	if !strings.Contains(out, "Removed -L 127.0.0.1:15432:127.0.0.1:5432") || !strings.Contains(out, "stopped") || len(e.services.calls) != 0 {
		t.Fatalf("removing from a stopped tunnel: %q %v", out, e.services.calls)
	}
	e.expectCode(exitFailure, "forward", "remove", "myhost", "-L", "15432")
	e.expectCode(exitFailure, "forward", "remove", "myhost", "-R", "0.0.0.0:2222:otherhost:22")
	e.mustGst("forward", "remove", "myhost", "-R", "*:2222")
	if fwds := e.tunnel("myhost").Forwards; len(fwds) != 0 {
		t.Fatalf("forwards left: %+v", fwds)
	}
}

func TestLifecycle(t *testing.T) {
	e := newEnv(t)
	e.mustGst("tunnel", "add", "myhost")
	e.mustGst("tunnel", "add", "db")

	if out := e.mustGst("tunnel", "start", "myhost"); !strings.Contains(out, "already started") {
		t.Fatalf("unexpected output %q", out)
	}
	e.mustGst("tunnel", "stop", "myhost")
	if out := e.mustGst("tunnel", "stop", "myhost"); !strings.Contains(out, "already stopped") {
		t.Fatalf("unexpected output %q", out)
	}
	e.expectCode(exitFailure, "tunnel", "restart", "myhost")

	e.services.calls = nil
	e.mustGst("tunnel", "restart", "--all")
	if !slices.Equal(e.services.calls, []string{"start db"}) {
		t.Fatalf("restart --all must only restart started tunnels: %v", e.services.calls)
	}
	e.services.calls = nil
	e.mustGst("tunnel", "start", "--all")
	if !slices.Equal(e.services.calls, []string{"start myhost"}) {
		t.Fatalf("start --all must only start stopped tunnels: %v", e.services.calls)
	}
	e.expectCode(exitUsage, "tunnel", "start")
	e.expectCode(exitUsage, "tunnel", "start", "myhost", "--all")
	e.expectCode(exitFailure, "tunnel", "stop", "myhost", "nope")

	e.services.status["db"] = service.Down
	var views []tunnelView
	if err := json.Unmarshal([]byte(e.mustGst("tunnel", "list", "--json")), &views); err != nil || len(views) != 2 ||
		views[0].Name != "db" || views[0].Status != service.Down || views[1].Status != service.Running {
		t.Fatalf("unexpected list %+v: %v", views, err)
	}

	e.services.calls = nil
	e.mustGst("tunnel", "rm", "myhost", "db")
	if f, _ := config.Load(e.config); len(f.Tunnels) != 0 || !slices.Equal(e.services.calls, []string{"stop db", "stop myhost"}) && !slices.Equal(e.services.calls, []string{"stop myhost", "stop db"}) {
		t.Fatalf("remove left %+v, calls %v", f.Tunnels, e.services.calls)
	}
	if out := e.mustGst("tunnel", "list"); !strings.Contains(out, "No tunnels") {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestSet(t *testing.T) {
	e := newEnv(t)
	e.mustGst("tunnel", "add", "myhost", "--identity", e.identity, "--keepalive-count", "5")
	e.expectCode(exitUsage, "tunnel", "set", "myhost")
	e.expectCode(exitUsage, "tunnel", "set", "myhost", "--keepalive-count", "-1")
	e.expectCode(exitFailure, "tunnel", "set", "myhost", "--destination", "unknown")
	if e.tunnel("myhost").Destination != "myhost" {
		t.Fatal("failed set changed the tunnel")
	}

	e.services.calls = nil
	out := e.mustGst("tunnel", "set", "myhost", "--identity", "", "--keepalive-count", "0", "--destination", "db")
	got := e.tunnel("myhost")
	if got.IdentityFile != "" || got.KeepaliveCountMax != 0 || got.Destination != "db" {
		t.Fatalf("set did not apply: %+v", got)
	}
	if !strings.Contains(out, "Restarted tunnel myhost") || !slices.Equal(e.services.calls, []string{"start myhost"}) {
		t.Fatalf("running tunnel not restarted: %q %v", out, e.services.calls)
	}
	e.services.calls = nil
	if out := e.mustGst("tunnel", "set", "myhost", "--cleanup-listeners", "--no-restart"); !strings.Contains(out, "gst tunnel restart myhost") || len(e.services.calls) != 0 {
		t.Fatalf("--no-restart restarted: %q %v", out, e.services.calls)
	}
}

func TestShowAndCheck(t *testing.T) {
	e := newEnv(t)
	e.mustGst("tunnel", "add", "myhost", "-R", "2222:localhost:22")
	var v tunnelView
	if err := json.Unmarshal([]byte(e.mustGst("tunnel", "show", "myhost", "--json")), &v); err != nil {
		t.Fatal(err)
	}
	if v.Resolved == nil || v.Resolved.Server != "ssh.example.com:22" || v.Resolved.KeepaliveTimeoutSeconds != 45 || len(v.Forwards) != 1 {
		t.Fatalf("unexpected view %+v", v)
	}
	if out := e.mustGst("tunnel", "show", "myhost"); !strings.Contains(out, "tunnel@ssh.example.com:22") {
		t.Fatalf("unexpected output %q", out)
	}

	e.mustGst("tunnel", "add", "closed")
	code, stdout, _ := e.gst("tunnel", "check", "closed", "--json")
	var result struct {
		OK    bool
		Steps []struct {
			Name string
			OK   bool
			Hint string
		}
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil || code != exitFailure || result.OK {
		t.Fatalf("check of an unreachable server: code %d, %s, %v", code, stdout, err)
	}
	last := result.Steps[len(result.Steps)-1]
	if last.Name != "connect" || last.OK || last.Hint == "" || len(result.Steps) != 4 {
		t.Fatalf("unexpected steps %+v", result.Steps)
	}

	e.mustGst("tunnel", "logs", "myhost", "-n", "5")
	e.expectCode(exitUsage, "tunnel", "logs", "myhost", "-n", "0")
}

func TestOptionsPrecedence(t *testing.T) {
	a := &app{resolve: func(context.Context, string) (sshconfig.Host, error) {
		return sshconfig.Host{Hostname: "h", Port: "22", User: "u", IdentityFiles: []string{"/ssh/key"}, KnownHostsFiles: []string{"/ssh/kh"},
			ConnectTimeout: 5 * time.Second, ServerAliveInterval: 0, ServerAliveCountMax: 4}, nil
	}}
	opts, err := a.options(context.Background(), &config.Tunnel{Destination: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.ConnectTimeout != 5*time.Second || opts.KeepaliveInterval != defaultKeepaliveInterval || opts.KeepaliveTimeout != 4*defaultKeepaliveInterval || opts.IdentityFiles[0] != "/ssh/key" {
		t.Fatalf("ssh config not applied: %+v", opts)
	}
	opts, _ = a.options(context.Background(), &config.Tunnel{Destination: "h", IdentityFile: "/gst/key", KnownHostsFile: "/gst/kh", ConnectTimeoutSeconds: 9, KeepaliveIntervalSeconds: 2, KeepaliveCountMax: 2})
	if opts.ConnectTimeout != 9*time.Second || opts.KeepaliveTimeout != 4*time.Second || !slices.Equal(opts.IdentityFiles, []string{"/gst/key"}) || !slices.Equal(opts.KnownHostsFiles, []string{"/gst/kh"}) {
		t.Fatalf("overrides not applied: %+v", opts)
	}
	if _, port, _ := net.SplitHostPort(opts.Server); port != "22" {
		t.Fatal(opts.Server)
	}
}
