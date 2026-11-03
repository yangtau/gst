package mesh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Tunnel is one of this machine's tunnels with mesh on. The daemon that
// runs it reports its state and its SSH connection; the node announces it
// as an edge of this node's record, keeps a peer link with the gst on its
// server while it is connected, and relays over it to a server without
// gst.
type Tunnel struct {
	n           *Node
	name        string
	destination string
	remoteGst   string      // the server's gst command; "gst" when empty
	log         *log.Logger // the tunnel's log

	mu      sync.Mutex
	state   string
	hostKey string      // the server's host key fingerprint, once connected
	client  *ssh.Client // the connection, while connected
	link    *link       // the latest peer link, kept when it ends for its last peer
}

// AddTunnel adds a tunnel with mesh on, in state connecting, until Remove.
// remoteGst is the server's gst command; tunnelLog receives the tunnel's
// log, which omits why links fail, as the reasons may quote the server;
// gst mesh peers shows them.
func (n *Node) AddTunnel(name, destination, remoteGst string, tunnelLog *log.Logger) *Tunnel {
	if remoteGst == "" {
		remoteGst = "gst"
	}
	t := &Tunnel{n: n, name: name, destination: destination, remoteGst: remoteGst, log: tunnelLog, state: wire.StateConnecting}
	n.mu.Lock()
	n.tunnels[t] = struct{}{}
	n.mu.Unlock()
	n.Update()
	return t
}

// Remove takes the tunnel out of the mesh.
func (t *Tunnel) Remove() {
	t.n.mu.Lock()
	delete(t.n.tunnels, t)
	t.n.mu.Unlock()
	t.n.Update()
}

// SetState reports the tunnel's state, as in wire.TunnelState.
func (t *Tunnel) SetState(state string) {
	t.mu.Lock()
	changed := t.state != state
	t.state = state
	t.mu.Unlock()
	if changed {
		t.n.Update()
	}
}

// Connected serves the tunnel's SSH connection to its server, whose
// verified host key is hostKey, until ctx is done: it keeps a peer link
// with the gst on the server, retrying after failures, and relays over
// client. It fits tunnel.Options.Attach.
func (t *Tunnel) Connected(ctx context.Context, client *ssh.Client, hostKey ssh.PublicKey) {
	t.connected(ctx, client, ssh.FingerprintSHA256(hostKey), func() (io.ReadWriteCloser, error) {
		return startBridge(client, t.remoteGst)
	})
}

// connected is Connected with bridge, which starts a connection to the
// server's daemon, in place of SSH.
func (t *Tunnel) connected(ctx context.Context, client *ssh.Client, hostKey string, bridge func() (io.ReadWriteCloser, error)) {
	n := t.n
	l := n.add(wire.LinkOut, t.name)
	t.mu.Lock()
	t.hostKey, t.client, t.link = hostKey, client, l
	t.mu.Unlock()
	n.Update()
	defer func() {
		n.remove(l)
		t.mu.Lock()
		t.client = nil
		t.mu.Unlock()
		n.Update()
	}()

	lastErr := ""
	for {
		l.set(wire.LinkConnecting, wire.NodeInfo{}, "")
		var err error = errDisabled
		if n.Enabled() {
			err = t.linkOnce(ctx, l, bridge, func() {
				t.log.Print("peer link up")
				lastErr = ""
			})
		}
		if ctx.Err() != nil {
			return
		}
		// Log once per reason rather than at every retry.
		if err.Error() != lastErr {
			t.log.Print("peer link down")
			lastErr = err.Error()
		}
		l.set(wire.LinkDown, wire.NodeInfo{}, err.Error())
		n.Update()
		select {
		case <-ctx.Done():
			return
		case <-time.After(n.retry):
		}
	}
}

// linkOnce runs one peer link over a new bridge until it ends.
func (t *Tunnel) linkOnce(ctx context.Context, l *link, bridge func() (io.ReadWriteCloser, error), onUp func()) error {
	conn, err := bridge()
	if err != nil {
		return err
	}
	err = t.n.linkOver(ctx, l, conn, onUp)
	if failed, ok := conn.(interface{ failure() string }); ok && errors.Is(err, errNoAnswer) && failed.failure() != "" {
		return fmt.Errorf("the remote gst failed: %s", failed.failure())
	}
	return err
}

// edge describes the tunnel for this node's record.
func (t *Tunnel) edge() wire.Edge {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := wire.Edge{Tunnel: t.name, Destination: t.destination, State: t.state, HostKey: t.hostKey}
	if t.link != nil {
		e.Peer, e.Linked = t.link.peer()
	}
	return e
}

// connection returns the tunnel's SSH connection while it runs.
func (t *Tunnel) connection() *ssh.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != wire.StateRunning {
		return nil
	}
	return t.client
}

// startBridge runs gst peer-bridge on the server over client and returns
// its standard input and output.
func startBridge(client *ssh.Client, remoteGst string) (io.ReadWriteCloser, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open an SSH session: %w", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		return nil, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return nil, err
	}
	conn := &bridgeConn{Reader: stdout, Writer: stdin, session: session, stderr: limitedBuffer{max: 512}}
	session.Stderr = &conn.stderr
	if err := session.Start(shellQuote(remoteGst) + " peer-bridge"); err != nil {
		session.Close()
		return nil, fmt.Errorf("start the remote gst: %w", err)
	}
	return conn, nil
}

// bridgeConn is the remote gst peer-bridge's standard input and output.
type bridgeConn struct {
	io.Reader
	io.Writer
	session *ssh.Session
	stderr  limitedBuffer
	once    sync.Once
}

func (c *bridgeConn) Close() error {
	c.once.Do(func() { c.session.Close() })
	return nil
}

// failure is what the remote gst wrote to its standard error.
func (c *bridgeConn) failure() string { return c.stderr.String() }

// limitedBuffer keeps the first max bytes written to it.
type limitedBuffer struct {
	mu  sync.Mutex
	max int
	buf bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.buf.String())
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
