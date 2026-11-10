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
	"reflect"
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
	removed    *wire.Removed  // what the daemon reports of a removal
	remove     func(node string) (wire.Removal, error)
	subnet     string // the last subnet set; the network reports it at once
	// staleNet is what the network reports for the first staleFor
	// questions after a subnet is set, before it applies the subnet.
	staleNet            *wire.NetState
	staleFor, staleLeft int
}

// Call answers the control methods as the daemon would, through JSON as
// the daemon's answers come.
func (f *fakeBackend) Call(_ context.Context, method string, params, result any) error {
	var p struct {
		Node   string `json:"node"`
		Size   int    `json:"size"`
		Subnet string `json:"subnet"`
	}
	if params != nil {
		data, _ := json.Marshal(params)
		json.Unmarshal(data, &p)
	}
	var r any
	var err error
	switch method {
	case wire.MethodStatus:
		st, ok := f.status()
		if !ok {
			return errors.New("no daemon")
		}
		r = st
	case wire.MethodNodes:
		r, err = f.nodes()
	case wire.MethodNodeStatus:
		r, err = f.nodeStatusOf(p.Node)
	case wire.MethodNetPing:
		r, err = f.netPing(p.Node, p.Size)
	case wire.MethodPing:
		r, err = f.linksPing(p.Node)
	case wire.MethodDirect:
		r, err = f.directOf(p.Node)
	case wire.MethodRemove:
		r, err = f.removeNode(p.Node)
	case wire.MethodSubnet:
		err = f.setSubnet(p.Subnet)
	default:
		return wire.Errorf(wire.CodeUnsupportedMethod, "no method %s", method)
	}
	if err != nil || result == nil {
		return err
	}
	data, _ := json.Marshal(r)
	return json.Unmarshal(data, result)
}

func (f *fakeBackend) removeNode(node string) (wire.Removal, error) {
	f.calls = append(f.calls, "remove "+node)
	if f.remove != nil {
		return f.remove(node)
	}
	return wire.Removal{Node: peerOf(node), Stranded: []wire.NodeInfo{}}, nil
}

func (f *fakeBackend) setSubnet(subnet string) error {
	f.calls = append(f.calls, "subnet "+subnet)
	f.subnet = subnet
	f.staleLeft = f.staleFor
	return nil
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

func (f *fakeBackend) status() (wire.Status, bool) {
	if !f.installed {
		return wire.Status{}, false
	}
	cfg, err := config.Load(f.config)
	if err != nil {
		return wire.Status{}, false
	}
	st := wire.Status{Software: software, Links: []wire.LinkState{}, Net: f.net, Removed: f.removed}
	if f.net != nil && f.subnet != "" {
		net := *f.net
		net.Subnet = f.subnet
		st.Net = &net
	}
	if f.staleLeft > 0 {
		f.staleLeft--
		st.Net = f.staleNet
	}
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

func (f *fakeBackend) nodeStatusOf(node string) (wire.Status, error) {
	f.calls = append(f.calls, "node-status "+node)
	if f.nodeStatus != nil {
		return f.nodeStatus(node)
	}
	st, ok := f.status()
	if !ok {
		return wire.Status{}, errors.New("no daemon")
	}
	return st, nil
}

func (f *fakeBackend) netPing(node string, size int) (wire.NetPing, error) {
	f.pingSize = size
	f.calls = append(f.calls, "ping "+node)
	if f.ping != nil {
		return f.ping(node)
	}
	return wire.NetPing{Node: peerOf(node), Address: "10.99.3.4", Path: wire.PathDirect, Endpoint: "192.0.2.2:6000", Via: []wire.NodeInfo{}, TimeMS: 1.5}, nil
}

func (f *fakeBackend) linksPing(node string) (wire.Ping, error) {
	f.calls = append(f.calls, "links "+node)
	if f.links != nil {
		return f.links(node)
	}
	return wire.Ping{Node: peerOf(node), Via: []wire.NodeInfo{}, TimeMS: 74.1}, nil
}

func (f *fakeBackend) directOf(node string) (wire.DirectReport, error) {
	f.calls = append(f.calls, "direct "+node)
	if f.direct != nil {
		return f.direct(node)
	}
	return wire.DirectReport{Node: peerOf(node)}, nil
}

func (f *fakeBackend) nodes() (wire.Topology, error) {
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
	home     string
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
		home:     filepath.Join(dir, "home"),
		euid:     1000,
		helper:   fakeHelper{installed: true, current: true},
	}
	e.back = &fakeBackend{config: e.config, linkStates: map[string]wire.LinkState{}}
	write(t, e.identity, string(pem.EncodeToMemory(block)))
	write(t, e.known, knownhosts.Line([]string{"ssh.example.com:22"}, signer.PublicKey())+"\n")
	host := sshconfig.Host{
		Hostname: "ssh.example.com", Port: "22", User: "tunnel",
		IdentityFiles: []string{e.identity}, KnownHostsFiles: []string{e.known},
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
		resolve: func(_ context.Context, destination string, sshArgs []string) (sshconfig.Host, error) {
			if h, ok := e.hosts[destination]; ok {
				if i := slices.Index(sshArgs, "-p"); i >= 0 && i+1 < len(sshArgs) {
					h.Port = sshArgs[i+1]
				}
				return h, nil
			}
			return sshconfig.Host{}, errors.New("ssh -G " + destination + ": no such host")
		},
		executable: func() (string, error) { return e.exe, nil },
		home:       func() (string, error) { return e.home, nil },
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
	e.mustGst("add", "myhost")
	e.mustGst("add", "db")

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
			"unlink":       {"error: missing argument <destination>\n", "usage: gst unlink <destination>...", "hint: this machine links to myhost, db\n", "Run 'gst unlink --help'"},
			"ad":           {`unknown command "ad" for "gst"`, `"add"`},
			"link x":       {`unknown command "link"`, "usage: gst <command>\n"},
			"nodes":        {`unknown command "nodes"`, "usage: gst <command>\n"},
			"frobnicate":   {`unknown command "frobnicate"`, "Run 'gst --help'"},
			"status a b":   {`error: unexpected argument "b"`, "usage: gst status [machine]"},
			"ping":         {"error: missing argument <machine>\n", "usage: gst ping <machine>"},
			"remove":       {"error: missing argument <machine>\n", "usage: gst remove <machine>"},
			"subnet a b":   {`error: unexpected argument "b"`, "usage: gst subnet [cidr]"},
			"add --nope":   {"error: unknown flag: --nope\n", "usage: gst add"},
			"add":          {"error: missing argument <destination>\n", "usage: gst add <destination>"},
			"add a b -- c": {`error: unexpected argument "b"`, "usage: gst add"},
		} {
			stderr := e.expectCode(exitUsage, strings.Fields(args)...)
			for _, w := range want {
				if !strings.Contains(stderr, w) {
					t.Errorf("gst %s: stderr %q lacks %q", args, stderr, w)
				}
			}
			if strings.HasPrefix(args, "add") && strings.Contains(stderr, "hint:") {
				t.Errorf("gst %s: a new link needs no list of links: %q", args, stderr)
			}
		}
	})

}

func TestEveryCommandNamesItsMissingArguments(t *testing.T) {
	e := newEnv(t)
	for args, want := range map[string]string{
		"add":      "<destination>",
		"unlink":   "<destination>",
		"remove":   "<machine>",
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
	write(t, e.config, `{"version":6,"surprise":true}`)
	e.expectCode(exitFailure, "status")
}

// --- gst add ---

func TestAdd(t *testing.T) {
	e := newEnv(t)
	out := e.mustGst("add", "myhost")
	if want := "Added myhost to the network.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	want := config.Link{Destination: "myhost", Admit: true}
	if got := e.link("myhost"); got == nil || !reflect.DeepEqual(*got, want) {
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

	// ssh options go with the destination.
	e.hosts["ssh.example.com"] = e.hosts["myhost"]
	e.back.linkStates["ssh.example.com"] = wire.LinkState{Destination: "ssh.example.com", State: wire.StateRunning, Peer: &wire.NodeInfo{ID: peerOf("web").ID, Name: "web"}}
	out = e.mustGst("add", "ssh.example.com", "--", "-l", "deploy", "-o", "ConnectTimeout=5")
	if want := "Added web to the network.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if got := e.link("ssh.example.com"); !slices.Equal(got.SSHArgs, []string{"-l", "deploy", "-o", "ConnectTimeout=5"}) {
		t.Fatalf("saved %+v", got)
	}

	// A machine of the network already.
	e.back.topology = wire.Topology{Nodes: []wire.TopoNode{{ID: peerOf("devbox").ID, Name: "devbox"}}}
	out = e.mustGst("add", "devbox")
	if want := "devbox is already in the network; this machine now also links to it.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}

	// Adding a destination again changes its link.
	out = e.mustGst("add", "devbox", "--", "-l", "deploy")
	if want := "Changed this machine's link to devbox.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if got := e.link("devbox"); !slices.Equal(got.SSHArgs, []string{"-l", "deploy"}) || !got.Admit || len(e.load().Links) != 3 {
		t.Fatalf("saved %+v", e.load().Links)
	}

	// A link that has not run yet connects in the background.
	e.back.linkStates["db"] = wire.LinkState{Destination: "db", State: wire.StateDown, Error: "connection refused"}
	out = e.mustGst("add", "db")
	if want := "Added the link to db. It connects in the background; see it with `gst status`.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
}

// gst add says when the other machine needs gst up.
func TestAddToAMachineThatIsNotUp(t *testing.T) {
	e := newEnv(t)
	e.back.linkStates["devbox"] = wire.LinkState{Destination: "devbox", State: wire.StateDown, Error: "the remote gst failed: error: " + notUp + "; run `gst up` there"}
	e.expectError(exitFailure, []string{"add", "devbox", "--", "-l", "deploy"},
		"error: added the link to devbox, but gst is not up there\n", "hint: run `ssh -t -l deploy devbox gst up`")
	if e.link("devbox") == nil {
		t.Fatal("the link was not saved")
	}
}

func TestAddRejects(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{
		{"add"},
		{"add", "--", "-oProxyCommand=x"},
		{"add", "my host"},
		{"add", "myhost", "db"},
		{"add", "myhost", "--no-such-flag"},
		{"add", "myhost", "--remote-gst", "/opt/gst"},
		// Settings that the ssh configuration owns are not flags.
		{"add", "myhost", "--identity", "/k"},
		{"add", "myhost", "--connect-timeout", "5s"},
		{"add", "myhost", "--", ""},
	} {
		if stderr := e.expectCode(exitUsage, args...); !strings.Contains(stderr, "--help") || !strings.Contains(stderr, "usage: gst add") {
			t.Errorf("gst %v: usage errors must show usage and point to help: %q", args, stderr)
		}
	}
	if f := e.load(); len(f.Links) != 0 || len(e.back.calls) != 0 {
		t.Fatalf("rejected links changed state: %+v %v", f.Links, e.back.calls)
	}
}

func TestAddPreflightFailures(t *testing.T) {
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
		{[]string{"add", "nowhere", "--", "-p", "2222"}, []string{"hint: check the destination and your ssh configuration with `ssh -G -p 2222 nowhere`\n"}},
		{[]string{"add", "nokey"}, []string{"error: no usable identity among /missing/key (missing)\n", "hint: gst needs a private key without a passphrase", "ssh-keygen", "ssh-copy-id"}},
		{[]string{"add", "locked"}, []string{"passphrase-protected", "hint: gst needs a private key without a passphrase"}},
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
		t.Fatalf("failed links changed state: %+v %v", f.Links, e.back.calls)
	}
}

func TestAddKeptWhenDaemonFails(t *testing.T) {
	e := newEnv(t)
	e.back.syncErr = errors.New("service manager unavailable")
	e.expectError(exitFailure, []string{"add", "myhost"},
		"error: added the link to myhost, but gst did not start: service manager unavailable\n", "hint: retry with `gst up`\n")
	if e.link("myhost") == nil {
		t.Fatal("the link was not saved")
	}
}

// --- gst unlink ---

func TestUnlink(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "myhost")
	e.mustGst("add", "db")
	e.mustGst("add", "devbox")

	// Nothing is removed unless every destination has a link.
	e.expectError(exitFailure, []string{"unlink", "myhost", "nope"}, "error: this machine has no link to nope\n", "hint: this machine links to ")
	if len(e.load().Links) != 3 {
		t.Fatal("a failed unlink changed the configuration")
	}

	e.resetCalls()
	out := e.mustGst("unlink", "myhost", "db")
	if want := "Unlinked myhost.\nUnlinked db.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if got := e.load().Links; len(got) != 1 || got[0].Destination != "devbox" {
		t.Fatalf("links left: %+v", got)
	}
	// Only this machine changes: nobody is removed from the network.
	if want := []string{"sync"}; !slices.Equal(e.back.calls, want) {
		t.Fatalf("calls %v, want %v", e.back.calls, want)
	}
	e.mustGst("unlink", "devbox")
	e.expectError(exitFailure, []string{"unlink", "db"}, "error: this machine has no link to db\n", "hint: this machine has no links yet; add one with `gst add <destination>`\n")
}

func TestUnlinkDaemonFailure(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "myhost")
	e.back.syncErr = errors.New("boom")
	e.expectError(exitFailure, []string{"unlink", "myhost"}, "may still run: boom", "hint: retry with `gst up`")
	if len(e.load().Links) != 0 {
		t.Fatal("the link stays configured")
	}
}

func TestCompletesLinks(t *testing.T) {
	e := newEnv(t)
	e.mustGst("add", "myhost")
	e.mustGst("add", "db")
	out := e.mustGst("__complete", "unlink", "")
	if got := strings.Fields(out); !slices.Equal(got[:2], []string{"myhost", "db"}) {
		t.Fatalf("completions %q", out)
	}
}

// --- gst remove ---

func TestRemove(t *testing.T) {
	e := newEnv(t)
	e.expectError(exitFailure, []string{"remove", "devbox"}, "error: gst is not running on this machine\n", "hint: start it with `gst up`")
	e.mustGst("up")
	e.back.remove = func(node string) (wire.Removal, error) {
		return wire.Removal{Node: peerOf(node), Stranded: []wire.NodeInfo{peerOf("gateway"), peerOf("tablet")}}, nil
	}
	out := e.mustGst("remove", "devbox.gst")
	if want := "Removed devbox from the network.\ngateway, tablet reached the network only through it, and stays offline until it links to another machine.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if !slices.Contains(e.back.calls, "remove devbox") {
		t.Fatalf("calls %v", e.back.calls)
	}
	e.back.remove = func(node string) (wire.Removal, error) { return wire.Removal{Node: peerOf(node), Self: true}, nil }
	if out := e.mustGst("remove", "laptop"); !strings.HasPrefix(out, "This machine left the network.") {
		t.Fatalf("output %q", out)
	}
	e.back.remove = func(string) (wire.Removal, error) {
		return wire.Removal{}, wire.Errorf(wire.CodeNotFound, "no machine \"nope\" in the network; see `gst status`")
	}
	e.expectError(exitFailure, []string{"remove", "nope"}, "error: no machine \"nope\" in the network")
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
