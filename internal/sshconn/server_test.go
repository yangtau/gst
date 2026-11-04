package sshconn

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// testServer is an in-process SSH server on a loopback port. Its opts
// connect to it with a key it accepts and a known_hosts file that
// records its host key.
type testServer struct {
	opts          Options
	hostKey       ssh.Signer
	authKey       ssh.PublicKey
	hangKeepalive bool // never answer keepalive requests
	probes        chan struct{}
	ctx           context.Context
	cancel        context.CancelFunc // drops every client connection
	wg            sync.WaitGroup
}

// commands are what the server runs for exec requests: it echoes the
// stdin of "cat", writes to stderr and fails for "fail", and prints
// "gst 0.3.0" for "gst version". Anything else is not found.
func runCommand(command string, stdin io.Reader, stdout, stderr io.Writer) uint32 {
	switch command {
	case "cat":
		io.Copy(stdout, stdin)
	case "fail":
		io.WriteString(stderr, "error: first line\nsecond line\n")
		return 1
	case "gst version":
		io.WriteString(stdout, "gst 0.3.0\nbuild details\n")
	default:
		io.WriteString(stderr, "sh: "+command+": command not found\n")
		return 127
	}
	return 0
}

func startServer(t *testing.T) *testServer {
	t.Helper()
	hostKey, _ := newKey(t)
	authKey, privateKey := newKey(t)
	// Real servers have more host keys than known_hosts records, and
	// x/crypto/ssh prefers ECDSA over the recorded ED25519 key.
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaHostKey, err := ssh.NewSignerFromKey(ecdsaKey)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	s := &testServer{
		hostKey: hostKey,
		authKey: authKey.PublicKey(),
		probes:  make(chan struct{}, 1000),
		ctx:     ctx,
		cancel:  cancel,
		opts: Options{
			Server:            listener.Addr().String(),
			User:              "remote-user",
			IdentityFiles:     []string{filepath.Join(dir, "key")},
			KnownHostsFiles:   []string{filepath.Join(dir, "known_hosts")},
			ConnectTimeout:    2 * time.Second,
			KeepaliveInterval: time.Second,
			KeepaliveTimeout:  time.Second,
		},
	}
	writeFile(t, s.opts.IdentityFiles[0], string(privateKey))
	writeFile(t, s.opts.KnownHostsFiles[0], knownhosts.Line([]string{s.opts.Server}, hostKey.PublicKey())+"\n")

	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), s.authKey.Marshal()) {
			return nil, errors.New("unauthorized")
		}
		return nil, nil
	}}
	config.AddHostKey(hostKey)
	config.AddHostKey(ecdsaHostKey)
	s.wg.Go(func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			s.wg.Go(func() { s.serveConn(raw, config) })
		}
	})
	t.Cleanup(func() {
		cancel()
		listener.Close()
		s.wg.Wait()
	})
	return s
}

func (s *testServer) serveConn(raw net.Conn, config *ssh.ServerConfig) {
	defer raw.Close()
	defer closeOnDone(s.ctx, raw)()
	conn, channels, requests, err := ssh.NewServerConn(raw, config)
	if err != nil {
		return
	}
	defer conn.Close()
	s.wg.Go(func() {
		for ch := range channels {
			if ch.ChannelType() != "session" {
				ch.Reject(ssh.UnknownChannelType, "unsupported")
				continue
			}
			channel, reqs, err := ch.Accept()
			if err != nil {
				continue
			}
			s.wg.Go(func() { serveSession(channel, reqs) })
		}
	})
	for req := range requests {
		if req.Type == "keepalive@openssh.com" {
			s.probes <- struct{}{}
			if s.hangKeepalive {
				continue
			}
		}
		req.Reply(false, nil) // a refusal still proves the server is alive
	}
}

func serveSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for req := range requests {
		var exec struct{ Command string }
		if req.Type != "exec" || ssh.Unmarshal(req.Payload, &exec) != nil {
			req.Reply(false, nil)
			continue
		}
		req.Reply(true, nil)
		status := runCommand(exec.Command, channel, channel, channel.Stderr())
		channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}

func newKey(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "")
	if err != nil {
		t.Fatal(err)
	}
	return signer, pem.EncodeToMemory(block)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
		panic("unreachable")
	}
}

// freeAddr returns a loopback address that nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}
