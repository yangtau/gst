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

func (f *fakeServices) RemoveLogs(name string) error {
	f.calls = append(f.calls, "remove-logs "+name)
	return nil
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
	out := e.mustGst("add", "myhost", "-R", "2222:localhost:22", "-L", "15432:127.0.0.1:5432", "--keepalive-interval", "30s")
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

	stderr := e.expectCode(exitFailure, "add", "myhost")
	if !strings.Contains(stderr, "already exists") || !strings.Contains(stderr, "hint:") {
		t.Fatalf("unexpected stderr %q", stderr)
	}

	e.mustGst("add", "home", "db")
	if e.tunnel("home").Destination != "db" || e.tunnel("home").Forwards == nil {
		t.Fatalf("explicit destination not saved: %+v", e.tunnel("home"))
	}
}

func equalTunnels(a, b *config.Tunnel) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestHelpListsCommandsByTask(t *testing.T) {
	e := newEnv(t)
	root := e.mustGst("--help")
	for _, want := range []string{"Usage:\n  gst <command>\n", "Configure tunnels:\n  add ", "Run tunnels:\n  start ", "Inspect tunnels:\n  list ", `Use "gst <command> --help"`} {
		if !strings.Contains(root, want) {
			t.Errorf("gst --help lacks %q:\n%s", want, root)
		}
	}
	if strings.Contains(root, "  run ") {
		t.Errorf("gst --help lists the hidden run command:\n%s", root)
	}
	if forward := e.mustGst("forward"); !strings.Contains(forward, "Usage:\n  gst forward <command>\n\n") {
		t.Errorf("gst forward must print one usage line:\n%s", forward)
	}
}

func TestArgumentErrorsNameTheArgument(t *testing.T) {
	e := newEnv(t)
	if stderr := e.expectCode(exitUsage, "forward", "list"); !strings.Contains(stderr, "missing argument <tunnel>") || !strings.Contains(stderr, "no tunnels yet") {
		t.Fatalf("stderr %q", stderr)
	}
	e.mustGst("add", "myhost")
	e.mustGst("add", "db")
	for args, want := range map[string][]string{
		"forward list":      {"error: missing argument <tunnel>\n", "usage: gst forward list <tunnel>", "hint: tunnels: db, myhost\n"},
		"stop":              {"error: missing argument <name> or --all\n", "hint: tunnels: db, myhost\n"},
		"remove":            {"error: missing argument <name>\n", "hint: tunnels: db, myhost\n"},
		"forward list db x": {`error: unexpected argument "x"`},
		"list x":            {`error: unexpected argument "x"`},
		"forward lst":       {"usage: gst forward <command>\n"},
		"lst":               {`did you mean "set" or "list"?`, "usage: gst <command>\n"},
	} {
		stderr := e.expectCode(exitUsage, strings.Fields(args)...)
		for _, w := range want {
			if !strings.Contains(stderr, w) {
				t.Errorf("gst %s: stderr %q lacks %q", args, stderr, w)
			}
		}
	}
	if stderr := e.expectCode(exitFailure, "show", "nope"); !strings.Contains(stderr, "error: no tunnel named nope\nhint: tunnels: db, myhost\n") {
		t.Errorf("unknown tunnel: stderr %q", stderr)
	}
	if stderr := e.expectCode(exitUsage, "add"); strings.Contains(stderr, "hint:") {
		t.Errorf("a new tunnel name needs no list of existing tunnels: %q", stderr)
	}
}

func TestAddRejects(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{
		{"add"},
		{"add", "MyHost"},
		{"add", "myhost", "-oProxyCommand=x"},
		{"add", "myhost", "-R", "0"},
		{"add", "myhost", "-R", "2222:a:1", "-R", "2222:b:1"},
		{"add", "myhost", "--connect-timeout", "1500ms"},
		{"add", "myhost", "--no-such-flag"},
		{"frobnicate"},
		{"tunnell"},
	} {
		if stderr := e.expectCode(exitUsage, args...); !strings.Contains(stderr, "--help") {
			t.Errorf("gst %v: usage errors must point to help: %q", args, stderr)
		}
	}

	// Preflight failures save nothing and start nothing.
	for _, args := range [][]string{
		{"add", "unknown"},
		{"add", "nowhere"},
		{"add", "myhost", "--identity", "/missing/key"},
	} {
		if stderr := e.expectCode(exitFailure, args...); !strings.Contains(stderr, "hint:") {
			t.Errorf("gst %v: no hint in %q", args, stderr)
		}
	}
	e.exe = "/tmp/go-build123/b001/exe/gst"
	if stderr := e.expectCode(exitFailure, "add", "myhost"); !strings.Contains(stderr, "go install") {
		t.Errorf("no install hint for go run: %q", stderr)
	}
	if f, _ := config.Load(e.config); len(f.Tunnels) != 0 || len(e.services.calls) != 0 {
		t.Fatalf("failed adds changed state: %+v %v", f.Tunnels, e.services.calls)
	}
}

func TestForwards(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "myhost")
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

	if out := e.mustGst("forward", "add", "myhost", "-D", "1080"); !strings.Contains(out, "Added -D 127.0.0.1:1080 to tunnel myhost.") {
		t.Fatalf("unexpected output %q", out)
	}
	if out := e.mustGst("forward", "add", "myhost", "-D", "127.0.0.1:1080"); !strings.Contains(out, "already has") {
		t.Fatalf("adding an existing dynamic forward must be a no-op: %q", out)
	}
	if stderr := e.expectCode(exitFailure, "forward", "add", "myhost", "-L", "1080:h:1"); !strings.Contains(stderr, "gst forward remove myhost -D 127.0.0.1:1080") {
		t.Fatalf("local and dynamic forwards on one port must conflict: %q", stderr)
	}
	e.expectCode(exitUsage, "forward", "add", "myhost", "-D", "1080:h:1")

	var views []forwardView
	if err := json.Unmarshal([]byte(e.mustGst("forward", "list", "myhost", "--json")), &views); err != nil || len(views) != 3 || views[1].Spec != "-L 127.0.0.1:15432:127.0.0.1:5432" || views[1].Number != 2 ||
		views[2] != (forwardView{3, "dynamic", "127.0.0.1:1080", "", "-D 127.0.0.1:1080"}) {
		t.Fatalf("unexpected forward list %+v: %v", views, err)
	}
	e.expectCode(exitFailure, "forward", "remove", "myhost", "-L", "1080")
	if out := e.mustGst("forward", "remove", "myhost", "-D", "1080"); !strings.Contains(out, "Removed -D 127.0.0.1:1080 from tunnel myhost.") {
		t.Fatalf("unexpected output %q", out)
	}

	if out := e.mustGst("forward", "add", "myhost", "-L", "/tmp/db.sock:/run/pg.sock", "-R", "/run/agent.sock:/tmp/agent.sock"); !strings.Contains(out, "Added -L /tmp/db.sock:/run/pg.sock") || !strings.Contains(out, "Added -R /run/agent.sock:/tmp/agent.sock") {
		t.Fatalf("unexpected output %q", out)
	}
	if stderr := e.expectCode(exitUsage, "forward", "add", "myhost", "-R", "run/agent.sock:localhost:22"); !strings.Contains(stderr, "must be absolute") {
		t.Fatalf("relative server socket accepted: %q", stderr)
	}
	if stderr := e.expectCode(exitFailure, "forward", "add", "myhost", "-L", "/tmp/db.sock:h:1"); !strings.Contains(stderr, "gst forward remove myhost -L /tmp/db.sock") {
		t.Fatalf("socket conflict hint missing: %q", stderr)
	}
	e.mustGst("forward", "remove", "myhost", "-L", "/tmp/db.sock", "-R", "/run/agent.sock:/tmp/agent.sock")

	e.mustGst("stop", "myhost")
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

func TestReverseDynamicForwards(t *testing.T) {
	e := newEnv(t)
	if out := e.mustGst("add", "myhost", "-R", "1080", "-D", "1080"); !strings.Contains(out, "-R 127.0.0.1:1080\n") {
		t.Fatalf("unexpected output %q", out)
	}
	want := config.Forward{Direction: config.Remote, Listen: "127.0.0.1:1080"}
	if got := e.tunnel("myhost").Forwards[0]; got != want {
		t.Fatalf("reverse SOCKS forward = %+v; want %+v", got, want)
	}
	e.services.calls = nil
	if out := e.mustGst("forward", "add", "myhost", "-R", "127.0.0.1:1080"); !strings.Contains(out, "already has") || len(e.services.calls) != 0 {
		t.Fatalf("duplicate must be a no-op: %q %v", out, e.services.calls)
	}
	if stderr := e.expectCode(exitFailure, "forward", "add", "myhost", "-R", "1080:localhost:22"); !strings.Contains(stderr, "gst forward remove myhost -R 127.0.0.1:1080") {
		t.Fatalf("missing listener conflict hint: %q", stderr)
	}
	var views []forwardView
	if err := json.Unmarshal([]byte(e.mustGst("forward", "list", "myhost", "--json")), &views); err != nil || len(views) != 2 ||
		views[0] != (forwardView{1, "remote", "127.0.0.1:1080", "", "-R 127.0.0.1:1080"}) {
		t.Fatalf("unexpected forward list %+v: %v", views, err)
	}
	if out := e.mustGst("forward", "remove", "myhost", "-R", "1080"); !strings.Contains(out, "Removed -R 127.0.0.1:1080 from tunnel myhost.") {
		t.Fatalf("unexpected output %q", out)
	}
	e.mustGst("forward", "add", "myhost", "-R", "1080:localhost:22")
	e.expectCode(exitFailure, "forward", "add", "myhost", "-R", "1080")
	e.mustGst("forward", "remove", "myhost", "-R", "1080")
	if out := e.mustGst("forward", "add", "myhost", "-R", "1080"); !strings.Contains(out, "Added -R 127.0.0.1:1080 to tunnel myhost.") {
		t.Fatalf("unexpected output %q", out)
	}
	e.mustGst("forward", "remove", "myhost", "-R", "127.0.0.1:1080", "-D", "1080")
	if fwds := e.tunnel("myhost").Forwards; len(fwds) != 0 {
		t.Fatalf("forwards left: %+v", fwds)
	}
}

func TestLifecycle(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "myhost")
	e.mustGst("add", "db")

	if out := e.mustGst("start", "myhost"); !strings.Contains(out, "already started") {
		t.Fatalf("unexpected output %q", out)
	}
	e.mustGst("stop", "myhost")
	if out := e.mustGst("stop", "myhost"); !strings.Contains(out, "already stopped") {
		t.Fatalf("unexpected output %q", out)
	}
	e.expectCode(exitFailure, "restart", "myhost")

	e.services.calls = nil
	e.mustGst("restart", "--all")
	if !slices.Equal(e.services.calls, []string{"start db"}) {
		t.Fatalf("restart --all must only restart started tunnels: %v", e.services.calls)
	}
	e.services.calls = nil
	e.mustGst("start", "--all")
	if !slices.Equal(e.services.calls, []string{"start myhost"}) {
		t.Fatalf("start --all must only start stopped tunnels: %v", e.services.calls)
	}
	e.expectCode(exitUsage, "start")
	e.expectCode(exitUsage, "start", "myhost", "--all")
	e.expectCode(exitFailure, "stop", "myhost", "nope")

	e.services.status["db"] = service.Down
	var views []tunnelView
	if err := json.Unmarshal([]byte(e.mustGst("list", "--json")), &views); err != nil || len(views) != 2 ||
		views[0].Name != "db" || views[0].Status != service.Down || views[1].Status != service.Running {
		t.Fatalf("unexpected list %+v: %v", views, err)
	}

	e.services.calls = nil
	e.mustGst("rm", "myhost", "db")
	if f, _ := config.Load(e.config); len(f.Tunnels) != 0 || !slices.Equal(e.services.calls, []string{"stop myhost", "remove-logs myhost", "stop db", "remove-logs db"}) {
		t.Fatalf("remove left %+v, calls %v", f.Tunnels, e.services.calls)
	}
	if out := e.mustGst("list"); !strings.Contains(out, "No tunnels") {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestSet(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "myhost", "--identity", e.identity, "--keepalive-count", "5")
	e.expectCode(exitUsage, "set", "myhost")
	e.expectCode(exitUsage, "set", "myhost", "--keepalive-count", "-1")
	e.expectCode(exitFailure, "set", "myhost", "--destination", "unknown")
	if e.tunnel("myhost").Destination != "myhost" {
		t.Fatal("failed set changed the tunnel")
	}

	e.services.calls = nil
	out := e.mustGst("set", "myhost", "--identity", "", "--keepalive-count", "0", "--destination", "db")
	got := e.tunnel("myhost")
	if got.IdentityFile != "" || got.KeepaliveCountMax != 0 || got.Destination != "db" {
		t.Fatalf("set did not apply: %+v", got)
	}
	if !strings.Contains(out, "Restarted tunnel myhost") || !slices.Equal(e.services.calls, []string{"start myhost"}) {
		t.Fatalf("running tunnel not restarted: %q %v", out, e.services.calls)
	}
	e.services.calls = nil
	if out := e.mustGst("set", "myhost", "--cleanup-listeners", "--no-restart"); !strings.Contains(out, "gst restart myhost") || len(e.services.calls) != 0 {
		t.Fatalf("--no-restart restarted: %q %v", out, e.services.calls)
	}
	if out := e.mustGst("set", "myhost", "--cleanup-listeners", "--keepalive-count", "0"); !strings.Contains(out, "nothing changed") || len(e.services.calls) != 0 {
		t.Fatalf("an unchanged tunnel must not restart: %q %v", out, e.services.calls)
	}
	e.mustGst("set", "myhost", "--cleanup-listeners=false")
	if e.tunnel("myhost").CleanupListeners {
		t.Fatal("--cleanup-listeners=false did not turn cleanup off")
	}
}

func TestShowAndCheck(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "myhost", "-R", "2222:localhost:22")
	var v tunnelView
	if err := json.Unmarshal([]byte(e.mustGst("show", "myhost", "--json")), &v); err != nil {
		t.Fatal(err)
	}
	if v.Resolved == nil || v.Resolved.Server != "ssh.example.com:22" || v.Resolved.KeepaliveTimeoutSeconds != 45 || len(v.Forwards) != 1 {
		t.Fatalf("unexpected view %+v", v)
	}
	if out := e.mustGst("show", "myhost"); !strings.Contains(out, "tunnel@ssh.example.com:22") {
		t.Fatalf("unexpected output %q", out)
	}

	e.mustGst("add", "closed")
	code, stdout, _ := e.gst("check", "closed", "--json")
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

	e.mustGst("logs", "myhost", "-n", "5")
	e.expectCode(exitUsage, "logs", "myhost", "-n", "0")
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
