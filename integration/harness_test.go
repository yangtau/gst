//go:build integration

// Package integration runs the tunnel binary against a real OpenSSH server
// in a disposable container, one container per test. Run it with
// "make test-integration"; see README for the environment variables.
package integration

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
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
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"reverse-ssh-tunnel/internal/config"
)

// forwardPorts are the container ports tests may use as remote listen ports.
// Each is published on a random loopback port of the host.
var forwardPorts = []int{2222, 3000}

var (
	containerRuntime string // podman or docker
	sshdImage        string
	tunnelBinary     string
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	os.Setenv("PODMAN_IGNORE_CGROUPSV1_WARNING", "1")
	dir, err := os.MkdirTemp("", "ssh-tunnel-integration-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)

	if err := setup(dir); err != nil {
		fmt.Fprintln(os.Stderr, "integration setup:", err)
		return 1
	}
	return m.Run()
}

func setup(dir string) error {
	containerRuntime = os.Getenv("CONTAINER_RUNTIME")
	if containerRuntime == "" {
		for _, candidate := range []string{"podman", "docker"} {
			if _, err := exec.LookPath(candidate); err == nil {
				containerRuntime = candidate
				break
			}
		}
	}
	if containerRuntime == "" {
		return errors.New("no container runtime found; install podman or docker, or set CONTAINER_RUNTIME")
	}

	tunnelBinary = filepath.Join(dir, "reverse-ssh-tunnel")
	if out, err := exec.Command("go", "build", "-o", tunnelBinary, "..").CombinedOutput(); err != nil {
		return fmt.Errorf("build tunnel binary: %w\n%s", err, out)
	}
	return ensureImage()
}

// ensureImage builds the sshd image unless one built from the same inputs
// already exists, so repeated runs skip the build.
func ensureImage() error {
	baseImage := os.Getenv("IT_BASE_IMAGE")
	tag, err := contentHash(baseImage, "sshd/Containerfile", "sshd/sshd_config")
	if err != nil {
		return err
	}
	sshdImage = "localhost/ssh-tunnel-sshd:" + tag
	if _, err := runtime(nil, "image", "inspect", sshdImage); err == nil {
		return nil
	}

	args := []string{"build"}
	args = append(args, strings.Fields(os.Getenv("IT_BUILD_FLAGS"))...)
	if baseImage != "" {
		args = append(args, "--build-arg", "BASE_IMAGE="+baseImage)
	}
	if mirror := os.Getenv("IT_APT_MIRROR"); mirror != "" {
		args = append(args, "--build-arg", "APT_MIRROR="+mirror)
	}
	args = append(args, "-t", sshdImage, "sshd")
	cmd := exec.Command(containerRuntime, args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build sshd image: %w", err)
	}
	return nil
}

func contentHash(extra string, files ...string) (string, error) {
	h := sha256.New()
	io.WriteString(h, extra)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// runtime runs the container CLI and returns its standard output.
func runtime(stdin io.Reader, args ...string) (string, error) {
	cmd := exec.Command(containerRuntime, args...)
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", containerRuntime, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// sshServer is a running sshd container. The "tunnel" user accepts the
// generated identity; the "other" user exists to own foreign processes.
type sshServer struct {
	t          *testing.T
	container  string
	addr       string // host address of the container's port 22
	identity   string // private key file for the "tunnel" user
	knownHosts string
}

func startSSHServer(t *testing.T) *sshServer {
	t.Helper()
	// SYS_PTRACE lets root inspect the sockets of non-dumpable sshd session
	// processes. It does not affect the unprivileged login user.
	args := []string{"run", "-d", "--rm", "--cap-add", "SYS_PTRACE", "-p", "127.0.0.1::22"}
	for _, port := range forwardPorts {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1::%d", port))
	}
	args = append(args, sshdImage)
	// The runtime picks a free host port and binds it later, so another
	// process can take it in between. Retrying picks a new port.
	out, err := runtime(nil, args...)
	for attempt := 1; err != nil && strings.Contains(strings.ToLower(err.Error()), "address already in use") && attempt < 5; attempt++ {
		out, err = runtime(nil, args...)
	}
	if err != nil {
		t.Fatal(err)
	}
	s := &sshServer{t: t, container: strings.TrimSpace(out)}
	t.Cleanup(func() { runtime(nil, "kill", s.container) })

	s.addr = s.hostAddr(22)
	s.waitForBanner()
	s.installIdentity()
	s.writeKnownHosts()
	return s
}

// hostAddr returns the host address published for a container port.
func (s *sshServer) hostAddr(containerPort int) string {
	s.t.Helper()
	out, err := runtime(nil, "port", s.container, fmt.Sprintf("%d/tcp", containerPort))
	if err != nil {
		s.t.Fatal(err)
	}
	addr, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return addr
}

// waitForBanner waits until sshd itself answers. The published port accepts
// connections before sshd listens, so a successful TCP dial is not enough.
func (s *sshServer) waitForBanner() {
	s.t.Helper()
	eventually(s.t, 30*time.Second, "sshd did not start", func() bool {
		conn, err := net.DialTimeout("tcp", s.addr, time.Second)
		if err != nil {
			return false
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(time.Second))
		line, _ := bufio.NewReader(conn).ReadString('\n')
		return strings.HasPrefix(line, "SSH-2.0-")
	})
}

func (s *sshServer) installIdentity() {
	s.t.Helper()
	var signer ssh.Signer
	s.identity, signer = newIdentity(s.t)
	const install = `f=/home/tunnel/.ssh/authorized_keys; cat > $f && chown tunnel:tunnel $f && chmod 600 $f`
	if _, err := runtime(bytes.NewReader(ssh.MarshalAuthorizedKey(signer.PublicKey())), "exec", "-i", s.container, "sh", "-c", install); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sshServer) writeKnownHosts() {
	s.t.Helper()
	line, err := s.exec("root", "cat", "/etc/ssh/ssh_host_ed25519_key.pub")
	if err != nil {
		s.t.Fatal(err)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		s.t.Fatal(err)
	}
	s.knownHosts = filepath.Join(s.t.TempDir(), "known_hosts")
	writeFile(s.t, s.knownHosts, []byte(knownhosts.Line([]string{s.addr}, key)+"\n"))
}

// config returns a configuration for the "tunnel" user with a one-second
// keepalive interval and a two-second keepalive timeout.
func (s *sshServer) config(cleanup bool, forwards ...config.Forward) config.Config {
	return config.Config{
		Server:                   s.addr,
		User:                     "tunnel",
		IdentityFile:             s.identity,
		KnownHostsFile:           s.knownHosts,
		ConnectTimeoutSeconds:    10,
		KeepaliveIntervalSeconds: 1,
		KeepaliveCountMax:        2,
		CleanupListeners:         cleanup,
		Forwards:                 forwards,
	}
}

func (s *sshServer) exec(user string, args ...string) (string, error) {
	return runtime(nil, append([]string{"exec", "-u", user, s.container}, args...)...)
}

// startListener occupies a container port with a plain TCP listener owned by user.
func (s *sshServer) startListener(user string, port int) {
	s.t.Helper()
	if _, err := runtime(nil, "exec", "-d", "-u", user, s.container, "nc", "-lk", fmt.Sprint(port)); err != nil {
		s.t.Fatal(err)
	}
	eventually(s.t, 5*time.Second, "listener did not start", func() bool {
		return slices.Contains(s.listenerOwners(port), user)
	})
}

// listenerOwners returns the users owning processes that listen on port.
func (s *sshServer) listenerOwners(port int) []string {
	out, _ := s.exec("root", "lsof", "-nP", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN", "-F", "L")
	var owners []string
	for line := range strings.Lines(out) {
		if user, ok := strings.CutPrefix(strings.TrimSpace(line), "L"); ok && !slices.Contains(owners, user) {
			owners = append(owners, user)
		}
	}
	return owners
}

// hasProcess reports whether user runs a process named name.
func (s *sshServer) hasProcess(user, name string) bool {
	_, err := s.exec("root", "pgrep", "-u", user, "-x", name)
	return err == nil
}

// signalSessions sends sig to the sshd processes serving the "tunnel" user,
// leaving the listening sshd alone.
func (s *sshServer) signalSessions(sig string) {
	s.t.Helper()
	if _, err := s.exec("root", "pkill", "-"+sig, "-f", "^sshd: tunnel"); err != nil {
		s.t.Fatal(err)
	}
}

// kill stops the whole container abruptly.
func (s *sshServer) kill() {
	s.t.Helper()
	if _, err := runtime(nil, "kill", s.container); err != nil {
		s.t.Fatal(err)
	}
}

// tunnelProcess is a running tunnel binary whose log output is collected.
type tunnelProcess struct {
	cmd      *exec.Cmd
	ready    chan struct{} // closed when "tunnel ready" is logged
	exited   chan struct{} // closed after the process exits
	exitCode int

	mu     sync.Mutex
	output strings.Builder
}

func startTunnel(t *testing.T, cfg config.Config) *tunnelProcess {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tunnel.json")
	writeFile(t, path, data)

	p := &tunnelProcess{
		cmd:    exec.Command(tunnelBinary, path),
		ready:  make(chan struct{}),
		exited: make(chan struct{}),
	}
	stderr, err := p.cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go p.collect(stderr)
	t.Cleanup(func() {
		p.cmd.Process.Kill()
		<-p.exited
		if t.Failed() {
			t.Logf("tunnel log:\n%s", p.logs())
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

func (p *tunnelProcess) signal(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
}

// startEchoTarget starts a local service that reads a request until EOF,
// then answers "reply:" + request and half-closes.
func startEchoTarget(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(30 * time.Second))
				request, err := io.ReadAll(conn)
				if err != nil {
					return
				}
				conn.Write(append([]byte("reply:"), request...))
				conn.(*net.TCPConn).CloseWrite()
			}()
		}
	}()
	return listener.Addr().String()
}

// echoRoundTrip sends payload to an echo target through addr and checks the reply.
func echoRoundTrip(addr, payload string) error {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.WriteString(conn, payload); err != nil {
		return err
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		return err
	}
	reply, err := io.ReadAll(conn)
	if err != nil {
		return err
	}
	if string(reply) != "reply:"+payload {
		return fmt.Errorf("got %d reply bytes, want %d", len(reply), len("reply:")+len(payload))
	}
	return nil
}

// unusedAddr returns a loopback address that refuses connections.
func unusedAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	return addr
}

func eventually(t *testing.T, limit time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// newIdentity writes a fresh unencrypted ed25519 private key to a file.
func newIdentity(t *testing.T) (string, ssh.Signer) {
	t.Helper()
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
	path := filepath.Join(t.TempDir(), "id_ed25519")
	writeFile(t, path, pem.EncodeToMemory(block))
	return path, signer
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
