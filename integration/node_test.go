//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/yangtau/ssh-tunnel/internal/config"
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
}

const (
	nodeUser = "tunnel"
	nodeHome = "/home/" + nodeUser
)

func startNode(t *testing.T) *node {
	t.Helper()
	// SYS_PTRACE lets root inspect the sockets of non-dumpable sshd session
	// processes. It does not affect the unprivileged login user.
	args := []string{"run", "-d", "--cap-add", "SYS_PTRACE"}
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

// gst runs gst as the gst user, ignoring any ssh configuration, and
// returns its exit code, stdout, and stderr.
func (n *node) gst(args ...string) (int, string, string) {
	n.t.Helper()
	cmd := exec.Command(containerRuntime, append([]string{"exec", "-u", nodeUser, n.container,
		"env", "HOME=" + nodeHome, "GST_SSH_CONFIG=/dev/null", "gst"}, args...)...)
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
// a server node with an identity and known_hosts file on the client.
type pair struct {
	client, server       *node
	identity, knownHosts string // paths on the client
}

func startPair(t *testing.T) *pair {
	t.Helper()
	p := &pair{client: startNode(t), server: startNode(t)}
	private, signer := newIdentity(t)
	p.identity = nodeHome + "/.ssh/id_" + p.server.container
	p.knownHosts = nodeHome + "/.ssh/known_hosts"
	p.client.mustSh(`mkdir -p -m 700 ~/.ssh`)
	p.client.writeFile(p.identity, private)
	p.client.mustSh(`printf '%s\n' "$1" >>"$2"`, knownhosts.Line([]string{p.server.addr(22)}, p.server.hostKey), p.knownHosts)
	p.server.mustSh(`umask 077 && mkdir -p ~/.ssh && printf '%s' "$1" >>~/.ssh/authorized_keys`, string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	return p
}

func (p *pair) destination() string { return "ssh://tunnel@" + p.server.addr(22) }

// flags selects the server for gst add, with fast keepalives.
func (p *pair) flags() []string {
	return []string{p.destination(), "--identity", p.identity, "--known-hosts", p.knownHosts,
		"--keepalive-interval", "1s", "--keepalive-count", "2", "--connect-timeout", "10s"}
}

// config returns a tunnel to the server with a one-second keepalive
// interval and a two-second keepalive timeout.
func (p *pair) config(cleanup bool, forwards ...config.Forward) config.Tunnel {
	return config.Tunnel{
		Destination:              p.destination(),
		IdentityFile:             p.identity,
		KnownHostsFile:           p.knownHosts,
		ConnectTimeoutSeconds:    10,
		KeepaliveIntervalSeconds: 1,
		KeepaliveCountMax:        2,
		CleanupListeners:         cleanup,
		Forwards:                 forwards,
	}
}

// secrets are what tunnel logs must never contain.
func (p *pair) secrets() []string {
	return []string{p.server.ip, p.identity, p.knownHosts}
}

// serve answers every connection to 127.0.0.1:port with reply.
func (n *node) serve(port int, reply string) {
	n.t.Helper()
	n.mustSh(`setsid sh -c 'while :; do printf "%s" "$1" | nc -N -l 127.0.0.1 "$2"; done' sh "$1" "$2" </dev/null >/dev/null 2>&1 &`, reply, fmt.Sprint(port))
	n.waitListening(port)
}

// startEcho starts an echo server on 127.0.0.1:port; see gst-it.
func (n *node) startEcho(port int) {
	n.t.Helper()
	n.mustSh(`setsid gst-it echo-server "127.0.0.1:$1" </dev/null >/dev/null 2>&1 &`, fmt.Sprint(port))
	n.waitListening(port)
}

func (n *node) waitListening(port int) {
	n.t.Helper()
	eventually(n.t, 5*time.Second, fmt.Sprintf("nothing listens on port %d", port), func() bool {
		_, err := n.sh(`nc -z 127.0.0.1 "$1"`, fmt.Sprint(port))
		return err == nil
	})
}

// fetch connects to 127.0.0.1:port and returns what it answers.
func (n *node) fetch(port int) (string, error) {
	return n.sh(`nc -N -w 5 127.0.0.1 "$1" </dev/null`, fmt.Sprint(port))
}

// eventuallyFetches waits until 127.0.0.1:port answers want.
func (n *node) eventuallyFetches(port int, want string) {
	n.t.Helper()
	eventually(n.t, 20*time.Second, fmt.Sprintf("port %d did not answer %q", port, want), func() bool {
		got, err := n.fetch(port)
		return err == nil && got == want
	})
}

// roundtrip sends payloads to echo servers through addrs; see gst-it.
func (n *node) roundtrip(args ...string) error {
	_, err := n.helper(append([]string{"roundtrip"}, args...)...)
	return err
}

type nodeTunnel struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

func (n *node) tunnels() map[string]nodeTunnel {
	n.t.Helper()
	var list []nodeTunnel
	if err := json.Unmarshal([]byte(n.mustGst("list", "--json")), &list); err != nil {
		n.t.Fatal(err)
	}
	m := map[string]nodeTunnel{}
	for _, v := range list {
		m[v.Name] = v
	}
	return m
}

func (n *node) waitStatus(name, status string) {
	n.t.Helper()
	var last nodeTunnel
	deadline := time.Now().Add(30 * time.Second)
	for {
		if last = n.tunnels()[name]; last.Status == status {
			return
		}
		if time.Now().After(deadline) {
			n.t.Fatalf("tunnel %s is %s (%s), want %s", name, last.Status, last.Error, status)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// daemonPID returns the PID of the gst daemon, or "" when none runs.
func (n *node) daemonPID() string {
	out, _ := n.exec("root", "pgrep", "-u", nodeUser, "-f", "gst daemon")
	return strings.TrimSpace(out)
}

func (n *node) dumpLogs() {
	out, _ := n.sh(`for f in ~/.systemctl-shim/*.log ~/.local/state/gst/logs/*.log; do [ -f "$f" ] && echo "== $f" && cat "$f"; done`)
	n.t.Logf("logs of %s:\n%s", n.container, out)
}

// startListener occupies a port with a plain TCP listener owned by user.
func (n *node) startListener(user string, port int) {
	n.t.Helper()
	if _, err := n.exec(user, "sh", "-c", `nc -lk "$1" </dev/null >/dev/null 2>&1 &`, "sh", fmt.Sprint(port)); err != nil {
		n.t.Fatal(err)
	}
	eventually(n.t, 5*time.Second, "listener did not start", func() bool {
		return slices.Contains(n.listenerOwners(port), user)
	})
}

// listenerOwners returns the users owning processes that listen on port.
func (n *node) listenerOwners(port int) []string {
	out, _ := n.exec("root", "lsof", "-nP", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN", "-F", "L")
	var owners []string
	for line := range strings.Lines(out) {
		if user, ok := strings.CutPrefix(strings.TrimSpace(line), "L"); ok && !slices.Contains(owners, user) {
			owners = append(owners, user)
		}
	}
	return owners
}

// hasProcess reports whether user runs a process named name.
func (n *node) hasProcess(user, name string) bool {
	_, err := n.exec("root", "pgrep", "-u", user, "-x", name)
	return err == nil
}

// signalSessions sends sig to the sshd processes serving the "tunnel" user,
// leaving the listening sshd alone.
func (n *node) signalSessions(sig string) {
	n.t.Helper()
	if _, err := n.exec("root", "pkill", "-"+sig, "-f", "^sshd: tunnel"); err != nil {
		n.t.Fatal(err)
	}
}

// kill stops the whole container abruptly.
func (n *node) kill() {
	n.t.Helper()
	if _, err := runtime("kill", n.container); err != nil {
		n.t.Fatal(err)
	}
}

// tunnelProcess is "gst run" in the foreground on a node, with its log
// output collected.
type tunnelProcess struct {
	n        *node
	pidFile  string
	cmd      *exec.Cmd
	ready    chan struct{} // closed when "tunnel ready" is logged
	exited   chan struct{} // closed after the process exits
	exitCode int

	mu     sync.Mutex
	output strings.Builder
}

var tunnelRuns struct {
	sync.Mutex
	n int
}

// startTunnel runs the tunnel on n in the foreground with "gst run",
// ignoring any ssh configuration.
func (n *node) startTunnel(tunnel config.Tunnel) *tunnelProcess {
	n.t.Helper()
	data, err := json.Marshal(config.File{Tunnels: map[string]*config.Tunnel{"it": &tunnel}})
	if err != nil {
		n.t.Fatal(err)
	}
	tunnelRuns.Lock()
	tunnelRuns.n++
	dir := fmt.Sprintf("%s/run-%d", nodeHome, tunnelRuns.n)
	tunnelRuns.Unlock()
	n.mustSh(`mkdir -p "$1"`, dir)
	n.writeFile(dir+"/config.json", data)

	p := &tunnelProcess{
		n:       n,
		pidFile: dir + "/pid",
		cmd: exec.Command(containerRuntime, "exec", "-u", nodeUser, n.container, "env", "HOME="+nodeHome, "GST_SSH_CONFIG=/dev/null",
			"sh", "-c", `echo $$ >"$1/pid" && exec gst --config "$1/config.json" run it`, "sh", dir),
		ready:  make(chan struct{}),
		exited: make(chan struct{}),
	}
	stderr, err := p.cmd.StderrPipe()
	if err != nil {
		n.t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		n.t.Fatal(err)
	}
	go p.collect(stderr)
	n.t.Cleanup(func() {
		if p.running() {
			n.exec("root", "sh", "-c", `kill -KILL "$(cat "$1")"`, "sh", p.pidFile)
			select {
			case <-p.exited:
			case <-time.After(10 * time.Second):
				p.cmd.Process.Kill()
				<-p.exited
			}
		}
		if n.t.Failed() {
			n.t.Logf("tunnel log:\n%s", p.logs())
		}
	})
	return p
}

func (p *tunnelProcess) collect(stderr io.Reader) {
	var readyOnce sync.Once
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		line := scanner.Text()
		p.mu.Lock()
		p.output.WriteString(line + "\n")
		p.mu.Unlock()
		if strings.HasSuffix(line, "tunnel ready") {
			readyOnce.Do(func() { close(p.ready) })
		}
	}
	p.cmd.Wait()
	p.exitCode = p.cmd.ProcessState.ExitCode()
	close(p.exited)
}

func (p *tunnelProcess) logs() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.output.String()
}

// assertPrivateLog fails if the log contains any of secrets.
func (p *tunnelProcess) assertPrivateLog(t *testing.T, secrets ...string) {
	t.Helper()
	logs := p.logs()
	for _, secret := range secrets {
		if strings.Contains(logs, secret) {
			t.Errorf("log reveals %q", secret)
		}
	}
}

func (p *tunnelProcess) waitReady(t *testing.T) {
	t.Helper()
	select {
	case <-p.ready:
	case <-p.exited:
		t.Fatalf("tunnel exited with code %d before becoming ready", p.exitCode)
	case <-time.After(20 * time.Second):
		t.Fatal("tunnel did not become ready")
	}
}

// waitExit waits up to limit for the process to exit and returns its exit
// code and how long the wait took.
func (p *tunnelProcess) waitExit(t *testing.T, limit time.Duration) (int, time.Duration) {
	t.Helper()
	start := time.Now()
	select {
	case <-p.exited:
		return p.exitCode, time.Since(start)
	case <-time.After(limit):
		t.Fatalf("tunnel still running after %v", limit)
		panic("unreachable")
	}
}

func (p *tunnelProcess) running() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// signal sends a signal such as "TERM" or "STOP" to gst in its container.
func (p *tunnelProcess) signal(t *testing.T, sig string) {
	t.Helper()
	if _, err := p.n.exec("root", "sh", "-c", `kill -"$1" "$(cat "$2")"`, "sh", sig, p.pidFile); err != nil {
		t.Fatal(err)
	}
}
