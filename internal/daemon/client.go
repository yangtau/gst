package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"os"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Client is a control connection to a daemon.
type Client struct {
	conn net.Conn
	// Daemon is the daemon's Hello.
	Daemon wire.Hello
}

// Dial connects to the daemon on socket and performs the handshake,
// identifying this side as software and build.
func Dial(ctx context.Context, socket, software, build string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	hello, err := wire.Dial(conn, wire.Hello{Software: software, Role: wire.RoleControl, Build: build})
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &Client{conn: conn, Daemon: hello}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) call(ctx context.Context, method string, params any) (wire.Status, error) {
	stop := context.AfterFunc(ctx, func() { c.conn.Close() })
	defer stop()
	var status wire.Status
	err := wire.Call(c.conn, method, params, &status)
	if ctx.Err() != nil {
		return status, ctx.Err()
	}
	return status, err
}

// Status returns the daemon's status.
func (c *Client) Status(ctx context.Context) (wire.Status, error) {
	return c.call(ctx, wire.MethodStatus, nil)
}

// Reload makes the daemon sync its tunnels with the configuration file.
func (c *Client) Reload(ctx context.Context) (wire.Status, error) {
	return c.call(ctx, wire.MethodReload, nil)
}

// Restart restarts the named tunnels.
func (c *Client) Restart(ctx context.Context, names []string) (wire.Status, error) {
	return c.call(ctx, wire.MethodRestart, wire.RestartParams{Names: names})
}

// Nodes returns the mesh as the daemon knows it.
func (c *Client) Nodes(ctx context.Context) (wire.Topology, error) {
	stop := context.AfterFunc(ctx, func() { c.conn.Close() })
	defer stop()
	var t wire.Topology
	err := wire.Call(c.conn, wire.MethodNodes, nil, &t)
	if ctx.Err() != nil {
		return t, ctx.Err()
	}
	return t, err
}

// Peers returns the daemon's peer links.
func (c *Client) Peers(ctx context.Context) (wire.Peers, error) {
	stop := context.AfterFunc(ctx, func() { c.conn.Close() })
	defer stop()
	var peers wire.Peers
	err := wire.Call(c.conn, wire.MethodPeers, nil, &peers)
	if ctx.Err() != nil {
		return peers, ctx.Err()
	}
	return peers, err
}

// Build identifies the executable at path by the SHA-256 of its content,
// so that a gst command can tell whether the daemon runs the same gst.
func Build(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// DialWait dials like Dial, retrying until the daemon accepts or ctx is
// done, for use right after the daemon's service starts.
func DialWait(ctx context.Context, socket, software, build string) (*Client, error) {
	for {
		c, err := Dial(ctx, socket, software, build)
		if err == nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(100 * time.Millisecond):
		}
	}
}
