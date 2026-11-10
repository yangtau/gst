package sshconn

import (
	"context"
	"errors"
	"io"
	"time"
)

// DefaultConnectTimeout bounds a connection whose settings name no
// timeout.
const DefaultConnectTimeout = 15 * time.Second

// RemoteGstPath is where gst up puts gst, under the home directory, for
// the other machines' links to run.
const RemoteGstPath = ".local/state/gst/bin/gst"

// LinkCommand runs gst peer-bridge on the other machine of a link: the
// gst that gst up put in place there, or else the one in the PATH. sh -c
// runs it whatever the login shell is.
const LinkCommand = `sh -c 'g="$HOME/` + RemoteGstPath + `"; [ -x "$g" ] || g=gst; exec "$g" peer-bridge'`

// Link connects to the daemon on the server: it logs in and runs gst
// peer-bridge there, whose standard input and output it returns. Closing
// that closes the SSH connection too. Its errors omit addresses, users,
// and paths.
func Link(ctx context.Context, opts Options) (io.ReadWriteCloser, error) {
	conn, err := Dial(ctx, opts)
	if err != nil {
		return nil, err
	}
	// A server can stall opening the session; the mesh handshake bounds
	// only what follows.
	stall := time.AfterFunc(DefaultConnectTimeout, func() { conn.Close() })
	session, err := conn.Start(LinkCommand)
	if !stall.Stop() && err == nil {
		session.Close()
		err = errors.New("the remote command did not start in time")
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &linkConn{session, conn}, nil
}

// linkConn is gst peer-bridge on a link's server; closing it closes the
// SSH connection too.
type linkConn struct {
	*Session
	conn *Conn
}

func (c *linkConn) Close() error {
	c.Session.Close()
	return c.conn.Close()
}
