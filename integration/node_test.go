//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// node is a container that runs both sshd and gst. gst runs as the
// "tunnel" user, which is also the login user of sshd, with the systemctl
// shim of the image in place of systemd. Nodes reach each other by
// container IP; nothing is published on this machine.
type node struct {
	t         *testing.T
	container string
	ip        string
	hostKey   ssh.PublicKey
	direct    bool // its daemon tries UDP paths
}

const (
	nodeUser = "tunnel"
	nodeHome = "/home/" + nodeUser
	// nodeSSHConfig is the ssh configuration of the gst user. The tests
	// point GST_SSH_CONFIG at it, so that "ssh -G" resolves the links'
	// destinations from the Host blocks that trust adds.
	nodeSSHConfig = nodeHome + "/.gst-it-ssh-config"
)

func startNode(t *testing.T) *node {
	t.Helper()
	// SYS_PTRACE lets root inspect the sockets of non-dumpable sshd session
	// processes. It does not affect the unprivileged login user, and neither
	// do NET_ADMIN and NET_RAW, which the network helper needs.
	args := []string{"run", "-d", "--cap-add", "SYS_PTRACE", "--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW"}
	if filepath.Base(containerRuntime) == "container" {
		args = append(args, "--memory", "256M", "--cpus", "1")
	}
	marker, err := os.CreateTemp(filepath.Join(testDir, "containers"), "gst-it-"+filepath.Base(testDir)+"-")
	if err != nil {
		t.Fatal(err)
	}
	marker.Close()
	n := &node{t: t, container: filepath.Base(marker.Name())}
	t.Cleanup(func() {
		if t.Failed() {
			n.dumpLogs()
		}
		if err := removeContainer(n.container); err != nil {
			t.Error(err)
		}
	})
	if _, err := runtime(append(args, "--name", n.container, sshdImage)...); err != nil {
		t.Fatal(err)
	}
	n.ip = n.address()
	eventually(t, 30*time.Second, "sshd did not start", func() bool {
		_, err := n.exec("root", "sh", "-c", "test -s /etc/ssh/ssh_host_ed25519_key.pub && nc -z 127.0.0.1 22")
		return err == nil
	})
	line, err := n.exec("root", "cat", "/etc/ssh/ssh_host_ed25519_key.pub")
	if err != nil {
		t.Fatal(err)
	}
	if n.hostKey, _, _, _, err = ssh.ParseAuthorizedKey([]byte(line)); err != nil {
		t.Fatal(err)
	}
	for local, remote := range map[string]string{nodeBinary: "/usr/local/bin/gst", helperBinary: "/usr/local/bin/gst-it"} {
		data, err := os.ReadFile(local)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtimeInput(bytes.NewReader(data), "exec", "-i", "-u", "root", n.container, "sh", "-c", `cat >"$1" && chmod 755 "$1"`, "sh", remote); err != nil {
			t.Fatal(err)
		}
	}
	n.writeFile(nodeSSHConfig, nil)
	return n
}

// address returns the container's IP address, which other containers reach.
func (n *node) address() string {
	n.t.Helper()
	if filepath.Base(containerRuntime) == "container" {
		out, err := runtime("inspect", n.container)
		if err != nil {
			n.t.Fatal(err)
		}
		var containers []struct {
			Status struct {
				Networks []struct {
					IPv4Address string `json:"ipv4Address"`
				}
			}
		}
		if err := json.Unmarshal([]byte(out), &containers); err != nil {
			n.t.Fatal(err)
		}
		if len(containers) != 1 || len(containers[0].Status.Networks) == 0 {
			n.t.Fatal("container has no network address")
		}
		ip, _, err := net.ParseCIDR(containers[0].Status.Networks[0].IPv4Address)
		if err != nil {
			n.t.Fatal(err)
		}
		return ip.String()
	}
	out, err := runtime("inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", n.container)
	if err != nil {
		n.t.Fatal(err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		n.t.Fatal("container has no network address")
	}
	return fields[0]
}

// addr returns host:port on n for another container.
func (n *node) addr(port int) string { return net.JoinHostPort(n.ip, fmt.Sprint(port)) }

func (n *node) exec(user string, args ...string) (string, error) {
	return runtime(append([]string{"exec", "-u", user, n.container}, args...)...)
}

// sh runs a shell script as the gst user, with arguments as $1, $2, ...
func (n *node) sh(script string, args ...string) (string, error) {
	return n.exec(nodeUser, append([]string{"env", "HOME=" + nodeHome, "sh", "-c", script, "sh"}, args...)...)
}

func (n *node) mustSh(script string, args ...string) string {
	n.t.Helper()
	out, err := n.sh(script, args...)
	if err != nil {
		n.t.Fatal(err)
	}
	return out
}

// writeFile writes data to path as the gst user, readable only by it. The
// data goes through standard input, as the container CLI would take
// arguments such as "-----BEGIN" for options.
func (n *node) writeFile(path string, data []byte) {
	n.t.Helper()
	if _, err := runtimeInput(bytes.NewReader(data), "exec", "-i", "-u", nodeUser, n.container, "sh", "-c", `umask 077 && cat >"$1"`, "sh", path); err != nil {
		n.t.Fatal(err)
	}
}

// helper runs the gst-it network helper as the gst user.
func (n *node) helper(args ...string) (string, error) {
	return n.exec(nodeUser, append([]string{"gst-it"}, args...)...)
}

// gst runs gst as the gst user, with the test ssh configuration in place
// of any other, and returns its exit code, stdout, and stderr.
func (n *node) gst(args ...string) (int, string, string) {
	n.t.Helper()
	// UDP paths are off unless a test wants them, so that packets between
	// the containers stay on the links that it set up.
	direct := "off"
	if n.direct {
		direct = "on"
	}
	cmd := exec.Command(containerRuntime, append([]string{"exec", "-u", nodeUser, n.container,
		"env", "HOME=" + nodeHome, "GST_SSH_CONFIG=" + nodeSSHConfig, "GST_DIRECT=" + direct, "gst"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		n.t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), stdout.String(), stderr.String()
}

func (n *node) mustGst(args ...string) string {
	n.t.Helper()
	code, stdout, stderr := n.gst(args...)
	if code != 0 {
		n.t.Fatalf("gst %v exited %d\nstdout: %s\nstderr: %s", args, code, stdout, stderr)
	}
	return stdout
}

// pair is a client node whose gst user can log in to the "tunnel" user of
// a server node: the client's ssh configuration has a Host block for the
// server, named by its container, with an identity and known_hosts file.
type pair struct {
	client, server       *node
	identity, knownHosts string // paths on the client
}

func startPair(t *testing.T) *pair {
	t.Helper()
	return trust(t, startNode(t), startNode(t))
}

// trust lets client's gst user log in to server.
func trust(t *testing.T, client, server *node) *pair {
	t.Helper()
	p := &pair{client: client, server: server}
	private, signer := newIdentity(t)
	p.identity = nodeHome + "/.ssh/id_" + p.server.container
	p.knownHosts = nodeHome + "/.ssh/known_hosts"
	p.client.mustSh(`mkdir -p -m 700 ~/.ssh && touch ~/.ssh/known_hosts`)
	p.client.writeFile(p.identity, private)
	p.client.mustSh(`printf '%s\n' "$1" >>"$2"`, knownhosts.Line([]string{p.server.addr(22)}, p.server.hostKey), p.knownHosts)
	p.server.mustSh(`umask 077 && mkdir -p ~/.ssh && printf '%s' "$1" >>~/.ssh/authorized_keys`, string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	p.client.mustSh(`cat >>"$1"`+" <<EOF\n"+`Host $2
  HostName $3
  Port 22
  User `+nodeUser+`
  IdentityFile $4
  IdentitiesOnly yes
  UserKnownHostsFile $5
  ConnectTimeout 10
EOF`, nodeSSHConfig, p.destination(), p.server.ip, p.identity, p.knownHosts)
	p.server.mustGst("up")
	return p
}

// destination is the ssh destination of the server in the client's ssh
// configuration, for gst add.
func (p *pair) destination() string { return p.server.container }

// deadEnd adds a destination to the client's ssh configuration that
// passes gst add's checks, because known_hosts records the server's
// key on that port, but where nothing listens: a link to it stays down.
// It returns the destination.
func (p *pair) deadEnd() string {
	p.client.t.Helper()
	const port = "2222"
	alias := "dead-" + p.server.container
	p.client.mustSh(`printf '%s\n' "$1" >>"$2"`, knownhosts.Line([]string{net.JoinHostPort(p.server.ip, port)}, p.server.hostKey), p.knownHosts)
	p.client.mustSh(`cat >>"$1"`+" <<EOF\n"+`Host $2
  HostName $3
  Port `+port+`
  User `+nodeUser+`
  IdentityFile $4
  IdentitiesOnly yes
  UserKnownHostsFile $5
  ConnectTimeout 10
EOF`, nodeSSHConfig, alias, p.server.ip, p.identity, p.knownHosts)
	return alias
}

// roundtrip sends payloads to echo servers through addrs; see gst-it.
func (n *node) roundtrip(args ...string) error {
	_, err := n.helper(append([]string{"roundtrip"}, args...)...)
	return err
}

// daemonPID returns the PID of the gst daemon, or "" when none runs.
func (n *node) daemonPID() string {
	out, _ := n.exec("root", "pgrep", "-u", nodeUser, "-f", "gst daemon")
	return strings.TrimSpace(out)
}

func (n *node) dumpLogs() {
	out, _ := n.sh(`for f in ~/.systemctl-shim/*.log; do [ -f "$f" ] && echo "== $f" && cat "$f"; done`)
	n.t.Logf("logs of %s:\n%s", n.container, out)
	if out, err := n.exec("root", "sh", "-c", `cat /run/systemctl-shim/*.log`); err == nil {
		n.t.Logf("system logs of %s:\n%s", n.container, out)
	}
}

// signalSessions sends sig to the sshd processes serving the "tunnel" user,
// leaving the listening sshd alone.
func (n *node) signalSessions(sig string) {
	n.t.Helper()
	if _, err := n.exec("root", "pkill", "-"+sig, "-f", "^sshd: tunnel"); err != nil {
		n.t.Fatal(err)
	}
}

// killDaemon kills the gst daemon abruptly, as a crash would.
func (n *node) killDaemon() {
	n.t.Helper()
	if _, err := n.exec("root", "pkill", "-KILL", "-u", nodeUser, "-f", "gst daemon"); err != nil {
		n.t.Fatal(err)
	}
	eventually(n.t, 10*time.Second, "the daemon did not die", func() bool { return n.daemonPID() == "" })
}

// call calls method of the daemon on n's machine, as gst does, and
// decodes its result into result: gst prints what it learns only for
// people.
func (n *node) call(method string, params, result any) {
	n.t.Helper()
	args := []string{"call", method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			n.t.Fatal(err)
		}
		args = append(args, string(raw))
	}
	out, err := n.sh(`exec gst-it "$@"`, args...)
	if err != nil {
		n.t.Fatalf("%s on %s: %v", method, n.container, err)
	}
	if err := json.Unmarshal([]byte(out), result); err != nil {
		n.t.Fatalf("%s on %s: %v: %q", method, n.container, err, out)
	}
}

// status returns what the daemon on n's machine reports of itself.
func (n *node) status() wire.Status {
	n.t.Helper()
	var st wire.Status
	n.call(wire.MethodStatus, nil, &st)
	return st
}

// nodeStatus returns the status of the machine that ref names, asked
// through the network, as gst status <machine> does.
func (n *node) nodeStatus(ref string) wire.Status {
	n.t.Helper()
	var st wire.Status
	n.call(wire.MethodNodeStatus, wire.NodeParams{Node: ref}, &st)
	return st
}

// links returns the links of n's machine by destination.
func (n *node) links() map[string]wire.LinkState {
	n.t.Helper()
	m := map[string]wire.LinkState{}
	for _, l := range n.status().Links {
		m[l.Destination] = l
	}
	return m
}

// waitLink waits until the link to destination is in state and returns it.
func (n *node) waitLink(destination, state string) wire.LinkState {
	n.t.Helper()
	var last wire.LinkState
	deadline := time.Now().Add(60 * time.Second)
	for {
		if last = n.links()[destination]; last.State == state {
			return last
		}
		if time.Now().After(deadline) {
			n.t.Fatalf("link to %s of %s is %q (%s), want %q", destination, n.container, last.State, last.Error, state)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// nodes returns the network as n's machine knows it.
func (n *node) nodes() wire.Topology {
	n.t.Helper()
	var topo wire.Topology
	n.call(wire.MethodNodes, nil, &topo)
	return topo
}

// find returns the node of topo with the given name.
func find(topo wire.Topology, name string) (wire.TopoNode, bool) {
	for _, n := range topo.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return wire.TopoNode{}, false
}

// self returns the node of topo that is its own machine.
func self(topo wire.Topology) wire.TopoNode {
	for _, n := range topo.Nodes {
		if n.ID == topo.Self {
			return n
		}
	}
	return wire.TopoNode{}
}

// path names the machines on the way to node, from n's machine.
func path(topo wire.Topology, node wire.TopoNode) string {
	names := make([]string, len(node.Path))
	for i, id := range node.Path {
		for _, v := range topo.Nodes {
			if v.ID == id {
				names[i] = v.Name
			}
		}
	}
	return strings.Join(names, ",")
}

// waitReachable waits until n can reach the machine called name, and
// returns it with the topology that shows it.
func (n *node) waitReachable(name string) (wire.TopoNode, wire.Topology) {
	n.t.Helper()
	var topo wire.Topology
	deadline := time.Now().Add(60 * time.Second)
	for {
		topo = n.nodes()
		if v, ok := find(topo, name); ok && v.Reachable {
			return v, topo
		}
		if time.Now().After(deadline) {
			n.t.Fatalf("%s cannot reach %s; nodes: %+v", n.container, name, topo.Nodes)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// waitUnreachable waits until n cannot reach the machine called name.
func (n *node) waitUnreachable(name string) {
	n.t.Helper()
	eventually(n.t, 60*time.Second, n.container+" still reaches "+name, func() bool {
		v, ok := find(n.nodes(), name)
		return !ok || !v.Reachable
	})
}

// startEchoOn starts an echo server on addr, a host:port.
func (n *node) startEchoOn(addr string) {
	n.t.Helper()
	n.mustSh(`setsid gst-it echo-server "$1" </dev/null >/dev/null 2>&1 &`, addr)
	eventually(n.t, 5*time.Second, "echo server on "+addr+" did not start", func() bool {
		host, port, _ := net.SplitHostPort(addr)
		_, err := n.sh(`nc -z "$1" "$2"`, host, port)
		return err == nil
	})
}

// start3 starts three nodes named a, b, and c, where a and c link to b.
// It waits until a and c reach each other through b.
func start3(t *testing.T) (a, b, c *node, ab, cb *pair) {
	t.Helper()
	return link3(t, startNode(t), startNode(t), startNode(t))
}

// link3 links a and c to b, as start3 does.
func link3(t *testing.T, a, b, c *node) (_, _, _ *node, ab, cb *pair) {
	t.Helper()
	ab, cb = trust(t, a, b), trust(t, c, b)
	for name, n := range map[string]*node{"a": a, "b": b, "c": c} {
		n.mustGst("name", name)
	}
	a.mustGst("add", ab.destination())
	c.mustGst("add", cb.destination())
	a.waitReachable("c")
	c.waitReachable("a")
	return a, b, c, ab, cb
}
