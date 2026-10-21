package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
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

	"github.com/yangtau/ssh-tunnel/internal/config"
)

func TestReverseForwardingAndCancellation(t *testing.T) {
	logs := captureLog(t)
	s := startServer(t, serverOptions{})

	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	targetCtx, cancelTarget := context.WithCancel(context.Background())
	var targets sync.WaitGroup
	defer func() { cancelTarget(); local.Close(); targets.Wait() }()
	accepted := make(chan net.Conn, 20)
	targets.Go(func() {
		for {
			c, err := local.Accept()
			if err != nil {
				return
			}
			targets.Go(func() {
				defer c.Close()
				defer closeOnDone(targetCtx, c)()
				accepted <- c
				data, err := io.ReadAll(c)
				if err == nil {
					c.Write(append([]byte("reply:"), data...))
					c.(*net.TCPConn).CloseWrite()
				}
			})
		}
	})

	s.opts.Forwards = []config.Forward{
		{Direction: config.Remote, Listen: "127.0.0.1:2222", Target: local.Addr().String()},
		{Direction: config.Remote, Listen: "127.0.0.1:3000", Target: local.Addr().String()},
	}
	cancel, result := runAsync(t, s.opts)
	await(t, logs.ready)
	remote := []net.Listener{await(t, s.forwards), await(t, s.forwards)}

	var clients sync.WaitGroup
	for i := range 8 {
		clients.Go(func() {
			c, err := net.DialTimeout("tcp", remote[i%2].Addr().String(), time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(3 * time.Second))
			message := strings.Repeat(fmt.Sprintf("payload-%d/", i), 20000)
			if _, err := io.WriteString(c, message); err != nil {
				t.Error(err)
				return
			}
			if err := c.(*net.TCPConn).CloseWrite(); err != nil {
				t.Error(err)
				return
			}
			response, err := io.ReadAll(c)
			if err != nil || string(response) != "reply:"+message {
				t.Error("half-close response failed", err)
			}
		})
	}
	clients.Wait()
	for range 8 {
		await(t, accepted)
	}

	active, err := net.DialTimeout("tcp", remote[0].Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	target := await(t, accepted)

	cancel()
	if err := await(t, result); err != nil {
		t.Fatal(err)
	}

	active.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := active.Read(make([]byte, 1)); err == nil {
		t.Fatal("incoming connection not closed")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("incoming close timed out")
	}
	// The target goroutine must see EOF and close its accepted socket too.
	eventually(t, "local target not torn down", func() bool {
		_, err := target.Write([]byte("x"))
		return err != nil
	})
}

func TestLocalForwarding(t *testing.T) {
	logs := captureLog(t)
	s := startServer(t, serverOptions{})
	listen := []string{freeAddr(t), freeAddr(t)}
	s.opts.Forwards = []config.Forward{
		{Direction: config.Local, Listen: listen[0], Target: startEchoTarget(t)},
		{Direction: config.Local, Listen: listen[1], Target: freeAddr(t)},
	}
	cancel, result := runAsync(t, s.opts)
	await(t, logs.ready)

	var clients sync.WaitGroup
	for i := range 8 {
		clients.Go(func() {
			message := strings.Repeat(fmt.Sprintf("payload-%d/", i), 20000)
			if reply, err := roundTrip(listen[0], message); err != nil || reply != "reply:"+message {
				t.Error("half-close response failed", err)
			}
		})
	}
	clients.Wait()

	if reply, _ := roundTrip(listen[1], "refused"); reply != "" {
		t.Fatal("refused target produced a reply")
	}
	eventually(t, "missing target failure log", func() bool {
		return strings.Contains(logs.String(), "target connection failed for forward 2")
	})
	if strings.Contains(logs.String(), "SECRET") {
		t.Fatal("remote rejection leaked")
	}

	cancel()
	if err := await(t, result); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", listen[0], 100*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("local listener survived shutdown")
	}
}

func TestUnixSocketForwarding(t *testing.T) {
	logs := captureLog(t)
	s := startServer(t, serverOptions{})
	dir := shortTempDir(t)
	localSocket := filepath.Join(dir, "local.sock")
	localTCP := freeAddr(t)
	stale := filepath.Join(dir, "stale.sock")
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()

	s.opts.Forwards = []config.Forward{
		{Direction: config.Local, Listen: localSocket, Target: startEchoTarget(t)},
		{Direction: config.Local, Listen: localTCP, Target: startUnixEchoTarget(t)},
		{Direction: config.Local, Listen: stale, Target: startUnixEchoTarget(t)},
		{Direction: config.Remote, Listen: "/srv/remote.sock", Target: startUnixEchoTarget(t)},
	}
	cancel, result := runAsync(t, s.opts)
	await(t, logs.ready)
	remote := await(t, s.forwards)

	for _, addr := range []string{localSocket, localTCP, stale, remote.Addr().String()} {
		message := strings.Repeat("payload/", 20000)
		if reply, err := roundTrip(addr, message); err != nil || reply != "reply:"+message {
			t.Errorf("relay through %s failed: %v", addr, err)
		}
	}
	if info, err := os.Stat(localSocket); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("local socket mode %v: %v", info.Mode(), err)
	}

	cancel()
	if err := await(t, result); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(localSocket); !os.IsNotExist(err) {
		t.Fatal("local socket survived shutdown")
	}
	if strings.Contains(logs.String(), dir) {
		t.Fatal("runtime log leaked a socket path")
	}
}

func TestLocalSocketInUse(t *testing.T) {
	dir := shortTempDir(t)
	live := filepath.Join(dir, "live.sock")
	ln, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	regular := filepath.Join(dir, "file")
	writeFile(t, regular, "keep")

	for _, path := range []string{live, regular} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			s := startServer(t, serverOptions{})
			s.opts.Forwards = []config.Forward{{Direction: config.Local, Listen: path, Target: "127.0.0.1:1"}}
			_, result := runAsync(t, s.opts)
			if err := await(t, result); err == nil || err.Error() != "listener start failed for forward 1" {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("existing file removed")
			}
		})
	}
	if c, err := net.Dial("unix", live); err != nil {
		t.Fatal("live socket broken")
	} else {
		c.Close()
	}
}

func TestListenerFailureClosesEarlierListeners(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	for _, second := range []config.Forward{
		{Direction: config.Remote, Listen: "127.0.0.1:3000", Target: "127.0.0.1:1"},
		{Direction: config.Local, Listen: busy.Addr().String(), Target: "127.0.0.1:1"},
	} {
		t.Run(second.Direction, func(t *testing.T) {
			s := startServer(t, serverOptions{rejectForward: 2})
			s.opts.Forwards = append(s.opts.Forwards, second)
			_, result := runAsync(t, s.opts)
			ln := await(t, s.forwards)
			if err := await(t, result); err == nil || err.Error() != "listener start failed for forward 2" {
				t.Fatalf("unexpected error: %v", err)
			}
			eventually(t, "listener survived failed startup", func() bool {
				c, err := net.DialTimeout("tcp", ln.Addr().String(), 100*time.Millisecond)
				if err != nil {
					return true
				}
				c.Close()
				return false
			})
		})
	}
}

func TestHostKeys(t *testing.T) {
	for _, mode := range []string{"valid", "unknown", "changed", "revoked", "malformed", "missing"} {
		t.Run(mode, func(t *testing.T) {
			s := startServer(t, serverOptions{})
			content := knownhosts.Line([]string{s.opts.Server}, s.hostKey.PublicKey()) + "\n"
			switch mode {
			case "unknown":
				content = ""
			case "changed":
				key, _ := newKey(t)
				content = knownhosts.Line([]string{s.opts.Server}, key.PublicKey()) + "\n"
			case "revoked":
				content = "@revoked " + content
			case "malformed":
				content = "SECRET invalid known_hosts"
			}
			if mode == "missing" {
				os.Remove(s.opts.KnownHostsFiles[0])
			} else {
				writeFile(t, s.opts.KnownHostsFiles[0], content)
			}

			clientConfig, err := newClientConfig(s.opts)
			if err == nil {
				var client *ssh.Client
				client, err = dial(context.Background(), s.opts.Server, clientConfig, time.Second)
				if client != nil {
					client.Close()
				}
			}
			if (mode == "valid") != (err == nil) {
				t.Fatalf("unexpected result: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "SECRET") {
				t.Fatal("sensitive error")
			}
		})
	}
}

func TestHandshakeTimeoutAndCancellation(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelEarly), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			accepted := make(chan net.Conn, 1)
			go func() {
				if c, err := listener.Accept(); err == nil {
					accepted <- c
				}
			}()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timeout := 150 * time.Millisecond
			if cancelEarly {
				timeout = 10 * time.Second
			}
			clientConfig := &ssh.ClientConfig{User: "test", HostKeyCallback: ssh.InsecureIgnoreHostKey()}
			result := make(chan error, 1)
			go func() {
				_, err := dial(ctx, listener.Addr().String(), clientConfig, timeout)
				result <- err
			}()

			raw := await(t, accepted)
			defer raw.Close()
			if cancelEarly {
				cancel()
			}
			start := time.Now()
			if err := await(t, result); err == nil {
				t.Fatal("handshake unexpectedly succeeded")
			}
			if time.Since(start) > time.Second {
				t.Fatal("slow handshake termination")
			}
			raw.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := io.ReadAll(raw); err != nil {
				t.Fatal("TCP connection was not closed", err)
			}
		})
	}
}

func TestKeepalive(t *testing.T) {
	for _, hang := range []bool{false, true} {
		t.Run(fmt.Sprint(hang), func(t *testing.T) {
			s := startServer(t, serverOptions{hangKeepalive: hang})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := s.connect(t, ctx)
			result := make(chan error, 1)
			go func() { result <- keepalive(ctx, client, 20*time.Millisecond, 80*time.Millisecond) }()
			await(t, s.probes)

			if hang {
				if err := await(t, result); err == nil || err.Error() != "SSH keepalive timed out" {
					t.Fatalf("unexpected result %v", err)
				}
				return
			}
			await(t, s.probes)
			await(t, s.probes)
			select {
			case err := <-result:
				t.Fatalf("negative reply treated as failure: %v", err)
			default:
			}
			cancel()
			if err := await(t, result); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestKeepaliveCancellationWithPendingReply(t *testing.T) {
	s := startServer(t, serverOptions{hangKeepalive: true})
	client := s.connect(t, context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- keepalive(ctx, client, 20*time.Millisecond, time.Hour) }()
	await(t, s.probes)
	cancel()
	if err := await(t, result); err != nil {
		t.Fatal(err)
	}
}

func TestRunReportsKeepaliveTimeout(t *testing.T) {
	s := startServer(t, serverOptions{hangKeepalive: true})
	_, result := runAsync(t, s.opts)
	// Closing the connection after the timeout must not be reported as the
	// cause instead.
	if err := await(t, result); err == nil || err.Error() != "SSH keepalive timed out" {
		t.Fatalf("unexpected result: %v", err)
	}
}

func TestStalledForwardRequest(t *testing.T) {
	s := startServer(t, serverOptions{hangForward: true})
	_, result := runAsync(t, s.opts)
	if err := await(t, result); err == nil {
		t.Fatal("stalled global request did not fail")
	}
}

func TestCleanup(t *testing.T) {
	for _, status := range []uint32{0, 1} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			logs := captureLog(t)
			s := startServer(t, serverOptions{cleanupStatus: status})
			s.opts.CleanupListeners = true
			s.opts.Forwards = append(s.opts.Forwards,
				config.Forward{Direction: config.Remote, Listen: "127.0.0.1:3000", Target: "localhost:1"},
				config.Forward{Direction: config.Local, Listen: freeAddr(t), Target: "localhost:1"},
				config.Forward{Direction: config.Remote, Listen: "/tmp/it's here.sock", Target: "localhost:1"},
			)
			cancel, result := runAsync(t, s.opts)

			call := await(t, s.cleanups)
			if call.command != `bash -s -- 2222 3000 '/tmp/it'\''s here.sock'` {
				t.Fatalf("unexpected command %q", call.command)
			}
			if !strings.Contains(call.script, "login_uid=$(id -u)") || strings.Contains(call.script, s.opts.User) {
				t.Fatal("cleanup does not use remote identity")
			}
			if status == 0 {
				await(t, logs.ready)
				cancel()
			}
			err := await(t, result)
			if status != 0 && (err == nil || err.Error() != "remote listener cleanup failed") {
				t.Fatalf("unexpected result: %v", err)
			}
			if strings.Contains(logs.String(), "SECRET") {
				t.Fatal("remote output leaked")
			}
		})
	}

	t.Run("timeout", func(t *testing.T) {
		s := startServer(t, serverOptions{hangCleanup: true})
		client := s.connect(t, context.Background())
		result := make(chan error, 1)
		go func() { result <- cleanupListeners(client, s.opts.Forwards, 100*time.Millisecond) }()
		if err := await(t, result); err == nil {
			t.Fatal("stalled cleanup succeeded")
		}
	})

	t.Run("local only", func(t *testing.T) {
		s := startServer(t, serverOptions{cleanupStatus: 1})
		client := s.connect(t, context.Background())
		forwards := []config.Forward{{Direction: config.Local, Listen: "127.0.0.1:2222", Target: "localhost:1"}}
		if err := cleanupListeners(client, forwards, time.Second); err != nil {
			t.Fatalf("cleanup ran without remote forwards: %v", err)
		}
	})
}

func TestShutdownDuringIncomingConnections(t *testing.T) {
	for _, remoteClose := range []bool{false, true} {
		t.Run(fmt.Sprint(remoteClose), func(t *testing.T) {
			logs := captureLog(t)
			s := startServer(t, serverOptions{})
			cancel, result, ln := startTunnel(t, s, logs)

			var clients sync.WaitGroup
			started := make(chan struct{}, 200)
			for range 200 {
				clients.Go(func() {
					c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
					started <- struct{}{}
					if err != nil {
						return
					}
					defer c.Close()
					c.SetReadDeadline(time.Now().Add(time.Second))
					io.Copy(io.Discard, c)
				})
			}
			for range 40 {
				await(t, started)
			}
			if remoteClose {
				s.cancel()
			} else {
				cancel()
			}
			await(t, result)
			clients.Wait()
		})
	}
}

func TestTargetHalfClose(t *testing.T) {
	logs := captureLog(t)
	s := startServer(t, serverOptions{})
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	received := make(chan string, 1)
	go func() {
		c, err := target.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		c.Write([]byte("greeting"))
		c.(*net.TCPConn).CloseWrite()
		data, _ := io.ReadAll(c)
		received <- string(data)
	}()

	s.opts.Forwards[0].Target = target.Addr().String()
	cancel, result, ln := startTunnel(t, s, logs)

	c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if greeting, err := io.ReadAll(c); err != nil || string(greeting) != "greeting" {
		t.Fatalf("missing half-close greeting: %v", err)
	}
	if _, err := c.Write([]byte("response after EOF")); err != nil {
		t.Fatal(err)
	}
	c.(*net.TCPConn).CloseWrite()
	if got := await(t, received); got != "response after EOF" {
		t.Fatalf("lost reverse-direction data: %q", got)
	}
	cancel()
	await(t, result)
}

func TestRuntimeLogPrivacy(t *testing.T) {
	logs := captureLog(t)
	s := startServer(t, serverOptions{})
	// Reserve then close a socket to obtain a refused target without assuming
	// that any particular service port is unused on the test machine.
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.opts.Forwards[0].Target = target.Addr().String()
	target.Close()

	cancel, result, ln := startTunnel(t, s, logs)
	c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadAll(c); err != nil {
		t.Fatal(err)
	}
	cancel()
	await(t, result)

	text := logs.String()
	if !strings.Contains(text, "target connection failed for forward 1") {
		t.Fatal("missing stage error")
	}
	fwd := s.opts.Forwards[0]
	for _, secret := range []string{s.opts.Server, s.opts.User, s.opts.IdentityFiles[0], s.opts.KnownHostsFiles[0], fwd.Listen, fwd.Target, "127.0.0.1"} {
		if strings.Contains(text, secret) {
			t.Fatal("runtime log leaked config")
		}
	}
}

func TestDiagnose(t *testing.T) {
	encrypted := func(t *testing.T, dir string) string {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		block, err := ssh.MarshalPrivateKeyWithPassphrase(private, "", []byte("secret"))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "encrypted")
		writeFile(t, path, string(pem.EncodeToMemory(block)))
		return path
	}
	tests := map[string]struct {
		change   func(t *testing.T, s *testServer)
		failStep string // empty when every step passes
	}{
		"ok": {},
		"skips unusable identities": {change: func(t *testing.T, s *testServer) {
			s.opts.IdentityFiles = append([]string{encrypted(t, t.TempDir()), "/missing/key"}, s.opts.IdentityFiles...)
		}},
		"only encrypted identity": {change: func(t *testing.T, s *testServer) {
			s.opts.IdentityFiles = []string{encrypted(t, t.TempDir())}
		}, failStep: "identity"},
		"unknown host": {change: func(t *testing.T, s *testServer) {
			writeFile(t, s.opts.KnownHostsFiles[0], "")
		}, failStep: "known_hosts"},
		"changed host key": {change: func(t *testing.T, s *testServer) {
			key, _ := newKey(t)
			writeFile(t, s.opts.KnownHostsFiles[0], knownhosts.Line([]string{s.opts.Server}, key.PublicKey())+"\n")
		}, failStep: "login"},
		"unauthorized identity": {change: func(t *testing.T, s *testServer) {
			_, private := newKey(t)
			writeFile(t, s.opts.IdentityFiles[0], string(private))
		}, failStep: "login"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			s := startServer(t, serverOptions{})
			if test.change != nil {
				test.change(t, s)
			}
			steps := Diagnose(context.Background(), s.opts, true)
			last := steps[len(steps)-1]
			if test.failStep == "" {
				if len(steps) != 4 || !last.OK {
					t.Fatalf("unexpected steps %+v", steps)
				}
				return
			}
			if last.Name != test.failStep || last.OK || last.Hint == "" {
				t.Fatalf("unexpected steps %+v", steps)
			}
		})
	}
}
