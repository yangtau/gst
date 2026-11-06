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
// it, it reports what the configuration file asks it to run: every link as
// running.
type fakeBackend struct {
	config    string
	installed bool
	command   []string // the last daemon command
	calls     []string

	syncErr  error
	nodesErr error

	linkStates map[string]wire.LinkState // replaces the state of a running link
	nodeStatus func(node string) (wire.Status, error)
	ping       func(node string) (wire.NetPing, error)
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

// peerOf is the node that a running link to name reaches.
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
	for _, name := range slices.Sorted(maps.Keys(cfg.Links)) {
		peer := peerOf(name)
		state := wire.LinkState{Name: name, State: wire.StateRunning, Peer: &peer}
		if s, ok := f.linkStates[name]; ok {
			state = s
		}
		st.Links = append(st.Links, state)
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

func (f *fakeBackend) NetPing(_ context.Context, node string) (wire.NetPing, error) {
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
	e.mustGst("add", "myhost")
	e.mustGst("add", "db")

	t.Run("unknown link is a failure with a hint", func(t *testing.T) {
		code, stdout, stderr := e.gst("remove", "nope")
		if code != exitFailure || stdout != "" {
			t.Fatalf("code %d, stdout %q", code, stdout)
		}
		if want := "error: no link named nope\nhint: links: db, myhost\n"; stderr != want {
			t.Fatalf("stderr %q, want %q", stderr, want)
		}
	})

	t.Run("usage errors print usage and exit 2", func(t *testing.T) {
		for args, want := range map[string][]string{
			"remove":     {"error: missing argument <name>\n", "usage: gst remove <name>...", "hint: links: db, myhost\n", "Run 'gst remove --help'"},
			"ad":         {`unknown command "ad" for "gst"`, `did you mean "add"`},
			"link add x": {`unknown command "link" for "gst"`, `did you mean "add"`},
			"nodes":      {`unknown command "nodes"`, "usage: gst <command>\n"},
			"lnk":        {`unknown command "lnk"`, "usage: gst <command>\n"},
			"frobnicate": {`unknown command "frobnicate"`, "Run 'gst --help'"},
			"status a b": {`error: unexpected argument "b"`, "usage: gst status [machine]"},
			"ping":       {"error: missing argument <machine>\n", "usage: gst ping <machine>"},
			"subnet a b": {`error: unexpected argument "b"`, "usage: gst subnet [cidr]"},
			"add --nope": {"error: unknown flag: --nope\n", "usage: gst add"},
			"add":        {"error: missing argument <name>\n", "usage: gst add <name> [destination]"},
		} {
			stderr := e.expectCode(exitUsage, strings.Fields(args)...)
			for _, w := range want {
				if !strings.Contains(stderr, w) {
					t.Errorf("gst %s: stderr %q lacks %q", args, stderr, w)
				}
			}
			if strings.HasPrefix(args, "add") && strings.Contains(stderr, "hint:") {
				t.Errorf("gst %s: a new link name needs no list of links: %q", args, stderr)
			}
		}
	})

}

func TestEveryCommandNamesItsMissingArguments(t *testing.T) {
	e := newEnv(t)
	for args, want := range map[string]string{
		"add":    "<name>",
		"remove": "<name>",
		"rm":     "<name>",
		"ping":   "<machine>",
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
	if code := a.execute(context.Background(), []string{"add", "myhost"}); code != exitOK {
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
	write(t, e.config, `{"version":4,"surprise":true}`)
	e.expectCode(exitFailure, "status")
}

// --- gst add ---

func TestLinkAdd(t *testing.T) {
	e := newEnv(t)
	out := e.mustGst("add", "myhost", "--remote-gst", "~/bin/gst")
	if want := "Added link myhost; it connects in the background. See it with: gst status\n"; out != want {
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

	e.mustGst("add", "home", "db")
	if got := e.link("home"); got.Destination != "db" || got.RemoteGst != "" {
		t.Fatalf("explicit destination not saved: %+v", got)
	}
	e.hosts["ssh://deploy@ssh.example.com:2200"] = e.hosts["myhost"]
	e.mustGst("add", "web", "ssh://deploy@ssh.example.com:2200")
	if got := e.link("web").Destination; got != "ssh://deploy@ssh.example.com:2200" {
		t.Fatalf("destination %q", got)
	}
}

func TestLinkAddDuplicate(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "myhost")
	e.resetCalls()
	e.expectError(exitFailure, []string{"add", "myhost", "db", "--remote-gst", "/opt/gst"},
		"error: link myhost already exists\n", "hint: to change it, remove it with `gst remove myhost` and add it again\n")
	if len(e.back.calls) != 0 || *e.link("myhost") != (config.Link{Destination: "myhost"}) {
		t.Fatalf("a duplicate changed state: %v %+v", e.back.calls, e.link("myhost"))
	}

	// To change a link, remove it and add it again.
	e.mustGst("remove", "myhost")
	e.mustGst("add", "myhost", "db", "--remote-gst", "/opt/gst")
	if got := e.link("myhost"); *got != (config.Link{Destination: "db", RemoteGst: "/opt/gst"}) {
		t.Fatalf("saved %+v", got)
	}
	if want := []string{"sync", "sync"}; !slices.Equal(e.back.calls, want) {
		t.Fatalf("calls %v, want %v", e.back.calls, want)
	}
}

func TestLinkAddRejects(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{
		{"add"},
		{"add", "MyHost"},
		{"add", "-bad"},
		{"add", "my_host"},
		{"add", strings.Repeat("a", 33)},
		{"add", "myhost", "--", "-oProxyCommand=x"},
		{"add", "myhost", "a", "b"},
		{"add", "myhost", "--no-such-flag"},
		// Settings that the ssh configuration owns are not flags.
		{"add", "myhost", "--identity", "/k"},
		{"add", "myhost", "--known-hosts", "/k"},
		{"add", "myhost", "--connect-timeout", "5s"},
		{"add", "myhost", "--keepalive-interval", "5s"},
		{"add", "myhost", "--keepalive-count", "2"},
		{"add", "myhost", "--no-restart"},
	} {
		if stderr := e.expectCode(exitUsage, args...); !strings.Contains(stderr, "--help") || !strings.Contains(stderr, "usage: gst add") {
			t.Errorf("gst %v: usage errors must show usage and point to help: %q", args, stderr)
		}
	}
	if f := e.load(); len(f.Links) != 0 || len(e.back.calls) != 0 {
		t.Fatalf("rejected adds changed state: %+v %v", f.Links, e.back.calls)
	}
}

func TestLinkAddPreflightFailures(t *testing.T) {
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
		{[]string{"add", "unknown"}, []string{"error: no host key for unknown.example.com:22", "hint: verify the server's host key and record it by connecting once with `ssh "}},
		{[]string{"add", "noknown"}, []string{"error: none of the known_hosts files exist: /missing/known_hosts\n", "hint: verify the server's host key"}},
		{[]string{"add", "nowhere"}, []string{"error: ssh -G nowhere: no such host\n", "hint: check the destination and your ssh configuration with `ssh -G nowhere`\n"}},
		{[]string{"add", "nokey"}, []string{"error: no usable identity among /missing/key (missing)\n", "hint: gst needs a private key without a passphrase", "ssh-keygen", "ssh-copy-id"}},
		{[]string{"add", "locked"}, []string{"passphrase-protected", "hint: gst needs a private key without a passphrase"}},
		{[]string{"add", "web", "nokey"}, []string{"/missing/key", "hint:"}},
	} {
		stderr := e.expectCode(exitFailure, tc.args...)
		for _, w := range tc.want {
			if !strings.Contains(stderr, w) {
				t.Errorf("gst %v: stderr %q lacks %q", tc.args, stderr, w)
			}
		}
	}
	e.exe = "/tmp/go-build123/b001/exe/gst"
	e.expectError(exitFailure, []string{"add", "myhost"}, "temporary go build", "hint: install gst first")
	if f := e.load(); len(f.Links) != 0 || len(e.back.calls) != 0 {
		t.Fatalf("failed adds changed state: %+v %v", f.Links, e.back.calls)
	}
}

func TestLinkAddKeepsLinkWhenDaemonFails(t *testing.T) {
	e := newEnv(t)
	e.back.syncErr = errors.New("service manager unavailable")
	e.expectError(exitFailure, []string{"add", "myhost"},
		"error: link myhost was added but did not start: service manager unavailable\n", "hint: retry with `gst up`\n")
	if e.link("myhost") == nil {
		t.Fatal("the link was not saved")
	}
}

// --- gst remove ---

func TestLinkRemove(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "myhost")
	e.mustGst("add", "db")
	e.mustGst("add", "home", "db")

	// Nothing is removed unless every name exists.
	e.expectError(exitFailure, []string{"remove", "myhost", "nope"}, "error: no link named nope\n", "hint: links: ")
	if len(e.load().Links) != 3 {
		t.Fatal("a failed remove changed the configuration")
	}

	e.resetCalls()
	out := e.mustGst("remove", "myhost", "db")
	if want := "Removed link myhost.\nRemoved link db.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if got := e.load().Links; len(got) != 1 || got["home"] == nil {
		t.Fatalf("links left: %+v", got)
	}
	if want := []string{"sync"}; !slices.Equal(e.back.calls, want) {
		t.Fatalf("calls %v, want %v", e.back.calls, want)
	}

	// rm is an alias, and the daemon stays up for the mesh.
	e.mustGst("rm", "home")
	if len(e.load().Links) != 0 || !e.back.installed {
		t.Fatalf("links %+v, installed %v", e.load().Links, e.back.installed)
	}
	e.expectError(exitFailure, []string{"remove", "home"}, "error: no link named home\n", "hint: there are no links yet; add one with `gst add <name>`\n")
}

func TestLinkRemoveDaemonFailure(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "myhost")
	e.back.syncErr = errors.New("boom")
	e.expectError(exitFailure, []string{"remove", "myhost"}, "the daemon may still run them: boom", "hint: retry with `gst up`")
	if len(e.load().Links) != 0 {
		t.Fatal("the link stays configured")
	}
}

func TestCompletesLinkNames(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "myhost")
	e.mustGst("add", "db")
	out := e.mustGst("__complete", "remove", "")
	if got := strings.Fields(out); !slices.Equal(got[:2], []string{"db", "myhost"}) {
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
