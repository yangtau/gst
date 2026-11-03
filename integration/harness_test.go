//go:build integration

// Package integration runs gst in disposable containers against real
// OpenSSH servers. Each test starts its own node containers, which run both
// sshd and gst and reach each other by container IP; nothing runs or
// listens on this machine. Run it with "make test-integration"; see README
// for the environment variables.
package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

var (
	containerRuntime string // podman, docker, or Apple's container
	sshdImage        string
	nodeBinary       string // gst for Linux in the containers
	helperBinary     string // ithelper for Linux in the containers
	testDir          string
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func setup(ctx context.Context, dir string) error {
	containerRuntime = os.Getenv("CONTAINER_RUNTIME")
	if containerRuntime == "" {
		candidates := []string{"podman", "docker"}
		if goruntime.GOOS == "darwin" {
			candidates = append([]string{"container"}, candidates...)
		}
		for _, candidate := range candidates {
			if _, err := exec.LookPath(candidate); err == nil {
				containerRuntime = candidate
				break
			}
		}
	}
	if containerRuntime == "" {
		return errors.New("no container runtime found; install podman, docker, or container, or set CONTAINER_RUNTIME")
	}

	if err := requireImage(); err != nil {
		return err
	}
	nodeBinary, helperBinary = binaries(dir)
	// The containers run Linux on this machine's architecture.
	for output, pkg := range map[string]string{nodeBinary: "../cmd/gst", helperBinary: "./ithelper"} {
		cmd := exec.CommandContext(ctx, "go", "build", "-tags", "integration", "-o", output, pkg)
		cmd.Env = append(os.Environ(), "TMPDIR="+dir, "GOTMPDIR="+dir, "GOCACHE="+filepath.Join(dir, "go-cache"), "GOMODCACHE="+filepath.Join(dir, "go-mod"), "GOFLAGS=-mod=readonly -modcacherw", "GOTOOLCHAIN=local",
			"GOOS=linux", "GOARCH="+goruntime.GOARCH, "CGO_ENABLED=0")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build %s: %w\n%s", pkg, err, out)
		}
	}
	return nil
}

func binaries(dir string) (gst, helper string) {
	return filepath.Join(dir, "gst-linux"), filepath.Join(dir, "gst-it-linux")
}

// The image is a prerequisite: building it here would alter shared builder
// state and image caches outside the test directory.
func requireImage() error {
	baseImage := os.Getenv("IT_BASE_IMAGE")
	tag, err := contentHash(baseImage, "sshd/Containerfile", "sshd/sshd_config", "sshd/systemctl")
	if err != nil {
		return err
	}
	sshdImage = "localhost/ssh-tunnel-sshd:" + tag
	if _, err := runtime("image", "inspect", sshdImage); err == nil {
		return nil
	}
	args := []string{containerRuntime, "build"}
	if baseImage != "" {
		args = append(args, "--build-arg", "BASE_IMAGE="+baseImage)
	}
	if mirror := os.Getenv("IT_APT_MIRROR"); mirror != "" {
		args = append(args, "--build-arg", "APT_MIRROR="+mirror)
	}
	args = append(args, "-f", "sshd/Containerfile", "-t", sshdImage, "sshd")
	return fmt.Errorf("sshd image is unavailable; prepare it separately from integration/: %s", strings.Join(args, " "))
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
func runtime(args ...string) (string, error) {
	return runtimeInput(nil, args...)
}

// runtimeInput runs the container CLI with stdin as its standard input.
func runtimeInput(stdin io.Reader, args ...string) (string, error) {
	timeout := 30 * time.Second
	if args[0] == "run" {
		// Apple's container may download its init image on a cold start.
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, containerRuntime, args...)
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("timed out after %s: %w", timeout, ctx.Err())
		}
		return string(out), fmt.Errorf("%s %s: %w: %s", containerRuntime, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
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

// newIdentity returns a fresh unencrypted ed25519 private key in OpenSSH
// PEM form and its signer.
func newIdentity(t *testing.T) ([]byte, ssh.Signer) {
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
	return pem.EncodeToMemory(block), signer
}
