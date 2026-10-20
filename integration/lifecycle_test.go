//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A supervising process owns the workspace and container names. It can
// clean up even when the test child panics, times out, or is interrupted.
func runTests(m *testing.M) (code int) {
	if testDir = os.Getenv("GST_INTEGRATION_CHILD"); testDir != "" {
		containerRuntime = os.Getenv("CONTAINER_RUNTIME")
		tunnelBinary = filepath.Join(testDir, "gst")
		if err := requireImage(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return m.Run()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dir, err := os.MkdirTemp(".", ".run-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	testDir, err = filepath.Abs(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.RemoveAll(dir)
		return 1
	}
	defer func() {
		if err := cleanup(); err != nil {
			fmt.Fprintln(os.Stderr, "integration cleanup:", err)
			code = 1
		}
	}()
	if err := os.Mkdir(filepath.Join(testDir, "containers"), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := setup(ctx, testDir); err != nil {
		fmt.Fprintln(os.Stderr, "integration setup:", err)
		return 1
	}
	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Env = append(os.Environ(), "GST_INTEGRATION_CHILD="+testDir, "CONTAINER_RUNTIME="+containerRuntime, "TMPDIR="+testDir, "PODMAN_IGNORE_CGROUPSV1_WARNING=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return 0
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() > 0 {
			return exit.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		fmt.Fprintln(os.Stderr, "integration tests interrupted")
	}
	return 1
}

func removeContainer(name string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := runtime("rm", "--force", name)
		if err == nil {
			break
		}
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "notfound:") ||
			strings.Contains(message, "no such container") || strings.Contains(message, "does not exist") {
			break
		}
		if time.Now().After(deadline) {
			return err
		}
		// A killed VM can still be stopping when the kill command returns.
		time.Sleep(100 * time.Millisecond)
	}
	return os.Remove(filepath.Join(testDir, "containers", name))
}

func cleanup() error {
	names, err := os.ReadDir(filepath.Join(testDir, "containers"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var errs []error
	for _, entry := range names {
		if err := removeContainer(entry.Name()); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w; resource records retained in %s", errors.Join(errs...), testDir)
	}
	return os.RemoveAll(testDir)
}
