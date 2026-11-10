// Package sshconn opens the SSH connections that carry gst links: it
// authenticates with keys that need no passphrase, accepts only host keys
// that known_hosts records, and runs a command on the server. It sends no
// keepalives of its own: the link above sends frames both ways, and tells
// a dead connection by their absence. Its errors omit addresses, users, and paths, so
// that they are safe to log; Check, which is for the user to read,
// reports them.
package sshconn

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Options are the resolved settings of a connection.
type Options struct {
	Server          string        // host:port of the SSH server
	User            string        // remote login user
	IdentityFiles   []string      // private keys to offer; missing and passphrase-protected ones are skipped
	KnownHostsFiles []string      // host key databases; missing files are skipped
	ConnectTimeout  time.Duration // bounds the TCP connect and the SSH handshake
}

// Conn is an SSH connection. It ends when the server closes it, or Close
// is called.
type Conn struct {
	client *ssh.Client
	stop   context.CancelFunc
	done   chan struct{}
	once   sync.Once
	err    error
}

var errClosed = errors.New("SSH connection closed")

// Dial connects and logs in to the server. Cancelling ctx aborts the
// connection attempt and, later, closes the connection.
func Dial(ctx context.Context, opts Options) (*Conn, error) {
	clientConfig, err := newClientConfig(opts)
	if err != nil {
		return nil, err
	}
	client, err := dial(ctx, opts.Server, clientConfig, opts.ConnectTimeout)
	if err != nil {
		return nil, err
	}
	ctx, stop := context.WithCancel(ctx)
	c := &Conn{client: client, stop: stop, done: make(chan struct{})}
	go func() {
		<-ctx.Done()
		c.end(errClosed)
	}()
	go func() {
		client.Wait()
		c.end(errClosed)
	}()
	return c, nil
}

// end records why the connection ended, the first time, and closes it.
func (c *Conn) end(err error) {
	c.once.Do(func() {
		c.err = err
		c.stop()
		c.client.Close()
		close(c.done)
	})
}

// Close closes the connection.
func (c *Conn) Close() error {
	c.end(errClosed)
	return nil
}

// Done is closed when the connection has ended.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err reports why the connection ended, once Done is closed.
func (c *Conn) Err() error {
	<-c.done
	return c.err
}

// Start runs command on the server and returns its standard input and
// output. Closing the session ends it.
func (c *Conn) Start(command string) (*Session, error) {
	session, err := c.client.NewSession()
	if err != nil {
		return nil, errors.New("SSH session open failed")
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
	s := &Session{Reader: stdout, Writer: stdin, session: session, stderr: limitedBuffer{max: 512}, exited: make(chan struct{})}
	session.Stderr = &s.stderr
	if err := session.Start(command); err != nil {
		session.Close()
		return nil, errors.New("remote command start failed")
	}
	go func() {
		session.Wait()
		close(s.exited)
	}()
	return s, nil
}

// Session is a command running on the server: its standard input and
// output.
type Session struct {
	io.Reader
	io.Writer
	session *ssh.Session
	stderr  limitedBuffer
	exited  chan struct{} // closed when the command has exited and its output is copied
}

// Close ends the session.
func (s *Session) Close() error { return s.session.Close() }

// Failure returns the first line that the command wrote to its standard
// error, which for gst carries the error. It may quote the server. It
// waits a moment for a command that is exiting to finish writing.
func (s *Session) Failure() string {
	select {
	case <-s.exited:
	case <-time.After(time.Second):
	}
	line, _, _ := strings.Cut(s.stderr.String(), "\n")
	// What a gst command prints, without the "error: " of its own output.
	return strings.TrimPrefix(line, "error: ")
}

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

// ShellQuote quotes s as one word for a POSIX shell.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// closeOnDone closes c when ctx is done. The returned function cancels this.
func closeOnDone(ctx context.Context, c io.Closer) (stop func() bool) {
	return context.AfterFunc(ctx, func() { c.Close() })
}
