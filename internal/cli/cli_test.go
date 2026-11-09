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
// it, it reports what the configuration file asks it to run: every link as
// running.
type fakeBackend struct {
	config    string
	installed bool
	command   []string // the last daemon command
	calls     []string

	exposes  []wire.ExposeState
	syncErr  error
	nodesErr error

	linkStates map[string]wire.LinkState // replaces the state of a running link, by destination
	nodeStatus func(node string) (wire.Status, error)
	ping       func(node string) (wire.NetPing, error)
	pingSize   int
	links      func(node string) (wire.Ping, error)
	direct     func(node string) (wire.DirectReport, error)
	topology   wire.Topology
	net        *wire.NetState // what the daemon reports of the network
}

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

// peerOf is the machine called name, which a running link to the
// destination name reaches.
func peerOf(name string) wire.NodeInfo {
	return wire.NodeInfo{
		ID:       fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(name))) + strings.Repeat("0", 24),
		Name:     name,
		Software: "gst 0.4.0",
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
	st := wire.Status{Software: software, Links: []wire.LinkState{}, Net: f.net}
	if cfg.Node != nil {
		st.Node = wire.NodeInfo{ID: cfg.Node.ID, Name: cfg.Node.Name}
	}
	for _, l := range cfg.Links {
		peer := peerOf(l.Destination)
		state := wire.LinkState{Destination: l.Destination, State: wire.StateRunning, Peer: &peer}
		if s, ok := f.linkStates[l.Destination]; ok {
			state = s
		}
		st.Links = append(st.Links, state)
	}
	for _, e := range cfg.Exposes {
		state := wire.ExposeState{Protocol: e.Protocol, Port: e.Port, Target: e.Target, State: "waiting"}
		if f.net != nil && f.net.Interface != "" {
			state.State = wire.StateRunning
			state.Address = fmt.Sprintf("%s:%d", f.net.Address, e.Port)
		}
		st.Exposes = append(st.Exposes, state)
	}
	if f.exposes != nil {
		st.Exposes = f.exposes
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

func (f *fakeBackend) NetPing(_ context.Context, node string, size int) (wire.NetPing, error) {
	f.pingSize = size
	f.calls = append(f.calls, "ping "+node)
	if f.ping != nil {
		return f.ping(node)
	}
	return wire.NetPing{Node: peerOf(node), Address: "10.99.3.4", Path: wire.PathDirect, Endpoint: "192.0.2.2:6000", Via: []wire.NodeInfo{}, TimeMS: 1.5}, nil
}

func (f *fakeBackend) Ping(_ context.Context, node string) (wire.Ping, error) {
	f.calls = append(f.calls, "links "+node)
	if f.links != nil {
		return f.links(node)
	}
	return wire.Ping{Node: peerOf(node), Via: []wire.NodeInfo{}, TimeMS: 74.1}, nil
}

func (f *fakeBackend) Direct(_ context.Context, node string) (wire.DirectReport, error) {
	f.calls = append(f.calls, "direct "+node)
	if f.direct != nil {
		return f.direct(node)
	}
	return wire.DirectReport{Node: peerOf(node)}, nil
}

func (f *fakeBackend) Nodes(context.Context) (wire.Topology, error) {
	f.calls = append(f.calls, "nodes")
	if !f.installed {
		return wire.Topology{}, errors.New("no daemon")
	}
	return f.topology, f.nodesErr
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

	helper    fakeHelper
	terminal  bool     // whether a person can answer
	euid      int      // the user gst runs as
	rootRuns  []string // what gst ran on the terminal
	rootErr   error    // what those commands return
	removeErr error    // what asking the helper to remove itself returns
}

// newEnv returns an environment where the destinations myhost, db, and
// devbox resolve to a server with a usable identity and a known host key,
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
		euid:     1000,
		helper:   fakeHelper{installed: true, current: true},
	}
	e.back = &fakeBackend{config: e.config, linkStates: map[string]wire.LinkState{}}
	write(t, e.identity, string(pem.EncodeToMemory(block)))
	write(t, e.known, knownhosts.Line([]string{"ssh.example.com:22"}, signer.PublicKey())+"\n")
	host := sshconfig.Host{
		Hostname: "ssh.example.com", Port: "22", User: "tunnel",
		IdentityFiles: []string{e.identity}, KnownHostsFiles: []string{e.known}, ServerAliveCountMax: 3,
	}
	unknown := host
	unknown.Hostname = "unknown.example.com"
	e.hosts = map[string]sshconfig.Host{"myhost": host, "db": host, "devbox": host, "unknown": unknown}
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
		newHelper:  func() (helperService, error) { return &e.helper, nil },
		removeHelper: func() error {
			if e.removeErr == nil {
				e.helper = fakeHelper{}
			}
			return e.removeErr
		},
		runRoot: func(argv []string) error {
			e.rootRuns = append(e.rootRuns, strings.Join(argv, " "))
			return e.rootErr
		},
		geteuid:     func() int { return e.euid },
		interactive: func() bool { return e.terminal },
	}
}

// fakeHelper is the helper as gst up sees it.
type fakeHelper struct{ installed, current bool }

func (h *fakeHelper) Installed() bool          { return h.installed }
func (h *fakeHelper) Current(int, string) bool { return h.current }
func (h *fakeHelper) Installs() []string       { return []string{"copies gst"} }
func (h *fakeHelper) Removes() []string        { return []string{"deletes gst"} }

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

func (e *testEnv) link(destination string) *config.Link {
	e.t.Helper()
	return e.load().Link(destination)
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
	e.mustGst("link", "myhost")
	e.mustGst("link", "db")

	t.Run("unknown link is a failure with a hint", func(t *testing.T) {
		code, stdout, stderr := e.gst("unlink", "nope")
		if code != exitFailure || stdout != "" {
			t.Fatalf("code %d, stdout %q", code, stdout)
		}
		if want := "error: this machine has no link to nope\nhint: this machine links to myhost, db\n"; stderr != want {
			t.Fatalf("stderr %q, want %q", stderr, want)
		}
	})

	t.Run("usage errors print usage and exit 2", func(t *testing.T) {
		for args, want := range map[string][]string{
			"unlink":      {"error: missing argument <machine>\n", "usage: gst unlink <machine>...", "hint: this machine links to myhost, db\n", "Run 'gst unlink --help'"},
			"lnk":         {`unknown command "lnk" for "gst"`, `did you mean "link"`},
			"add x":       {`unknown command "add"`, "usage: gst <command>\n"},
			"nodes":       {`unknown command "nodes"`, "usage: gst <command>\n"},
			"frobnicate":  {`unknown command "frobnicate"`, "Run 'gst --help'"},
			"status a b":  {`error: unexpected argument "b"`, "usage: gst status [machine]"},
			"ping":        {"error: missing argument <machine>\n", "usage: gst ping <machine>"},
			"subnet a b":  {`error: unexpected argument "b"`, "usage: gst subnet [cidr]"},
			"link --nope": {"error: unknown flag: --nope\n", "usage: gst link"},
			"link":        {"error: missing argument <destination>\n", "usage: gst link <destination>"},
		} {
			stderr := e.expectCode(exitUsage, strings.Fields(args)...)
			for _, w := range want {
				if !strings.Contains(stderr, w) {
					t.Errorf("gst %s: stderr %q lacks %q", args, stderr, w)
				}
			}
			if strings.HasPrefix(args, "link") && strings.Contains(stderr, "hint:") {
				t.Errorf("gst %s: a new link needs no list of links: %q", args, stderr)
			}
		}
	})

}

func TestEveryCommandNamesItsMissingArguments(t *testing.T) {
	e := newEnv(t)
	for args, want := range map[string]string{
		"link":     "<destination>",
		"unlink":   "<machine>",
		"ping":     "<machine>",
		"unexpose": "<port>",
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
	if code := a.execute(context.Background(), []string{"link", "myhost"}); code != exitOK {
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
	stderr := e.expectCode(exitFailure, "name")
	if !strings.HasPrefix(stderr, "error: config ") || !strings.Contains(stderr, "invalid JSON") {
		t.Fatalf("stderr %q", stderr)
	}
	write(t, e.config, `{"version":5,"surprise":true}`)
	e.expectCode(exitFailure, "status")
}

// --- gst link ---

func TestLink(t *testing.T) {
	e := newEnv(t)
	out := e.mustGst("link", "myhost", "--remote-gst", "~/bin/gst")
	if want := "Linked to myhost.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	want := &config.Link{Destination: "myhost", RemoteGst: "~/bin/gst"}
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

	// The machine at the other end has a name of its own.
	e.hosts["ssh://deploy@ssh.example.com:2200"] = e.hosts["myhost"]
	e.back.linkStates["ssh://deploy@ssh.example.com:2200"] = wire.LinkState{Destination: "ssh://deploy@ssh.example.com:2200", State: wire.StateRunning, Peer: &wire.NodeInfo{ID: peerOf("web").ID, Name: "web"}}
	out = e.mustGst("link", "ssh://deploy@ssh.example.com:2200")
	if want := "Linked to ssh://deploy@ssh.example.com:2200, which is web.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}

	// A link that has not run yet connects in the background.
	e.back.linkStates["db"] = wire.LinkState{Destination: "db", State: wire.StateDown, Error: "connection refused"}
	out = e.mustGst("link", "db")
	if want := "Linked to db. It connects in the background; see it with `gst status`.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
}

func TestLinkDuplicate(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "myhost")
	e.resetCalls()
	e.expectError(exitFailure, []string{"link", "myhost", "--remote-gst", "/opt/gst"},
		"error: this machine already links to myhost\n", "hint: to change the link, run `gst unlink myhost` and link again\n")
	if len(e.back.calls) != 0 || *e.link("myhost") != (config.Link{Destination: "myhost"}) {
		t.Fatalf("a duplicate changed state: %v %+v", e.back.calls, e.link("myhost"))
	}

	// To change a link, unlink it and link again.
	e.mustGst("unlink", "myhost")
	e.mustGst("link", "myhost", "--remote-gst", "/opt/gst")
	if got := e.link("myhost"); *got != (config.Link{Destination: "myhost", RemoteGst: "/opt/gst"}) {
		t.Fatalf("saved %+v", got)
	}
	if want := []string{"sync", "sync"}; !slices.Equal(e.back.calls, want) {
		t.Fatalf("calls %v, want %v", e.back.calls, want)
	}
}

func TestLinkRejects(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{
		{"link"},
		{"link", "--", "-oProxyCommand=x"},
		{"link", "my host"},
		{"link", "myhost", "db"},
		{"link", "myhost", "--no-such-flag"},
		// Settings that the ssh configuration owns are not flags.
		{"link", "myhost", "--identity", "/k"},
		{"link", "myhost", "--known-hosts", "/k"},
		{"link", "myhost", "--connect-timeout", "5s"},
		{"link", "myhost", "--keepalive-interval", "5s"},
		{"link", "myhost", "--keepalive-count", "2"},
		{"link", "myhost", "--no-restart"},
	} {
		if stderr := e.expectCode(exitUsage, args...); !strings.Contains(stderr, "--help") || !strings.Contains(stderr, "usage: gst link") {
			t.Errorf("gst %v: usage errors must show usage and point to help: %q", args, stderr)
		}
	}
	if f := e.load(); len(f.Links) != 0 || len(e.back.calls) != 0 {
		t.Fatalf("rejected links changed state: %+v %v", f.Links, e.back.calls)
	}
}

func TestLinkPreflightFailures(t *testing.T) {
	e := newEnv(t)
	noKey := e.hosts["myhost"]
	noKey.IdentityFiles = []string{"/missing/key"}
	e.hosts["nokey"] = noKey
	noKnown := e.hosts["myhost"]
	noKnown.KnownHostsFiles = []string{"/missing/known_hosts"}
	e.hosts["noknown"] = noKnown
	locked := e.hosts["myhost"]
	locked.IdentityFiles = []string{filepath.Join(t.TempDir(), "locked")}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(private, "", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, locked.IdentityFiles[0], string(pem.EncodeToMemory(block)))
	e.hosts["locked"] = locked

	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"link", "unknown"}, []string{"error: no host key for unknown.example.com:22", "hint: verify the server's host key and record it by connecting once with `ssh "}},
		{[]string{"link", "noknown"}, []string{"error: none of the known_hosts files exist: /missing/known_hosts\n", "hint: verify the server's host key"}},
		{[]string{"link", "nowhere"}, []string{"error: ssh -G nowhere: no such host\n", "hint: check the destination and your ssh configuration with `ssh -G nowhere`\n"}},
		{[]string{"link", "nokey"}, []string{"error: no usable identity among /missing/key (missing)\n", "hint: gst needs a private key without a passphrase", "ssh-keygen", "ssh-copy-id"}},
		{[]string{"link", "locked"}, []string{"passphrase-protected", "hint: gst needs a private key without a passphrase"}},
	} {
		stderr := e.expectCode(exitFailure, tc.args...)
		for _, w := range tc.want {
			if !strings.Contains(stderr, w) {
				t.Errorf("gst %v: stderr %q lacks %q", tc.args, stderr, w)
			}
		}
	}
	e.exe = "/tmp/go-build123/b001/exe/gst"
	e.expectError(exitFailure, []string{"link", "myhost"}, "temporary go build", "hint: install gst first")
	if f := e.load(); len(f.Links) != 0 || len(e.back.calls) != 0 {
		t.Fatalf("failed links changed state: %+v %v", f.Links, e.back.calls)
	}
}

func TestLinkKeptWhenDaemonFails(t *testing.T) {
	e := newEnv(t)
	e.back.syncErr = errors.New("service manager unavailable")
	e.expectError(exitFailure, []string{"link", "myhost"},
		"error: linked to myhost, but gst did not start: service manager unavailable\n", "hint: retry with `gst up`\n")
	if e.link("myhost") == nil {
		t.Fatal("the link was not saved")
	}
}

// --- gst unlink ---

func TestUnlink(t *testing.T) {
	e := newEnv(t)
	e.hosts["admin@db"] = e.hosts["db"]
	e.mustGst("link", "myhost")
	e.mustGst("link", "db")
	e.mustGst("link", "admin@db")
	e.back.linkStates["admin@db"] = wire.LinkState{Destination: "admin@db", State: wire.StateRunning, Peer: &wire.NodeInfo{ID: peerOf("db").ID, Name: "db"}}

	// Nothing is removed unless every machine has a link.
	e.expectError(exitFailure, []string{"unlink", "myhost", "nope"}, "error: this machine has no link to nope\n", "hint: this machine links to ")
	if len(e.load().Links) != 3 {
		t.Fatal("a failed unlink changed the configuration")
	}

	// A machine's name removes every link to it.
	e.resetCalls()
	out := e.mustGst("unlink", "myhost", "db.gst")
	if want := "Unlinked myhost.\nUnlinked db.\nUnlinked admin@db.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if got := e.load().Links; len(got) != 0 {
		t.Fatalf("links left: %+v", got)
	}
	if want := []string{"sync"}; !slices.Equal(e.back.calls, want) {
		t.Fatalf("calls %v, want %v", e.back.calls, want)
	}
	if !e.back.installed {
		t.Fatal("gst stopped: other machines may still link to this one")
	}
	e.expectError(exitFailure, []string{"unlink", "db"}, "error: this machine has no link to db\n", "hint: this machine has no links yet; add one with `gst link <destination>`\n")

	// A link that has not run yet has only its destination.
	e.mustGst("link", "devbox")
	e.back.linkStates["devbox"] = wire.LinkState{Destination: "devbox", State: wire.StateConnecting}
	e.mustGst("unlink", "devbox")
	if len(e.load().Links) != 0 {
		t.Fatal("the link stays configured")
	}
}

func TestUnlinkDaemonFailure(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "myhost")
	e.back.syncErr = errors.New("boom")
	e.expectError(exitFailure, []string{"unlink", "myhost"}, "may still run: boom", "hint: retry with `gst up`")
	if len(e.load().Links) != 0 {
		t.Fatal("the link stays configured")
	}
}

func TestCompletesLinks(t *testing.T) {
	e := newEnv(t)
	e.mustGst("link", "myhost")
	e.mustGst("link", "db")
	out := e.mustGst("__complete", "unlink", "")
	if got := strings.Fields(out); !slices.Equal(got[:2], []string{"myhost", "db"}) {
		t.Fatalf("completions %q", out)
	}
}

func TestOldConfigIsRefused(t *testing.T) {
	e := newEnv(t)
	old := `{"version": 3, "links": {"myhost": {"destination": "myhost"}}}`
	write(t, e.config, old)
	code, _, stderr := e.gst("up")
	if code != exitFailure || !strings.Contains(stderr, "config version 3 is too old") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if data, _ := os.ReadFile(e.config); string(data) != old {
		t.Fatalf("the file changed: %s", data)
	}
}
