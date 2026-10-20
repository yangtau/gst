package tunnel

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
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

// serverOptions selects how the test SSH server misbehaves.
type serverOptions struct {
	rejectForward int  // reject the n-th tcpip-forward request (1-based)
	hangForward   bool // never answer tcpip-forward requests
	hangKeepalive bool // never answer keepalive requests
	cleanupStatus uint32
	hangCleanup   bool
}

type cleanupCall struct{ command, script string }

// testServer is an in-process SSH server that accepts one client connection.
// Remote forwards are served on ephemeral local ports, published on forwards.
type testServer struct {
	options      serverOptions
	opts         Options
	hostKey      ssh.Signer
	ecdsaHostKey ssh.Signer // not recorded in known_hosts
	authKey      ssh.PublicKey
	forwards     chan net.Listener
	probes       chan struct{}
	cleanups     chan cleanupCall
	ctx          context.Context
	cancel       context.CancelFunc // drops the client connection
	wg           sync.WaitGroup
}

func startServer(t *testing.T, options serverOptions) *testServer {
	t.Helper()
	hostKey, _ := newKey(t)
	authKey, privateKey := newKey(t)
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
		options:      options,
		hostKey:      hostKey,
		ecdsaHostKey: ecdsaHostKey,
		authKey:      authKey.PublicKey(),
		forwards:     make(chan net.Listener, 10),
		probes:       make(chan struct{}, 100),
		cleanups:     make(chan cleanupCall, 1),
		ctx:          ctx,
		cancel:       cancel,
		opts: Options{
			Server:            listener.Addr().String(),
			User:              "remote-user",
			IdentityFiles:     []string{filepath.Join(dir, "key")},
			KnownHostsFiles:   []string{filepath.Join(dir, "known_hosts")},
			ConnectTimeout:    2 * time.Second,
			KeepaliveInterval: time.Second,
			KeepaliveTimeout:  time.Second,
			Forwards:          []config.Forward{{Direction: config.Remote, Listen: "127.0.0.1:2222", Target: "127.0.0.1:1"}},
		},
	}
	writeFile(t, s.opts.IdentityFiles[0], string(privateKey))
	writeFile(t, s.opts.KnownHostsFiles[0], knownhosts.Line([]string{s.opts.Server}, hostKey.PublicKey())+"\n")

	s.wg.Go(func() {
		raw, err := listener.Accept()
		if err != nil {
			return
		}
		s.serveConn(raw)
	})
	t.Cleanup(func() {
		cancel()
		listener.Close()
		s.wg.Wait()
	})
	return s
}

func (s *testServer) serveConn(raw net.Conn) {
	defer raw.Close()
	defer closeOnDone(s.ctx, raw)()

	serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), s.authKey.Marshal()) {
			return nil, errors.New("unauthorized")
		}
		return nil, nil
	}}
	serverConfig.AddHostKey(s.hostKey)
	// Real servers usually have more host keys than known_hosts records, and
	// x/crypto/ssh prefers ECDSA over the recorded ED25519 key.
	serverConfig.AddHostKey(s.ecdsaHostKey)
	conn, channels, requests, err := ssh.NewServerConn(raw, serverConfig)
	if err != nil {
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	s.wg.Go(func() { s.serveChannels(ctx, channels) })
	s.serveRequests(ctx, conn, requests)
}

func (s *testServer) serveChannels(ctx context.Context, channels <-chan ssh.NewChannel) {
	for ch := range channels {
		switch ch.ChannelType() {
		case "session":
			channel, requests, err := ch.Accept()
			if err != nil {
				continue
			}
			s.wg.Go(func() { s.serveSession(ctx, channel, requests) })
		case "direct-tcpip":
			s.wg.Go(func() { s.serveDirect(ctx, ch) })
		default:
			ch.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}

// serveDirect dials the target of a local forward from the server side.
func (s *testServer) serveDirect(ctx context.Context, ch ssh.NewChannel) {
	var target struct {
		Host       string
		Port       uint32
		Origin     string
		OriginPort uint32
	}
	if ssh.Unmarshal(ch.ExtraData(), &target) != nil {
		ch.Reject(ssh.Prohibited, "malformed")
		return
	}
	outgoing, err := net.Dial("tcp", net.JoinHostPort(target.Host, strconv.Itoa(int(target.Port))))
	if err != nil {
		// Deliberately sensitive message that must never reach local logs.
		ch.Reject(ssh.ConnectionFailed, "SECRET "+err.Error())
		return
	}
	defer outgoing.Close()
	defer closeOnDone(ctx, outgoing)()
	channel, requests, err := ch.Accept()
	if err != nil {
		return
	}
	defer channel.Close()
	s.wg.Go(func() { ssh.DiscardRequests(requests) })
	relay(channel, outgoing)
}

func (s *testServer) serveSession(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for req := range requests {
		var exec struct{ Command string }
		if req.Type != "exec" || ssh.Unmarshal(req.Payload, &exec) != nil {
			req.Reply(false, nil)
			continue
		}
		req.Reply(true, nil)
		script, _ := io.ReadAll(channel)
		s.cleanups <- cleanupCall{exec.Command, string(script)}
		if s.options.hangCleanup {
			<-ctx.Done()
			return
		}
		// Deliberately sensitive output must never reach local logs.
		io.WriteString(channel, "SECRET remote address user key config\n")
		io.WriteString(channel.Stderr(), "SECRET remote stderr\n")
		channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{s.options.cleanupStatus}))
		return
	}
}

func (s *testServer) serveRequests(ctx context.Context, conn *ssh.ServerConn, requests <-chan *ssh.Request) {
	forwardCount := 0
	for req := range requests {
		switch req.Type {
		case "tcpip-forward":
			forwardCount++
			switch {
			case s.options.hangForward:
			case forwardCount == s.options.rejectForward:
				req.Reply(false, nil)
			default:
				s.startForward(ctx, conn, req)
			}
		case "keepalive@openssh.com":
			s.probes <- struct{}{}
			if !s.options.hangKeepalive {
				req.Reply(false, nil)
			}
		default:
			req.Reply(false, nil)
		}
	}
}

// startForward serves a tcpip-forward request on an ephemeral local port but
// reports the requested address in forwarded channels, so tests need neither
// fixed ports nor external hosts.
func (s *testServer) startForward(ctx context.Context, conn *ssh.ServerConn, req *ssh.Request) {
	var requested struct {
		Host string
		Port uint32
	}
	if ssh.Unmarshal(req.Payload, &requested) != nil {
		req.Reply(false, nil)
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		req.Reply(false, nil)
		return
	}
	closeOnDone(ctx, ln)
	req.Reply(true, nil)
	s.forwards <- ln

	s.wg.Go(func() {
		defer ln.Close()
		for {
			incoming, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Go(func() {
				defer incoming.Close()
				defer closeOnDone(ctx, incoming)()
				origin := incoming.RemoteAddr().(*net.TCPAddr)
				payload := struct {
					Host       string
					Port       uint32
					Origin     string
					OriginPort uint32
				}{requested.Host, requested.Port, origin.IP.String(), uint32(origin.Port)}
				channel, requests, err := conn.OpenChannel("forwarded-tcpip", ssh.Marshal(payload))
				if err != nil {
					return
				}
				defer channel.Close()
				s.wg.Go(func() { ssh.DiscardRequests(requests) })
				relay(channel, incoming)
			})
		}
	})
}

// connect opens a client to the test server without starting a tunnel.
func (s *testServer) connect(t *testing.T, ctx context.Context) *ssh.Client {
	t.Helper()
	clientConfig, err := newClientConfig(s.opts)
	if err != nil {
		t.Fatal(err)
	}
	client, err := dial(ctx, s.opts.Server, clientConfig, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
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

// capturedLog records the standard logger and signals once the tunnel is ready.
type capturedLog struct {
	mu    sync.Mutex
	data  bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func captureLog(t *testing.T) *capturedLog {
	t.Helper()
	c := &capturedLog{ready: make(chan struct{})}
	previous := log.Writer()
	log.SetOutput(c)
	t.Cleanup(func() { log.SetOutput(previous) })
	return c
}

func (c *capturedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if bytes.Contains(p, []byte("tunnel ready")) {
		c.once.Do(func() { close(c.ready) })
	}
	return c.data.Write(p)
}

func (c *capturedLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.String()
}

// startTunnel runs the tunnel against s and waits for its first remote listener.
func startTunnel(t *testing.T, s *testServer, logs *capturedLog) (context.CancelFunc, <-chan error, net.Listener) {
	t.Helper()
	cancel, result := runAsync(t, s.opts)
	await(t, logs.ready)
	return cancel, result, await(t, s.forwards)
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

// startEchoTarget serves connections by reading until EOF, then answering
// "reply:" + request and half-closing.
func startEchoTarget(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				if data, err := io.ReadAll(c); err == nil {
					c.Write(append([]byte("reply:"), data...))
					c.(*net.TCPConn).CloseWrite()
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// roundTrip sends request to addr, half-closes, and returns the reply.
func roundTrip(addr, request string) (string, error) {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c, request); err != nil {
		return "", err
	}
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		return "", err
	}
	reply, err := io.ReadAll(c)
	return string(reply), err
}

// eventually polls cond for up to a second and fails with msg if it never holds.
func eventually(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(time.Millisecond)
	}
}

// runAsync starts Run in the background and returns its cancel function and
// a channel that receives its result.
func runAsync(t *testing.T, opts Options) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() { result <- Run(ctx, opts) }()
	return cancel, result
}
