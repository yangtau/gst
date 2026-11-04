package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/sshconfig"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// fakeBackend is a daemon that only records state. Once Sync has installed
// it, it reports what the configuration file asks it to run: every started
// link as running, the proxy, and the maps as listening.
type fakeBackend struct {
	config    string
	installed bool
	command   []string // the last daemon command
	calls     []string

	syncErr    error
	restartErr error
	nodesErr   error

	linkStates   map[string]wire.LinkState // replaces the state of a running link
	nodeStatus   func(node string) (wire.Status, error)
	listenErrors map[string]string // listen address -> why it does not listen
	topology     wire.Topology
	logOutput    string
	sent         bytes.Buffer // what gst connect sent
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
	if command == nil {
		f.calls = append(f.calls, "sync (uninstall)")
	} else {
		f.calls = append(f.calls, "sync")
	}
	if f.syncErr != nil {
		return f.syncErr
	}
	f.installed, f.command = command != nil, command
	return nil
}

func (f *fakeBackend) Restart(_ context.Context, names []string) error {
	f.calls = append(f.calls, "restart "+strings.Join(names, " "))
	return f.restartErr
}

// peerOf is the node that a running link to name reaches.
func peerOf(name string) wire.NodeInfo {
	return wire.NodeInfo{
		ID:       fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(name))) + strings.Repeat("0", 24),
		Name:     name,
		Software: "gst 0.3.0",
	}
}

func (f *fakeBackend) Status(context.Context) (wire.Status, bool) {
	if !f.installed {
		return wire.Status{}, false
	}
	cfg, err := config.Load(f.config)
	if err != nil {
		return wire.Status{}, false
	}
	st := wire.Status{Software: software, Links: []wire.LinkState{}, Maps: []wire.MapState{}}
	if cfg.Node != nil {
		st.Node = wire.NodeInfo{ID: cfg.Node.ID, Name: cfg.Node.Name, Exit: cfg.Node.Exit}
	}
	listener := func(listen string) wire.Listener {
		return wire.Listener{Listen: listen, Error: f.listenErrors[listen]}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Links)) {
		if cfg.Links[name].Stopped {
			continue
		}
		peer := peerOf(name)
		state := wire.LinkState{Name: name, State: wire.StateRunning, Peer: &peer}
		if s, ok := f.linkStates[name]; ok {
			state = s
		}
		st.Links = append(st.Links, state)
	}
	if address := cfg.ProxyAddress(); address != "" {
		st.Proxy = listener(address)
	}
	for _, m := range cfg.Maps {
		st.Maps = append(st.Maps, wire.MapState{Listener: listener(m.Listen), Machine: m.Machine, Target: m.Target})
	}
	return st, true
}

func (f *fakeBackend) NodeStatus(ctx context.Context, node string) (wire.Status, error) {
	f.calls = append(f.calls, "node-status "+node)
	if f.nodeStatus != nil {
		return f.nodeStatus(node)
	}
	st, ok := f.Status(ctx)
	if !ok {
		return wire.Status{}, errors.New("no daemon")
	}
	return st, nil
}

func (f *fakeBackend) Nodes(context.Context) (wire.Topology, error) {
	f.calls = append(f.calls, "nodes")
	if !f.installed {
		return wire.Topology{}, errors.New("no daemon")
	}
	return f.topology, f.nodesErr
}

func (f *fakeBackend) Connect(_ context.Context, node string, target wire.Target) (io.ReadWriteCloser, error) {
	f.calls = append(f.calls, "connect "+node+" "+target.Network+" "+target.Address)
	if node == "nowhere" {
		return nil, errors.New(`no machine "nowhere" in the mesh`)
	}
	return &fakeConn{reply: strings.NewReader("hello from " + node), sent: &f.sent, done: make(chan struct{})}, nil
}

func (f *fakeBackend) Logs(_ context.Context, name string, lines int, follow bool, w io.Writer) error {
	f.calls = append(f.calls, fmt.Sprintf("logs %s %d %v", name, lines, follow))
	_, err := io.WriteString(w, f.logOutput)
	return err
}

func (f *fakeBackend) RemoveLogs(name string) error {
	f.calls = append(f.calls, "remove-logs "+name)
	return nil
}

type testEnv struct {
	t        *testing.T
	config   string
	back     *fakeBackend
	hosts    map[string]sshconfig.Host // ssh -G results by destination
	identity string
	known    string // a known_hosts file that records ssh.example.com
	exe      string
	stdin    string // standard input of the next commands
}

// newEnv returns an environment where the destinations myhost, db, and
// devcn resolve to a server with a usable identity and a known host key,
// unknown to one whose host key is not recorded, and nowhere to nothing.
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
		known:    filepath.Join(dir, "known_hosts"),
		exe:      "/usr/local/bin/gst",
	}
	e.back = &fakeBackend{config: e.config, linkStates: map[string]wire.LinkState{}, listenErrors: map[string]string{}}
	write(t, e.identity, string(pem.EncodeToMemory(block)))
	write(t, e.known, knownhosts.Line([]string{"ssh.example.com:22"}, signer.PublicKey())+"\n")
	host := sshconfig.Host{
		Hostname: "ssh.example.com", Port: "22", User: "tunnel",
		IdentityFiles: []string{e.identity}, KnownHostsFiles: []string{e.known}, ServerAliveCountMax: 3,
	}
	unknown := host
	unknown.Hostname = "unknown.example.com"
	e.hosts = map[string]sshconfig.Host{"myhost": host, "db": host, "devcn": host, "unknown": unknown}
	return e
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
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

// expectCode runs the command line, expects exit code want, and returns stderr.
func (e *testEnv) expectCode(want int, args ...string) string {
	e.t.Helper()
	code, stdout, stderr := e.gst(args...)
	if code != want {
		e.t.Fatalf("gst %v exited %d, want %d\nstdout: %s\nstderr: %s", args, code, want, stdout, stderr)
	}
	return stderr
}

// expectError expects the command line to fail with code and the lines in want
// among its stderr.
func (e *testEnv) expectError(code int, args []string, want ...string) {
	e.t.Helper()
	stderr := e.expectCode(code, args...)
	for _, w := range want {
		if !strings.Contains(stderr, w) {
			e.t.Errorf("gst %v: stderr %q lacks %q", args, stderr, w)
		}
	}
}

func (e *testEnv) load() *config.File {
	e.t.Helper()
	f, err := config.Load(e.config)
	if err != nil {
		e.t.Fatal(err)
	}
	return f
}

func (e *testEnv) link(name string) *config.Link {
	e.t.Helper()
	return e.load().Links[name]
}

func (e *testEnv) resetCalls() { e.back.calls = nil }

func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("invalid JSON %q: %v", s, err)
	}
	return v
}

func contains(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%q lacks %q", got, w)
		}
	}
}

// rows splits table output into rows of whitespace-separated fields.
func rows(out string) [][]string {
	var rs [][]string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		rs = append(rs, strings.Fields(line))
	}
	return rs
}

// --- the error contract ---

func TestExitCodesAndErrorContract(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "myhost")
	e.mustGst("link", "add", "db")

	t.Run("unknown link is a failure with a hint", func(t *testing.T) {
		code, stdout, stderr := e.gst("link", "show", "nope")
		if code != exitFailure || stdout != "" {
			t.Fatalf("code %d, stdout %q", code, stdout)
		}
		if want := "error: no link named nope\nhint: links: db, myhost\n"; stderr != want {
			t.Fatalf("stderr %q, want %q", stderr, want)
		}
	})

	t.Run("usage errors print usage and exit 2", func(t *testing.T) {
		for args, want := range map[string][]string{
			"link show":     {"error: missing argument <name>\n", "usage: gst link show <name>", "hint: links: db, myhost\n", "Run 'gst link show --help'"},
			"link remove":   {"error: missing argument <name>\n", "usage: gst link remove <name>...", "hint: links: db, myhost\n"},
			"link start":    {"error: missing argument <name> or --all\n", "hint: links: db, myhost\n"},
			"link list x":   {`error: unexpected argument "x"`, "usage: gst link list"},
			"nodes x":       {`error: unexpected argument "x"`, "usage: gst nodes"},
			"link lst":      {`unknown command "lst" for "gst link"`, `did you mean "set" or "list"?`, "usage: gst link <command>\n"},
			"lnk":           {`unknown command "lnk"`, "usage: gst <command>\n"},
			"frobnicate":    {`unknown command "frobnicate"`, "Run 'gst --help'"},
			"map add 1":     {"error: missing argument <machine>:<target>\n", "usage: gst map add <listen> <machine>:<target>"},
			"route add x":   {"error: missing argument <machine>\n", "usage: gst route add <match> <machine>"},
			"link --nope":   {"error: unknown flag: --nope\n", "usage: gst link"},
			"link add":      {"error: missing argument <name>\n", "usage: gst link add <name> [destination]"},
			"exit sideways": {`error: invalid argument "sideways": use on or off`, "usage: gst exit [on | off]"},
		} {
			stderr := e.expectCode(exitUsage, strings.Fields(args)...)
			for _, w := range want {
				if !strings.Contains(stderr, w) {
					t.Errorf("gst %s: stderr %q lacks %q", args, stderr, w)
				}
			}
			if strings.HasPrefix(args, "link add") && strings.Contains(stderr, "hint:") {
				t.Errorf("gst %s: a new link name needs no list of links: %q", args, stderr)
			}
		}
	})

	t.Run("a group alone prints its help", func(t *testing.T) {
		out := e.mustGst("link")
		contains(t, out, "Usage:\n  gst link <command>\n", "Available Commands:")
		contains(t, e.mustGst("map"), "Usage:\n  gst map <command>\n")
		contains(t, e.mustGst("route"), "Usage:\n  gst route <command>\n")
	})

	t.Run("a failed check exits 1 without a message", func(t *testing.T) {
		delete(e.hosts, "db")
		defer func() { e.hosts["db"] = e.hosts["myhost"] }()
		code, _, stderr := e.gst("link", "check", "db")
		if code != exitFailure || stderr != "" {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
	})
}

func TestEveryCommandNamesItsMissingArguments(t *testing.T) {
	e := newEnv(t)
	for args, want := range map[string]string{
		"link add":         "<name>",
		"link set":         "<name>",
		"link remove":      "<name>",
		"link show":        "<name>",
		"link logs":        "<name>",
		"link check":       "<name>",
		"connect":          "<machine>",
		"map add":          "<listen>",
		"map remove":       "<listen>",
		"route add":        "<match>",
		"route remove":     "<match>",
		"link start":       "<name> or --all",
		"link stop":        "<name> or --all",
		"link restart":     "<name> or --all",
		"map add 15432":    "<machine>:<target>",
		"route add corp.x": "<machine>",
	} {
		stderr := e.expectCode(exitUsage, strings.Fields(args)...)
		if !strings.Contains(stderr, "error: missing argument "+want+"\n") {
			t.Errorf("gst %s: stderr %q does not name %s", args, stderr, want)
		}
	}
}

func TestConfigFromEnvironment(t *testing.T) {
	e := newEnv(t)
	t.Setenv("GST_CONFIG", e.config)
	var stdout, stderr bytes.Buffer
	a := e.app(&stdout, &stderr)
	a.configFlag = ""
	if code := a.execute(context.Background(), []string{"link", "add", "myhost"}); code != exitOK {
		t.Fatalf("exit %d: %s", code, &stderr)
	}
	if e.link("myhost") == nil {
		t.Fatal("GST_CONFIG was not used")
	}
	// A non-default path is passed on to the daemon.
	if !slices.Equal(e.back.command, []string{e.exe, "--config", e.config, "daemon"}) {
		t.Fatalf("daemon command %v", e.back.command)
	}
}

func TestInvalidConfigIsReported(t *testing.T) {
	e := newEnv(t)
	write(t, e.config, "{not json")
	stderr := e.expectCode(exitFailure, "link", "list")
	if !strings.HasPrefix(stderr, "error: config ") || !strings.Contains(stderr, "invalid JSON") {
		t.Fatalf("stderr %q", stderr)
	}
	write(t, e.config, `{"version":3,"surprise":true}`)
	e.expectCode(exitFailure, "nodes")
}

// --- link add ---

func TestLinkAdd(t *testing.T) {
	e := newEnv(t)
	out := e.mustGst("link", "add", "myhost",
		"--keepalive-interval", "30s", "--connect-timeout", "20s", "--keepalive-count", "5",
		"--identity", e.identity, "--known-hosts", e.known, "--remote-gst", "~/bin/gst")
	if want := "Added link myhost; it connects in the background. See it with: gst link list\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	want := &config.Link{
		Destination:              "myhost",
		IdentityFile:             e.identity,
		KnownHostsFile:           e.known,
		ConnectTimeoutSeconds:    20,
		KeepaliveIntervalSeconds: 30,
		KeepaliveCountMax:        5,
		RemoteGst:                "~/bin/gst",
	}
	if got := e.link("myhost"); got == nil || *got != *want {
		t.Fatalf("saved %+v, want %+v", got, want)
	}
	if !slices.Equal(e.back.command, []string{e.exe, "--config", e.config, "daemon"}) || !e.back.installed {
		t.Fatalf("daemon command %v, installed %v", e.back.command, e.back.installed)
	}
	if !slices.Equal(e.back.calls, []string{"sync"}) {
		t.Fatalf("calls %v", e.back.calls)
	}
	if e.load().Node == nil {
		t.Fatal("the first change gives the machine an identity")
	}

	e.mustGst("link", "add", "home", "db")
	if got := e.link("home"); got.Destination != "db" || got.Stopped {
		t.Fatalf("explicit destination not saved: %+v", got)
	}
	e.hosts["ssh://deploy@ssh.example.com:2200"] = e.hosts["myhost"]
	e.mustGst("link", "add", "web", "ssh://deploy@ssh.example.com:2200")
	if got := e.link("web").Destination; got != "ssh://deploy@ssh.example.com:2200" {
		t.Fatalf("destination %q", got)
	}
}

func TestLinkAddDuplicate(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "myhost")
	e.resetCalls()
	e.expectError(exitFailure, []string{"link", "add", "myhost", "--keepalive-interval", "5s"},
		"error: link myhost already exists\n", "hint: change it with `gst link set myhost`, or remove it with `gst link remove myhost`\n")
	if len(e.back.calls) != 0 || e.link("myhost").KeepaliveIntervalSeconds != 0 {
		t.Fatalf("a duplicate changed state: %v %+v", e.back.calls, e.link("myhost"))
	}
}

func TestLinkAddRejects(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{
		{"link", "add"},
		{"link", "add", "MyHost"},
		{"link", "add", "-bad"},
		{"link", "add", "my_host"},
		{"link", "add", strings.Repeat("a", 33)},
		{"link", "add", "myhost", "--", "-oProxyCommand=x"},
		{"link", "add", "myhost", "a", "b"},
		{"link", "add", "myhost", "--connect-timeout", "1500ms"},
		{"link", "add", "myhost", "--keepalive-interval", "-5s"},
		{"link", "add", "myhost", "--keepalive-count", "-1"},
		{"link", "add", "myhost", "--keepalive-count", "100000"},
		{"link", "add", "myhost", "--no-such-flag"},
		{"link", "add", "myhost", "--no-restart"},
	} {
		if stderr := e.expectCode(exitUsage, args...); !strings.Contains(stderr, "--help") || !strings.Contains(stderr, "usage: gst link add") {
			t.Errorf("gst %v: usage errors must show usage and point to help: %q", args, stderr)
		}
	}
	if f := e.load(); len(f.Links) != 0 || len(e.back.calls) != 0 {
		t.Fatalf("rejected adds changed state: %+v %v", f.Links, e.back.calls)
	}
}

func TestLinkAddPreflightFailures(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"link", "add", "unknown"}, []string{"error: known_hosts: ", "hint:"}},
		{[]string{"link", "add", "nowhere"}, []string{"error: ssh -G nowhere: no such host\n", "hint: check the destination and your ssh configuration with `ssh -G nowhere`\n"}},
		{[]string{"link", "add", "myhost", "--identity", "/missing/key"}, []string{"error: identity: ", "/missing/key", "hint: gst needs a private key without a passphrase"}},
		{[]string{"link", "add", "myhost", "--known-hosts", "/missing/known_hosts"}, []string{"hint:"}},
	} {
		stderr := e.expectCode(exitFailure, tc.args...)
		for _, w := range tc.want {
			if !strings.Contains(stderr, w) {
				t.Errorf("gst %v: stderr %q lacks %q", tc.args, stderr, w)
			}
		}
	}
	e.exe = "/tmp/go-build123/b001/exe/gst"
	e.expectError(exitFailure, []string{"link", "add", "myhost"}, "temporary go build", "hint: install gst first")
	if f := e.load(); len(f.Links) != 0 || len(e.back.calls) != 0 {
		t.Fatalf("failed adds changed state: %+v %v", f.Links, e.back.calls)
	}
}

func TestLinkAddKeepsLinkWhenDaemonFails(t *testing.T) {
	e := newEnv(t)
	e.back.syncErr = errors.New("service manager unavailable")
	e.expectError(exitFailure, []string{"link", "add", "myhost"},
		"error: link myhost was added but did not start: service manager unavailable\n", "hint: retry with `gst link start myhost`\n")
	if e.link("myhost") == nil {
		t.Fatal("the link was not saved")
	}
}

// --- link set ---

func TestLinkSetChangesOnlyGivenFlags(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "myhost", "--identity", e.identity, "--remote-gst", "~/bin/gst", "--keepalive-interval", "30s")
	e.resetCalls()

	out := e.mustGst("link", "set", "myhost", "--keepalive-count", "4")
	contains(t, out, "Updated link myhost.\n", "Restarted link myhost.\n")
	want := config.Link{Destination: "myhost", IdentityFile: e.identity, RemoteGst: "~/bin/gst", KeepaliveIntervalSeconds: 30, KeepaliveCountMax: 4}
	if got := e.link("myhost"); *got != want {
		t.Fatalf("saved %+v, want %+v", *got, want)
	}
	if !slices.Equal(e.back.calls, []string{"sync", "restart myhost"}) {
		t.Fatalf("a running link must be restarted once: %v", e.back.calls)
	}

	// Empty and zero values go back to the defaults.
	e.mustGst("link", "set", "myhost", "--identity", "", "--remote-gst", "", "--keepalive-interval", "0s", "--keepalive-count", "0")
	if got := e.link("myhost"); *got != (config.Link{Destination: "myhost"}) {
		t.Fatalf("defaults not restored: %+v", *got)
	}

	e.mustGst("link", "set", "myhost", "--destination", "db")
	if got := e.link("myhost").Destination; got != "db" {
		t.Fatalf("destination %q", got)
	}
}

func TestLinkSetRestartModes(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "myhost")
	e.mustGst("link", "add", "db")

	e.resetCalls()
	out := e.mustGst("link", "set", "myhost", "--connect-timeout", "9s", "--no-restart")
	contains(t, out, "Updated link myhost.\n", "Restart the link to apply the change: gst link restart myhost\n")
	if len(e.back.calls) != 0 {
		t.Fatalf("--no-restart reached the daemon: %v", e.back.calls)
	}

	e.mustGst("link", "stop", "db")
	e.resetCalls()
	out = e.mustGst("link", "set", "db", "--connect-timeout", "9s")
	contains(t, out, "Updated link db.\n", "Link db is stopped; the change applies when you start it.\n")
	if len(e.back.calls) != 0 || e.link("db").ConnectTimeoutSeconds != 9 {
		t.Fatalf("a stopped link: calls %v, %+v", e.back.calls, e.link("db"))
	}

	e.back.restartErr = errors.New("daemon busy")
	e.expectError(exitFailure, []string{"link", "set", "myhost", "--connect-timeout", "10s"},
		"error: restart link myhost: daemon busy\n", "hint: retry with `gst link restart myhost`\n")
}

func TestLinkSetUnchanged(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "myhost", "--keepalive-interval", "30s")
	e.resetCalls()
	out := e.mustGst("link", "set", "myhost", "--keepalive-interval", "30s")
	if want := "Link myhost already has these settings; nothing changed.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if len(e.back.calls) != 0 {
		t.Fatalf("an unchanged link reached the daemon: %v", e.back.calls)
	}
	e.mustGst("link", "set", "myhost", "--destination", "myhost", "--remote-gst", "")
	if len(e.back.calls) != 0 {
		t.Fatalf("an unchanged link reached the daemon: %v", e.back.calls)
	}
}

func TestLinkSetErrors(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "myhost")
	e.expectError(exitUsage, []string{"link", "set", "myhost"}, "error: nothing to change; give at least one setting flag\n", "usage: gst link set <name>")
	e.expectError(exitUsage, []string{"link", "set"}, "error: missing argument <name>\n", "hint: links: myhost\n")
	e.expectError(exitUsage, []string{"link", "set", "myhost", "--connect-timeout", "1500ms"}, "whole number of seconds")
	e.expectError(exitUsage, []string{"link", "set", "myhost", "--destination", "-oProxyCommand=x"}, "usage: gst link set")
	e.expectError(exitFailure, []string{"link", "set", "nope", "--keepalive-count", "2"}, "error: no link named nope\n", "hint: links: myhost\n")

	before := *e.link("myhost")
	e.expectError(exitFailure, []string{"link", "set", "myhost", "--destination", "unknown"}, "error: known_hosts: ", "hint:")
	e.expectError(exitFailure, []string{"link", "set", "myhost", "--identity", "/missing/key"}, "error: identity: ")
	if got := *e.link("myhost"); got != before {
		t.Fatalf("a failed set changed the link: %+v", got)
	}
}

// --- link remove ---

func TestLinkRemove(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "myhost")
	e.mustGst("link", "add", "db")
	e.mustGst("link", "add", "home", "db")

	// Nothing is removed unless every name exists.
	e.expectError(exitFailure, []string{"link", "remove", "myhost", "nope"}, "error: no link named nope\n", "hint: links: ")
	if len(e.load().Links) != 3 {
		t.Fatal("a failed remove changed the configuration")
	}

	e.resetCalls()
	out := e.mustGst("link", "remove", "myhost", "db")
	if want := "Removed link myhost.\nRemoved link db.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if got := e.load().Links; len(got) != 1 || got["home"] == nil {
		t.Fatalf("links left: %+v", got)
	}
	if want := []string{"sync", "remove-logs myhost", "remove-logs db"}; !slices.Equal(e.back.calls, want) {
		t.Fatalf("calls %v, want %v", e.back.calls, want)
	}

	// rm is an alias, and the daemon stays up for the mesh.
	e.mustGst("link", "rm", "home")
	if len(e.load().Links) != 0 || !e.back.installed {
		t.Fatalf("links %+v, installed %v", e.load().Links, e.back.installed)
	}
	e.expectError(exitFailure, []string{"link", "remove", "home"}, "error: no link named home\n", "hint: there are no links yet; add one with `gst link add <name>`\n")
}

func TestLinkRemoveDaemonFailure(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "myhost")
	e.back.syncErr = errors.New("boom")
	e.expectError(exitFailure, []string{"link", "remove", "myhost"}, "the daemon may still run them: boom", "hint: retry with `gst up`")
	if len(e.load().Links) != 0 {
		t.Fatal("the link stays configured")
	}
}

// --- link start, stop, restart ---

func TestLinkStopStart(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "myhost")
	e.mustGst("link", "add", "db")

	if out := e.mustGst("link", "start", "myhost"); out != "Link myhost is already started.\n" {
		t.Fatalf("output %q", out)
	}
	if out := e.mustGst("link", "stop", "myhost"); out != "Stopped link myhost.\n" {
		t.Fatalf("output %q", out)
	}
	if !e.link("myhost").Stopped || e.link("db").Stopped {
		t.Fatalf("stopped: myhost %v, db %v", e.link("myhost").Stopped, e.link("db").Stopped)
	}
	if out := e.mustGst("link", "stop", "myhost"); out != "Link myhost is already stopped.\n" {
		t.Fatalf("output %q", out)
	}
	e.resetCalls()
	if out := e.mustGst("link", "start", "myhost"); out != "Started link myhost.\n" {
		t.Fatalf("output %q", out)
	}
	if e.link("myhost").Stopped || !slices.Equal(e.back.calls, []string{"sync"}) {
		t.Fatalf("start: stopped %v, calls %v", e.link("myhost").Stopped, e.back.calls)
	}

	// Several names, in order and without repeats.
	out := e.mustGst("link", "stop", "myhost", "db", "myhost")
	if want := "Stopped link db.\nStopped link myhost.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
}

func TestLinkAllSkipsSilently(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "myhost")
	e.mustGst("link", "add", "db")
	e.mustGst("link", "stop", "myhost")

	e.resetCalls()
	if out := e.mustGst("link", "restart", "--all"); out != "Restarted link db.\n" {
		t.Fatalf("restart --all output %q", out)
	}
	if want := []string{"sync", "restart db"}; !slices.Equal(e.back.calls, want) {
		t.Fatalf("restart --all must only restart started links: %v", e.back.calls)
	}
	if out := e.mustGst("link", "start", "--all"); out != "Started link myhost.\n" {
		t.Fatalf("start --all output %q", out)
	}
	if out := e.mustGst("link", "start", "--all"); out != "" {
		t.Fatalf("start --all with nothing to start printed %q", out)
	}
	if out := e.mustGst("link", "stop", "--all"); out != "Stopped link db.\nStopped link myhost.\n" {
		t.Fatalf("stop --all output %q", out)
	}
	if out := e.mustGst("link", "stop", "--all"); out != "" {
		t.Fatalf("stop --all with nothing to stop printed %q", out)
	}
	if out := e.mustGst("link", "restart", "--all"); out != "" {
		t.Fatalf("restart --all with nothing to restart printed %q", out)
	}
}

func TestLinkActionErrors(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "myhost")
	e.mustGst("link", "add", "db")
	e.mustGst("link", "stop", "myhost")

	e.expectError(exitUsage, []string{"link", "start", "myhost", "--all"}, "error: give link names or --all, not both\n")
	e.expectError(exitUsage, []string{"link", "start"}, "error: missing argument <name> or --all\n", "hint: links: db, myhost\n")
	e.expectError(exitFailure, []string{"link", "stop", "db", "nope"}, "error: no link named nope\n")
	if e.link("db").Stopped {
		t.Fatal("db was stopped although a name was unknown")
	}
	e.expectError(exitFailure, []string{"link", "restart", "myhost"},
		"error: link myhost is stopped\n", "hint: start it with `gst link start myhost`\n")

	// Starting and restarting check that the link can connect.
	delete(e.hosts, "myhost")
	e.expectError(exitFailure, []string{"link", "start", "myhost"}, "error: start myhost: ssh -G myhost: no such host\n", "hint: check the destination")
	if !e.link("myhost").Stopped {
		t.Fatal("a link that cannot connect was started")
	}
	delete(e.hosts, "db")
	e.resetCalls()
	e.expectError(exitFailure, []string{"link", "restart", "db"}, "error: restart db: ssh -G db: no such host\n")
	if len(e.back.calls) != 0 {
		t.Fatalf("a restart that failed the check reached the daemon: %v", e.back.calls)
	}
}

func TestLinkRestartReportsDaemonFailure(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "myhost")
	e.back.restartErr = errors.New("daemon busy")
	e.expectError(exitFailure, []string{"link", "restart", "myhost"}, "error: restart: daemon busy\n")
}

// --- link list and show ---

func TestLinkList(t *testing.T) {
	e := newEnv(t)
	if out := e.mustGst("link", "list"); out != "No links. Add one with: gst link add <name> [destination]\n" {
		t.Fatalf("output %q", out)
	}
	if out := e.mustGst("link", "list", "--json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty JSON %q", out)
	}

	e.mustGst("link", "add", "web", "myhost")
	e.mustGst("link", "add", "db")
	e.mustGst("link", "add", "cache", "devcn")
	e.mustGst("link", "add", "old", "db")
	e.mustGst("link", "stop", "db")
	e.back.linkStates["cache"] = wire.LinkState{Name: "cache", State: wire.StateDown, Error: "SSH keepalive timed out"}
	e.back.linkStates["old"] = wire.LinkState{Name: "old", State: wire.StateConnecting}

	rs := rows(e.mustGst("link", "list"))
	web := peerOf("web")
	want := [][]string{
		{"NAME", "STATUS", "DESTINATION", "PEER"},
		{"cache", "down", "(last", "error:", "SSH", "keepalive", "timed", "out)", "devcn", "-"},
		{"db", "stopped", "db", "-"},
		{"old", "connecting", "db", "-"},
		{"web", "running", "myhost", "web", "(" + web.ID[:8] + ")"},
	}
	if fmt.Sprint(rs) != fmt.Sprint(want) {
		t.Fatalf("list\n got %q\nwant %q", rs, want)
	}
	if out := e.mustGst("link", "ls"); !strings.HasPrefix(out, "NAME") {
		t.Fatalf("ls alias: %q", out)
	}

	views := decode[[]linkView](t, e.mustGst("link", "list", "--json"))
	if len(views) != 4 {
		t.Fatalf("views %+v", views)
	}
	cache, db, oldLink, webView := views[0], views[1], views[2], views[3]
	if cache.Name != "cache" || cache.Status != statusDown || cache.Error != "SSH keepalive timed out" || cache.Peer != nil || cache.Destination != "devcn" {
		t.Fatalf("cache %+v", cache)
	}
	if db.Status != statusStopped || oldLink.Status != statusConnecting {
		t.Fatalf("db %+v, old %+v", db, oldLink)
	}
	if webView.Status != statusRunning || webView.Peer == nil || *webView.Peer != web || webView.Resolved != nil {
		t.Fatalf("web %+v", webView)
	}
	raw := e.mustGst("link", "list", "--json")
	contains(t, raw, `"name": "web"`, `"status": "running"`, `"peer": {`, `"destination": "myhost"`)

	// Without a daemon, started links are down and stopped ones stay stopped.
	e.back.installed = false
	views = decode[[]linkView](t, e.mustGst("link", "list", "--json"))
	for _, v := range views {
		wantStatus := statusDown
		if v.Name == "db" {
			wantStatus = statusStopped
		}
		if v.Status != wantStatus || v.Peer != nil || v.Error != "" {
			t.Errorf("without a daemon: %+v, want %s", v, wantStatus)
		}
	}
}

func TestLinkShow(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "web", "myhost", "--remote-gst", "/opt/gst", "--keepalive-interval", "20s", "--keepalive-count", "2")

	out := e.mustGst("link", "show", "web")
	web := peerOf("web")
	for _, w := range []string{
		"Link:", "web\n",
		"Status:", "running\n",
		"Peer:", "web (" + web.ID[:8] + ")\n",
		"Destination:", "myhost\n",
		"Server:", "tunnel@ssh.example.com:22\n",
		"Identities:", e.identity,
		"Known hosts:", e.known,
		"Connect timeout:", "15s\n",
		"Keepalive:", "every 20s, give up after 40s without a reply\n",
		"Remote gst:", "/opt/gst\n",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("show lacks %q:\n%s", w, out)
		}
	}

	v := decode[linkView](t, e.mustGst("link", "show", "web", "--json"))
	if v.Resolved == nil || v.Resolved.Server != "ssh.example.com:22" || v.Resolved.User != "tunnel" ||
		v.Resolved.KeepaliveIntervalSeconds != 20 || v.Resolved.KeepaliveTimeoutSeconds != 40 || v.Resolved.ConnectTimeoutSeconds != 15 ||
		v.RemoteGst != "/opt/gst" || v.ResolveError != "" {
		t.Fatalf("view %+v resolved %+v", v, v.Resolved)
	}

	// A link follows the ssh config, and shows when that fails.
	delete(e.hosts, "myhost")
	out = e.mustGst("link", "show", "web")
	contains(t, out, "unresolved: ssh -G myhost: no such host")
	v = decode[linkView](t, e.mustGst("link", "show", "web", "--json"))
	if v.Resolved != nil || v.ResolveError != "ssh -G myhost: no such host" {
		t.Fatalf("view %+v", v)
	}

	e.back.linkStates["web"] = wire.LinkState{Name: "web", State: wire.StateDown, Error: "SSH connection closed"}
	contains(t, e.mustGst("link", "show", "web"), "down (last error: SSH connection closed)")

	e.expectCode(exitFailure, "link", "show", "nope")
}

func TestLinkShowDefaultsRemoteGst(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "web", "myhost")
	if out := rows(e.mustGst("link", "show", "web")); !slices.ContainsFunc(out, func(r []string) bool { return fmt.Sprint(r) == "[Remote gst: gst]" }) {
		t.Fatalf("show does not default the remote gst to gst: %q", out)
	}
}

// --- link logs ---

func TestLinkLogs(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "web", "myhost")
	e.back.logOutput = "2026-01-01 connected\n"
	e.resetCalls()
	if out := e.mustGst("link", "logs", "web"); out != e.back.logOutput {
		t.Fatalf("output %q", out)
	}
	e.mustGst("link", "logs", "web", "-n", "5", "-f")
	if want := []string{"logs web 50 false", "logs web 5 true"}; !slices.Equal(e.back.calls, want) {
		t.Fatalf("calls %v, want %v", e.back.calls, want)
	}
	e.expectError(exitUsage, []string{"link", "logs", "web", "-n", "0"}, "error: --lines must be at least 1\n")
	e.expectError(exitFailure, []string{"link", "logs", "nope"}, "error: no link named nope\n", "hint: links: web\n")
	e.expectError(exitUsage, []string{"link", "logs"}, "error: missing argument <name>\n", "hint: links: web\n")
}

// --- link check ---

func TestLinkCheckFailures(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "web", "myhost")

	t.Run("ssh config", func(t *testing.T) {
		delete(e.hosts, "myhost")
		code, stdout, stderr := e.gst("link", "check", "web")
		if code != exitFailure || stderr != "" {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
		contains(t, stdout, "FAIL  ssh config  ssh -G myhost: no such host", "hint: check the destination and your ssh configuration with `ssh -G myhost`")
		if strings.Contains(stdout, "ok") {
			t.Fatalf("later steps ran: %s", stdout)
		}

		code, stdout, _ = e.gst("link", "check", "web", "--json")
		if code != exitFailure {
			t.Fatalf("code %d", code)
		}
		res := decode[struct {
			OK    bool `json:"ok"`
			Steps []struct {
				Name   string `json:"name"`
				OK     bool   `json:"ok"`
				Detail string `json:"detail"`
				Hint   string `json:"hint"`
			} `json:"steps"`
		}](t, stdout)
		if res.OK || len(res.Steps) != 1 || res.Steps[0].Name != "ssh config" || res.Steps[0].OK || res.Steps[0].Hint == "" {
			t.Fatalf("result %+v", res)
		}
	})

	t.Run("identity", func(t *testing.T) {
		host := e.hosts["db"]
		host.IdentityFiles = []string{"/missing/id"}
		e.hosts["myhost"] = host
		code, stdout, _ := e.gst("link", "check", "web")
		if code != exitFailure {
			t.Fatalf("code %d", code)
		}
		contains(t, stdout, "ok    ssh config  myhost resolves to", "FAIL  identity", "hint: gst needs a private key without a passphrase")
		if strings.Contains(stdout, "known_hosts") || strings.Contains(stdout, "reached") {
			t.Fatalf("checks went on after a failure: %s", stdout)
		}
	})

	t.Run("known hosts", func(t *testing.T) {
		e.hosts["myhost"] = e.hosts["unknown"]
		code, stdout, _ := e.gst("link", "check", "web")
		if code != exitFailure {
			t.Fatalf("code %d", code)
		}
		contains(t, stdout, "ok    ssh config", "ok    identity", "FAIL  known_hosts", "hint:")
		if strings.Contains(stdout, "reached") {
			t.Fatalf("connected despite an unknown host: %s", stdout)
		}
	})

	t.Run("unknown link", func(t *testing.T) {
		e.expectError(exitFailure, []string{"link", "check", "nope"}, "error: no link named nope\n")
		e.expectError(exitUsage, []string{"link", "check"}, "error: missing argument <name>\n")
	})
}

func TestConnectMissingTargetNamesArgument(t *testing.T) {
	e := newEnv(t)
	e.expectError(exitUsage, []string{"connect", "devcn"}, "error: missing argument <port | host:port | socket>\n")
}

func TestCompletesLinkNames(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "add", "myhost")
	e.mustGst("link", "add", "db")
	out := e.mustGst("__complete", "link", "show", "")
	if got := strings.Fields(out); !slices.Equal(got[:2], []string{"db", "myhost"}) {
		t.Fatalf("completions %q", out)
	}
}
