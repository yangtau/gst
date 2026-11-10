package sshconn

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func dialServer(t *testing.T, s *testServer) *Conn {
	t.Helper()
	c, err := Dial(context.Background(), s.opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestDial(t *testing.T) {
	s := startServer(t)
	c := dialServer(t, s)
	select {
	case <-c.Done():
		t.Fatal("connection ended early")
	default:
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	await(t, c.Done())
	if err := c.Err(); err != errClosed {
		t.Fatalf("Err = %v, want %v", err, errClosed)
	}
	c.Close() // again
}

func TestDialContextCancelClosesConnection(t *testing.T) {
	s := startServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	c, err := Dial(ctx, s.opts)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	await(t, c.Done())
	if c.Err() != errClosed {
		t.Fatalf("Err = %v", c.Err())
	}
}

func TestDialFailures(t *testing.T) {
	encrypted := func(t *testing.T, s *testServer) { writeFile(t, s.opts.IdentityFiles[0], encryptedPEM()) }
	tests := []struct {
		name   string
		change func(t *testing.T, s *testServer)
		want   string
	}{
		{"unknown host", func(t *testing.T, s *testServer) { writeFile(t, s.opts.KnownHostsFiles[0], "") }, "SSH handshake failed"},
		{"host key mismatch", func(t *testing.T, s *testServer) {
			other, _ := newKey(t)
			writeFile(t, s.opts.KnownHostsFiles[0], knownhosts.Line([]string{s.opts.Server}, other.PublicKey())+"\n")
		}, "SSH handshake failed"},
		{"revoked host key", func(t *testing.T, s *testServer) {
			writeFile(t, s.opts.KnownHostsFiles[0], "@revoked "+knownhosts.Line([]string{s.opts.Server}, s.hostKey.PublicKey())+"\n")
		}, "SSH handshake failed"},
		{"known_hosts missing", func(t *testing.T, s *testServer) { os.Remove(s.opts.KnownHostsFiles[0]) }, "known hosts load failed"},
		{"known_hosts malformed", func(t *testing.T, s *testServer) { writeFile(t, s.opts.KnownHostsFiles[0], "SECRET malformed") }, "known hosts load failed"},
		{"unauthorized key", func(t *testing.T, s *testServer) {
			_, private := newKey(t)
			writeFile(t, s.opts.IdentityFiles[0], string(private))
		}, "SSH handshake failed"},
		{"no identity file", func(t *testing.T, s *testServer) { os.Remove(s.opts.IdentityFiles[0]) }, "no usable identity"},
		{"passphrase-protected identity", encrypted, "no usable identity"},
		{"nothing listens", func(t *testing.T, s *testServer) { s.opts.Server = freeAddr(t) }, "TCP connection failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := startServer(t)
			tt.change(t, s)
			c, err := Dial(context.Background(), s.opts)
			if err == nil {
				c.Close()
				t.Fatal("Dial succeeded")
			}
			if err.Error() != tt.want {
				t.Errorf("error = %q, want %q", err, tt.want)
			}
			// Errors are logged, so they never carry addresses, paths, or
			// server text.
			for _, secret := range []string{"SECRET", s.opts.User, "127.0.0.1", s.opts.IdentityFiles[0]} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q contains %q", err, secret)
				}
			}
		})
	}
}

func TestDialHandshakeTimeout(t *testing.T) {
	s := startServer(t)
	silent, err := net.Listen("tcp", "127.0.0.1:0") // accepts into its backlog, never answers
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	s.opts.Server = silent.Addr().String()
	writeFile(t, s.opts.KnownHostsFiles[0], knownhosts.Line([]string{s.opts.Server}, s.hostKey.PublicKey())+"\n")
	s.opts.ConnectTimeout = 100 * time.Millisecond

	start := time.Now()
	if _, err := Dial(context.Background(), s.opts); err == nil || err.Error() != "SSH handshake failed" {
		t.Fatalf("Dial error = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("Dial took %v with a 100ms timeout", time.Since(start))
	}
}

func TestDialCancelDuringHandshake(t *testing.T) {
	s := startServer(t)
	silent, _ := net.Listen("tcp", "127.0.0.1:0")
	defer silent.Close()
	s.opts.Server = silent.Addr().String()
	writeFile(t, s.opts.KnownHostsFiles[0], knownhosts.Line([]string{s.opts.Server}, s.hostKey.PublicKey())+"\n")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := Dial(ctx, s.opts)
		result <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := await(t, result); err == nil {
		t.Fatal("Dial succeeded")
	}
}

func TestDoneAfterServerCloses(t *testing.T) {
	s := startServer(t)
	c := dialServer(t, s)
	s.cancel()
	await(t, c.Done())
	if c.Err() != errClosed {
		t.Fatalf("Err = %v, want %v", c.Err(), errClosed)
	}
	if _, err := c.Start("cat"); err == nil {
		t.Fatal("Start on an ended connection succeeded")
	}
}

func TestStart(t *testing.T) {
	s := startServer(t)
	c := dialServer(t, s)
	session, err := c.Start("cat")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, text := range []string{"hello\n", "again\n"} {
		if _, err := io.WriteString(session, text); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(text))
		if _, err := io.ReadFull(session, got); err != nil || string(got) != text {
			t.Fatalf("round trip = %q, %v; want %q", got, err, text)
		}
	}
	// Closing stdin ends cat, and its output ends.
	session.Writer.(io.Closer).Close()
	if rest, err := io.ReadAll(session); err != nil || len(rest) != 0 {
		t.Fatalf("after stdin closed: %q, %v", rest, err)
	}
	if got := session.Failure(); got != "" {
		t.Errorf("Failure = %q for a command that wrote no errors", got)
	}
}

func TestSessionFailure(t *testing.T) {
	s := startServer(t)
	c := dialServer(t, s)
	session, err := c.Start("fail")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if out, err := io.ReadAll(session); err != nil || len(out) != 0 {
		t.Fatalf("stdout = %q, %v", out, err)
	}
	// The command's stderr is copied in the background, so it may land just
	// after stdout ends.
	deadline := time.Now().Add(5 * time.Second)
	for session.Failure() == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := session.Failure(); got != "error: first line" {
		t.Fatalf("Failure = %q, want the first stderr line", got)
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := limitedBuffer{max: 8}
	for _, p := range []string{"  abc", "defghij", "klm"} {
		if n, err := b.Write([]byte(p)); n != len(p) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", p, n, err)
		}
	}
	if got := b.String(); got != "abcdef" { // "  abcdef" trimmed
		t.Errorf("String = %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"":            `''`,
		"gst":         `'gst'`,
		"/opt/my gst": `'/opt/my gst'`,
		"it's":        `'it'\''s'`,
		"$(rm -rf /)": `'$(rm -rf /)'`,
		"a;b\nc":      "'a;b\nc'",
	} {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// encryptedPEM is a passphrase-protected key; encrypting is slow on purpose,
// so it is done once.
var encryptedPEM = sync.OnceValue(func() string {
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(private, "", []byte("secret"))
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(block))
})

func encryptedKey(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "encrypted")
	writeFile(t, path, encryptedPEM())
	return path
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name    string
		change  func(t *testing.T, s *testServer)
		problem string // expected in the problem; empty when the check passes
		hint    string
	}{
		{name: "usable"},
		{
			name: "skips unusable identities",
			change: func(t *testing.T, s *testServer) {
				s.opts.IdentityFiles = append([]string{encryptedKey(t), "/missing/key"}, s.opts.IdentityFiles...)
			},
		},
		{
			name:    "missing identity",
			change:  func(t *testing.T, s *testServer) { s.opts.IdentityFiles = []string{"/missing/key"} },
			problem: "/missing/key (missing)", hint: "ssh-keygen",
		},
		{
			name:    "several missing identities",
			change:  func(t *testing.T, s *testServer) { s.opts.IdentityFiles = []string{"/missing/a", "/missing/b"} },
			problem: "none of the identity files exist: /missing/a, /missing/b", hint: "ssh-keygen",
		},
		{
			name:    "only an encrypted identity",
			change:  func(t *testing.T, s *testServer) { s.opts.IdentityFiles = []string{encryptedKey(t)} },
			problem: "passphrase-protected", hint: "ssh-copy-id",
		},
		{
			name:    "missing known_hosts",
			change:  func(t *testing.T, s *testServer) { os.Remove(s.opts.KnownHostsFiles[0]) },
			problem: "none of the known_hosts files exist", hint: "ssh -p ",
		},
		{
			name:    "malformed known_hosts",
			change:  func(t *testing.T, s *testServer) { writeFile(t, s.opts.KnownHostsFiles[0], "malformed") },
			problem: "known_hosts", hint: "ssh -p ",
		},
		{
			name:    "server not in known_hosts",
			change:  func(t *testing.T, s *testServer) { writeFile(t, s.opts.KnownHostsFiles[0], "") },
			problem: "no host key for " + "127.0.0.1:", hint: "ssh -p ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := startServer(t)
			if tt.change != nil {
				tt.change(t, s)
			}
			err := Check(s.opts)
			if tt.problem == "" {
				if err != nil {
					t.Fatalf("Check = %v", err)
				}
			} else {
				var ce *CheckError
				if !errors.As(err, &ce) {
					t.Fatalf("Check = %v, want a *CheckError", err)
				}
				if !strings.Contains(ce.Problem, tt.problem) || !strings.Contains(ce.Hint, tt.hint) {
					t.Errorf("Check = %+v, want problem %q and hint %q", ce, tt.problem, tt.hint)
				}
				if ce.Error() != ce.Problem {
					t.Errorf("Error() = %q, want the problem %q", ce.Error(), ce.Problem)
				}
			}
			select {
			case <-s.probes:
				t.Error("Check contacted the server")
			default:
			}
		})
	}
}

func TestCheckHintFollowsServerPort(t *testing.T) {
	s := startServer(t)
	s.opts.IdentityFiles = []string{"/missing/key"}
	s.opts.Server = "ssh.example.com:22"
	s.opts.User = "me"
	var ce *CheckError
	if err := Check(s.opts); !errors.As(err, &ce) || !strings.Contains(ce.Hint, "ssh-copy-id -i ~/.ssh/gst_ed25519 me@ssh.example.com`") {
		t.Fatalf("Check = %v, want a hint that logs in as me@ssh.example.com", err)
	}
	s.opts.Server = "ssh.example.com:2200"
	if err := Check(s.opts); !errors.As(err, &ce) || !strings.Contains(ce.Hint, "-p 2200 me@ssh.example.com") {
		t.Fatalf("Check = %v, want a hint with the port", err)
	}
}
