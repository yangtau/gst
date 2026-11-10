package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// Call calls method, one of wire's control methods, and decodes its
// result into result, which may be nil. When ctx is done first, it closes
// the connection and returns ctx's error.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	stop := context.AfterFunc(ctx, func() { c.conn.Close() })
	err := wire.Call(c.conn, method, params, result)
	if !stop() {
		return ctx.Err()
	}
	return err
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
// done, for use right after the daemon's service starts. A daemon that
// refuses the handshake, as one of another protocol does, is not retried.
func DialWait(ctx context.Context, socket, software, build string) (*Client, error) {
	for {
		c, err := Dial(ctx, socket, software, build)
		var refused *wire.Error
		if err == nil || errors.As(err, &refused) {
			return c, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(100 * time.Millisecond):
		}
	}
}
