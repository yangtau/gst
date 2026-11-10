package sshconn

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// A machine without ssh sets a link up with these alone: a key of its own,
// the server's host key shown to a person, and a login.
func TestTryAndTrust(t *testing.T) {
	s := startServer(t)
	if err := Try(t.Context(), s.opts); err != nil {
		t.Fatalf("a login that works: %v", err)
	}

	// A machine that has not seen the server learns the key that OpenSSH
	// shows first, though the server has an ECDSA key too.
	opts := s.opts
	opts.KnownHostsFiles = []string{filepath.Join(t.TempDir(), "known_hosts")}
	var unknown *HostKeyError
	if err := Try(t.Context(), opts); !errors.As(err, &unknown) || unknown.Changed {
		t.Fatalf("an unknown server: %v", err)
	}
	if got, want := ssh.FingerprintSHA256(unknown.Key), ssh.FingerprintSHA256(s.hostKey.PublicKey()); got != want {
		t.Fatalf("shown %s, want the ED25519 key %s", got, want)
	}
	if err := Trust(opts.KnownHostsFiles[0], opts.Server, unknown.Key); err != nil {
		t.Fatal(err)
	}
	if err := Try(t.Context(), opts); err != nil {
		t.Fatalf("after trusting the key: %v", err)
	}
	if _, err := Dial(t.Context(), opts); err != nil {
		t.Fatalf("a link after trusting the key: %v", err)
	}

	// Another key under the server's address is a changed key, and
	// trusting the new one replaces it and nothing else.
	other, _ := newKey(t)
	keep := knownhosts.Line([]string{"other.example.com"}, other.PublicKey())
	writeFile(t, opts.KnownHostsFiles[0], keep+"\n"+knownhosts.Line([]string{opts.Server}, other.PublicKey())+"\n")
	var changed *HostKeyError
	if err := Try(t.Context(), opts); !errors.As(err, &changed) || !changed.Changed {
		t.Fatalf("a changed key: %v", err)
	}
	if err := Trust(opts.KnownHostsFiles[0], opts.Server, s.hostKey.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if err := Try(t.Context(), opts); err != nil {
		t.Fatalf("after trusting the new key: %v", err)
	}
	if data, _ := os.ReadFile(opts.KnownHostsFiles[0]); !strings.Contains(string(data), keep) || strings.Count(string(data), "\n") != 2 {
		t.Fatalf("known_hosts is %q", data)
	}

	// A key that the server does not know.
	opts.IdentityFiles = []string{filepath.Join(t.TempDir(), "key")}
	if err := NewIdentity(opts.IdentityFiles[0]); err != nil {
		t.Fatal(err)
	}
	if err := Try(t.Context(), opts); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("an unauthorized key: %v", err)
	}

	opts.Server = freeAddr(t)
	if err := Try(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "cannot connect to "+opts.Server) {
		t.Fatalf("no server: %v", err)
	}
}

func TestNewIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := NewIdentity(path); err != nil {
		t.Fatal(err)
	}
	line, err := AuthorizedKey(path, "gst@phone")
	if err != nil || !strings.HasPrefix(line, "ssh-ed25519 ") || !strings.HasSuffix(line, " gst@phone") {
		t.Fatalf("AuthorizedKey = %q, %v", line, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("the key's mode is %v", info.Mode())
	}
	// It is usable as a link's identity, and stays the same.
	if ids := loadIdentities([]string{path}); ids[0].signer == nil {
		t.Fatalf("the key is %s", ids[0].problem)
	}
	if err := NewIdentity(path); err != nil {
		t.Fatal(err)
	}
	if again, _ := AuthorizedKey(path, "gst@phone"); again != line {
		t.Fatal("NewIdentity replaced a key")
	}
}
