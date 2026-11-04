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
	"github.com/yangtau/ssh-tunnel/internal/sshconfig"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// fakeBackend is a daemon that only records state. Sync runs the started
// tunnels of the configuration file.
type fakeBackend struct {
	config    string
	installed bool
	command   []string                    // last daemon command
	running   map[string]bool             // tunnels the daemon runs
	states    map[string]wire.TunnelState // overrides the running state
	legacy    map[string]bool             // gst 0.1 services
	topology  wire.Topology
	calls     []string
	sent      bytes.Buffer // what gst connect sent
}

// fakeConn keeps what gst connect sends and, like an echo server, answers
// once gst connect has sent everything.
type fakeConn struct {
	reply io.Reader
	sent  *bytes.Buffer
	done  chan struct{}
}

func (c *fakeConn) Read(p []byte) (int, error) {
	<-c.done
	return c.reply.Read(p)
}
func (c *fakeConn) Write(p []byte) (int, error) { return c.sent.Write(p) }
func (c *fakeConn) CloseWrite() error           { close(c.done); return nil }
func (c *fakeConn) Close() error                { return nil }

func (f *fakeBackend) Sync(_ context.Context, command []string) error {
	f.calls = append(f.calls, "sync")
	f.installed, f.command, f.running = command != nil, command, map[string]bool{}
	if command == nil {
		return nil
	}
	cfg, err := config.Load(f.config)
	if err != nil {
		return err
	}
	for name, t := range cfg.Tunnels {
		if !t.Stopped {
			f.running[name] = true
		}
	}
	return nil
}

func (f *fakeBackend) Restart(_ context.Context, names []string) error {
	f.calls = append(f.calls, "restart "+strings.Join(names, " "))
	return nil
}

func (f *fakeBackend) States(context.Context) (map[string]wire.TunnelState, bool) {
	if !f.installed {
		return nil, false
	}
	states := map[string]wire.TunnelState{}
	for name := range f.running {
		states[name] = wire.TunnelState{Name: name, State: wire.StateRunning}
		if s, ok := f.states[name]; ok {
			states[name] = s
		}
	}
	return states, true
}

func (f *fakeBackend) RemoveLogs(name string) error {
	f.calls = append(f.calls, "remove-logs "+name)
	return nil
}

func (f *fakeBackend) Logs(_ context.Context, name string, lines int, follow bool, w io.Writer) error {
	f.calls = append(f.calls, "logs "+name)
	return nil
}

func (f *fakeBackend) Peers(context.Context) (wire.Peers, error) {
	f.calls = append(f.calls, "peers")
	return wire.Peers{Enabled: true, Links: []wire.PeerLink{}}, nil
}

func (f *fakeBackend) Nodes(context.Context) (wire.Topology, error) {
	f.calls = append(f.calls, "nodes")
	return f.topology, nil
}

func (f *fakeBackend) Connect(_ context.Context, node string, target wire.Target) (io.ReadWriteCloser, error) {
	f.calls = append(f.calls, "connect "+node+" "+target.Network+" "+target.Address)
	if node == "nowhere" {
		return nil, errors.New("no machine \"nowhere\" in the mesh")
	}
	return &fakeConn{reply: strings.NewReader("hello from " + node), sent: &f.sent, done: make(chan struct{})}, nil
}

func (f *fakeBackend) HasLegacy(name string) bool { return f.legacy[name] }

func (f *fakeBackend) RemoveLegacy(name string) error {
	f.calls = append(f.calls, "remove-legacy "+name)
	delete(f.legacy, name)
	return nil
}

type testEnv struct {
	t        *testing.T
	config   string
	back     *fakeBackend
	hosts    map[string]sshconfig.Host // ssh -G results by destination
	identity string
	exe      string
	stdin    string // standard input of the next commands
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
		identity: filepath.Join(dir, "id_ed25519"),
		exe:      "/usr/local/bin/gst",
	}
	e.back = &fakeBackend{config: e.config, states: map[string]wire.TunnelState{}, legacy: map[string]bool{}}
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
	a := e.app(&stdout, &stderr)
	code := a.execute(context.Background(), append([]string{"--config", e.config}, args...))
	return code, stdout.String(), stderr.String()
}

// app returns the command line app as e.gst runs it, without --config.
func (e *testEnv) app(stdout, stderr io.Writer) *app {
	return &app{
		configFlag: e.config,
		stdin:      strings.NewReader(e.stdin),
		stdout:     stdout,
		stderr:     stderr,
		newBackend: func() (backend, error) { return e.back, nil },
		resolve: func(_ context.Context, destination string) (sshconfig.Host, error) {
			if h, ok := e.hosts[destination]; ok {
				return h, nil
			}
			return sshconfig.Host{}, errors.New("ssh -G " + destination + ": no such host")
		},
		executable: func() (string, error) { return e.exe, nil },
	}
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
	if !slices.Equal(e.back.command, []string{e.exe, "--config", e.config, "daemon"}) || !e.back.running["myhost"] {
		t.Fatalf("daemon command %v, running %v", e.back.command, e.back.running)
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
	if f, _ := config.Load(e.config); len(f.Tunnels) != 0 || len(e.back.calls) != 0 {
		t.Fatalf("failed adds changed state: %+v %v", f.Tunnels, e.back.calls)
	}
}

func TestForwards(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "myhost")
	e.back.calls = nil

	out := e.mustGst("forward", "add", "myhost", "-R", "*:2222:localhost:22", "-L", "15432:127.0.0.1:5432")
	if !strings.Contains(out, "Added -R 0.0.0.0:2222:localhost:22") || !strings.Contains(out, "Restarted tunnel myhost") {
		t.Fatalf("unexpected output %q", out)
	}
	if !slices.Equal(e.back.calls, []string{"sync", "restart myhost"}) {
		t.Fatalf("running tunnel not restarted once: %v", e.back.calls)
	}

	e.back.calls = nil
	out = e.mustGst("forward", "add", "myhost", "-R", "0.0.0.0:2222:localhost:22")
	if !strings.Contains(out, "already has") || len(e.back.calls) != 0 {
		t.Fatalf("adding an existing forward must be a no-op: %q %v", out, e.back.calls)
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
	e.back.calls = nil
	out = e.mustGst("forward", "rm", "myhost", "-L", "15432")
	if !strings.Contains(out, "Removed -L 127.0.0.1:15432:127.0.0.1:5432") || !strings.Contains(out, "stopped") || len(e.back.calls) != 0 {
		t.Fatalf("removing from a stopped tunnel: %q %v", out, e.back.calls)
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
	e.back.calls = nil
	if out := e.mustGst("forward", "add", "myhost", "-R", "127.0.0.1:1080"); !strings.Contains(out, "already has") || len(e.back.calls) != 0 {
		t.Fatalf("duplicate must be a no-op: %q %v", out, e.back.calls)
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

	if !e.tunnel("myhost").Stopped || e.back.running["myhost"] || !e.back.running["db"] {
		t.Fatalf("stop did not reach the daemon: %+v %v", e.tunnel("myhost"), e.back.running)
	}

	e.back.calls = nil
	e.mustGst("restart", "--all")
	if !slices.Equal(e.back.calls, []string{"sync", "restart db"}) {
		t.Fatalf("restart --all must only restart started tunnels: %v", e.back.calls)
	}
	e.back.calls = nil
	if out := e.mustGst("start", "--all"); out != "Started tunnel myhost.\n" || !e.back.running["myhost"] {
		t.Fatalf("start --all must only start stopped tunnels: %q %v", out, e.back.running)
	}
	e.expectCode(exitUsage, "start")
	e.expectCode(exitUsage, "start", "myhost", "--all")
	e.expectCode(exitFailure, "stop", "myhost", "nope")

	e.back.states["db"] = wire.TunnelState{Name: "db", State: wire.StateDown, Error: "SSH keepalive timed out"}
	var views []tunnelView
	if err := json.Unmarshal([]byte(e.mustGst("list", "--json")), &views); err != nil || len(views) != 2 ||
		views[0].Name != "db" || views[0].Status != statusDown || views[0].Error != "SSH keepalive timed out" || views[1].Status != statusRunning {
		t.Fatalf("unexpected list %+v: %v", views, err)
	}
	if out := e.mustGst("show", "db"); !strings.Contains(out, "down (last error: SSH keepalive timed out)") {
		t.Fatalf("show lacks the last error: %q", out)
	}

	// Without a daemon, started tunnels are down.
	e.back.installed = false
	if err := json.Unmarshal([]byte(e.mustGst("list", "--json")), &views); err != nil || views[1].Status != statusDown {
		t.Fatalf("unexpected list without a daemon %+v: %v", views, err)
	}

	e.back.calls = nil
	e.mustGst("rm", "myhost", "db")
	if f, _ := config.Load(e.config); len(f.Tunnels) != 0 || !slices.Equal(e.back.calls, []string{"sync", "remove-logs myhost", "remove-logs db"}) || e.back.installed {
		t.Fatalf("remove left %+v, calls %v, daemon installed %v", f.Tunnels, e.back.calls, e.back.installed)
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

	e.back.calls = nil
	out := e.mustGst("set", "myhost", "--identity", "", "--keepalive-count", "0", "--destination", "db")
	got := e.tunnel("myhost")
	if got.IdentityFile != "" || got.KeepaliveCountMax != 0 || got.Destination != "db" {
		t.Fatalf("set did not apply: %+v", got)
	}
	if !strings.Contains(out, "Restarted tunnel myhost") || !slices.Equal(e.back.calls, []string{"sync", "restart myhost"}) {
		t.Fatalf("running tunnel not restarted: %q %v", out, e.back.calls)
	}
	e.back.calls = nil
	if out := e.mustGst("set", "myhost", "--cleanup-listeners", "--no-restart"); !strings.Contains(out, "gst restart myhost") || len(e.back.calls) != 0 {
		t.Fatalf("--no-restart restarted: %q %v", out, e.back.calls)
	}
	if out := e.mustGst("set", "myhost", "--cleanup-listeners", "--keepalive-count", "0"); !strings.Contains(out, "nothing changed") || len(e.back.calls) != 0 {
		t.Fatalf("an unchanged tunnel must not restart: %q %v", out, e.back.calls)
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

func TestStopAllRemovesDaemon(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "myhost")
	e.mustGst("add", "db")
	e.mustGst("stop", "--all")
	if e.back.installed {
		t.Fatal("the daemon stays installed without started tunnels")
	}
	e.mustGst("start", "db")
	if !e.back.installed || !e.back.running["db"] || e.back.running["myhost"] {
		t.Fatalf("start did not bring the daemon up: %v", e.back.running)
	}
}

func TestMigratesLegacyServices(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(filepath.Dir(e.config), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, e.config, `{"tunnels": {"myhost": {"destination": "myhost", "forwards": []}, "db": {"destination": "db", "forwards": []}}}`)
	e.back.legacy["myhost"] = true

	code, stdout, stderr := e.gst("list", "--json")
	if code != 0 || !strings.Contains(stderr, "note: the gst daemon now runs tunnels myhost") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	var views []tunnelView
	if err := json.Unmarshal([]byte(stdout), &views); err != nil || views[0].Status != statusStopped || views[1].Status != statusRunning {
		t.Fatalf("unexpected list %+v: %v", views, err)
	}
	if !slices.Equal(e.back.calls, []string{"remove-legacy myhost", "sync"}) {
		t.Fatalf("unexpected calls %v", e.back.calls)
	}
	if f, _ := config.Load(e.config); f.Version != config.Version {
		t.Fatalf("version %d after migration", f.Version)
	}

	// Migration happens once; help and completion never migrate.
	e.back.calls = nil
	e.mustGst("list")
	e.mustGst("version")
	if len(e.back.calls) != 0 {
		t.Fatalf("migrated again: %v", e.back.calls)
	}

	// A service left behind by an interrupted migration goes later.
	e.back.legacy["db"] = true
	e.mustGst("list")
	if !slices.Equal(e.back.calls, []string{"remove-legacy db", "sync"}) || e.back.legacy["db"] {
		t.Fatalf("leftover service: calls %v", e.back.calls)
	}
}

func TestMesh(t *testing.T) {
	e := newEnv(t)
	if stderr := e.expectCode(exitFailure, "mesh", "peers"); !strings.Contains(stderr, "gst mesh enable") {
		t.Fatalf("peers without the mesh: %q", stderr)
	}
	e.expectCode(exitUsage, "mesh", "enable", "--name", "Bad Name")

	out := e.mustGst("mesh", "enable", "--name", "laptop")
	f, _ := config.Load(e.config)
	if f.Mesh == nil || f.Mesh.Name != "laptop" || len(f.Mesh.ID) != 32 || !strings.Contains(out, "this machine is laptop ("+f.Mesh.ID[:8]+")") {
		t.Fatalf("mesh %+v, output %q", f.Mesh, out)
	}
	// The mesh keeps the daemon running without started tunnels.
	if !e.back.installed {
		t.Fatal("the daemon is not installed for the mesh")
	}
	id := f.Mesh.ID
	e.mustGst("mesh", "enable", "--name", "desk")
	if f, _ := config.Load(e.config); f.Mesh.ID != id || f.Mesh.Name != "desk" {
		t.Fatalf("renaming changed the id: %+v", f.Mesh)
	}
	e.mustGst("mesh", "peers", "--json")

	e.back.topology = wire.Topology{Enabled: true, Self: "aaaa1111aaaa1111", Nodes: []wire.TopoNode{
		{ID: "aaaa1111aaaa1111", Name: "mini", Gst: true, Software: "gst 0.2.0", Reachable: true},
		{ID: "bbbb2222bbbb2222", Name: "m3", Gst: true, Software: "gst 0.2.0", Reachable: true},
		{ID: "cccc3333cccc3333", Name: "web", Gst: true, Software: "gst 0.2.0"},
		{ID: "ssh:SHA256:abc", Name: "web", Reachable: true},
	}, Edges: []wire.TopoEdge{
		{From: "bbbb2222bbbb2222", To: "aaaa1111aaaa1111", Tunnel: "mini", State: "running", Linked: true},
		{From: "bbbb2222bbbb2222", To: "ssh:SHA256:abc", Tunnel: "web", State: "running"},
	}}
	out = e.mustGst("mesh", "nodes")
	for _, want := range []string{
		"mini            aaaa1111  gst 0.2.0  this machine",
		"web (cccc3333)  cccc3333  gst 0.2.0  no",
		"web             -         -          yes",
		"m3    mini  mini    running, peer link up",
		"m3    web   web     running",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("mesh nodes lacks %q:\n%s", want, out)
		}
	}
	if out := e.mustGst("mesh", "nodes", "--json"); !strings.Contains(out, `"self": "aaaa1111aaaa1111"`) {
		t.Fatalf("mesh nodes --json: %s", out)
	}

	e.mustGst("add", "myhost", "--remote-gst", "/opt/gst/bin/gst")
	if tn := e.tunnel("myhost"); tn.NoMesh || tn.RemoteGst != "/opt/gst/bin/gst" {
		t.Fatalf("mesh settings not saved: %+v", tn)
	}
	if out := e.mustGst("show", "myhost"); !strings.Contains(out, "while the mesh is enabled; remote gst /opt/gst/bin/gst") {
		t.Fatalf("show lacks mesh settings: %q", out)
	}
	e.mustGst("set", "myhost", "--no-mesh", "--remote-gst", "")
	if tn := e.tunnel("myhost"); !tn.NoMesh || tn.RemoteGst != "" {
		t.Fatalf("mesh settings not cleared: %+v", tn)
	}
	if out := e.mustGst("show", "myhost"); !strings.Contains(out, "off (--no-mesh)") {
		t.Fatalf("show lacks --no-mesh: %q", out)
	}

	e.mustGst("stop", "myhost")
	if !e.back.installed {
		t.Fatal("stopping the last tunnel removed the daemon of the mesh")
	}
	e.mustGst("mesh", "disable")
	if f, _ := config.Load(e.config); f.Mesh != nil || e.back.installed {
		t.Fatalf("disable left mesh %+v, daemon installed %v", f.Mesh, e.back.installed)
	}
	if out := e.mustGst("mesh", "disable"); !strings.Contains(out, "already disabled") {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestDefaultNodeName(t *testing.T) {
	if err := config.ValidateNodeName(defaultNodeName()); err != nil {
		t.Fatal(err)
	}
}

func TestMeshConnectAndTunnels(t *testing.T) {
	e := newEnv(t)
	if stderr := e.expectCode(exitFailure, "add", "db-mesh", "mesh:devcn", "-L", "15432:localhost:5432"); !strings.Contains(stderr, "gst mesh enable") {
		t.Fatalf("via mesh without the mesh: %q", stderr)
	}
	e.mustGst("mesh", "enable", "--name", "mini")

	// gst connect pipes standard input and output.
	e.stdin = "ping"
	if out := e.mustGst("connect", "devcn", "5432"); out != "hello from devcn" || e.back.sent.String() != "ping" {
		t.Fatalf("connect printed %q and sent %q", out, e.back.sent.String())
	}
	e.mustGst("connect", "devcn", "db.internal:5432")
	e.mustGst("connect", "devcn", "/run/app.sock")
	for _, want := range []string{"connect devcn tcp localhost:5432", "connect devcn tcp db.internal:5432", "connect devcn unix /run/app.sock"} {
		if !slices.Contains(e.back.calls, want) {
			t.Errorf("missing call %q in %q", want, e.back.calls)
		}
	}
	e.expectCode(exitUsage, "connect", "devcn", "70000")
	e.expectCode(exitUsage, "connect", "devcn", "relative/sock")
	if stderr := e.expectCode(exitFailure, "connect", "nowhere", "22"); !strings.Contains(stderr, "gst mesh nodes") {
		t.Fatalf("connect to a missing machine: %q", stderr)
	}

	// A tunnel through the mesh has no SSH settings and only -L and -D.
	e.mustGst("add", "db-mesh", "mesh:devcn", "-L", "15432:localhost:5432", "-D", "1080")
	if tn := e.tunnel("db-mesh"); tn.Destination != "mesh:devcn" || len(tn.Forwards) != 2 {
		t.Fatalf("tunnel %+v", tn)
	}
	if !e.back.running["db-mesh"] {
		t.Fatal("the tunnel through the mesh did not start")
	}
	e.expectCode(exitUsage, "add", "bad", "mesh:devcn", "-R", "2222:localhost:22")
	e.expectCode(exitUsage, "add", "bad", "mesh:devcn", "--no-mesh")
	e.expectCode(exitUsage, "add", "bad", "mesh:devcn", "--identity", "/tmp/key")
	e.expectCode(exitUsage, "add", "bad", "mesh:")
	e.expectCode(exitUsage, "add", "bad", "mesh:Bad_Name")
	e.expectCode(exitUsage, "forward", "add", "db-mesh", "-R", "2222:localhost:22")
	if out := e.mustGst("show", "db-mesh"); !strings.Contains(out, "connects through the gst mesh from devcn") || strings.Contains(out, "Cleanup") {
		t.Fatalf("show: %s", out)
	}

	e.back.topology = wire.Topology{Enabled: true, Self: "aaaa1111", Nodes: []wire.TopoNode{
		{ID: "aaaa1111", Name: "mini", Gst: true, Reachable: true},
		{ID: "bbbb2222", Name: "m3", Gst: true, Reachable: true},
		{ID: "ssh:SHA256:devcn", Name: "devcn", Reachable: true},
		{ID: "cccc3333", Name: "far", Gst: true},
	}, Edges: []wire.TopoEdge{
		{From: "bbbb2222", To: "aaaa1111", Tunnel: "mini", State: "running", Linked: true},
		{From: "bbbb2222", To: "ssh:SHA256:devcn", Tunnel: "devcn", State: "running"},
	}}
	if out := e.mustGst("check", "db-mesh"); !strings.Contains(out, "mini → m3 → devcn") {
		t.Fatalf("check: %s", out)
	}
	e.mustGst("set", "db-mesh", "--destination", "mesh:far")
	if code, out, _ := e.gst("check", "db-mesh"); code != exitFailure || !strings.Contains(out, "no route to far") {
		t.Fatalf("check exited %d: %s", code, out)
	}
}
