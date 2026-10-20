// Package tunnel runs an SSH tunnel. Remote forwards ask the SSH server to
// listen and relay accepted connections to targets dialed from this machine;
// local forwards listen on this machine and relay to targets dialed by the
// server.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

// Options are the resolved settings of one tunnel.
type Options struct {
	Server            string        // host:port of the SSH server
	User              string        // remote login user
	IdentityFiles     []string      // private keys to offer; missing and passphrase-protected ones are skipped
	KnownHostsFiles   []string      // host key databases; missing files are skipped
	ConnectTimeout    time.Duration // bounds TCP connects, the SSH handshake, and remote cleanup
	KeepaliveInterval time.Duration // time between keepalive probes
	KeepaliveTimeout  time.Duration // how long one probe may stay unanswered
	CleanupListeners  bool
	Forwards          []config.Forward
}

// Run connects to the server and serves the forwards until ctx is
// cancelled (returning nil) or the tunnel fails (returning the first error).
// Errors omit addresses and paths so that they are safe to log.
func Run(ctx context.Context, opts Options) error {
	clientConfig, err := newClientConfig(opts)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	client, err := dial(ctx, opts.Server, clientConfig, opts.ConnectTimeout)
	if err != nil {
		return err
	}

	t := &tunnel{
		opts:            opts,
		client:          client,
		failure:         make(chan error, 1),
		listenersClosed: make(chan struct{}),
	}
	defer t.shutdown(cancel)
	return t.serve(ctx)
}

// tunnel holds the state of one SSH connection and its listeners.
type tunnel struct {
	opts      Options
	client    *ssh.Client
	listeners []net.Listener
	workers   sync.WaitGroup

	failure         chan error    // holds the first failure; later ones are dropped
	listenersClosed chan struct{} // closed once shutdown has closed every listener
}

func (t *tunnel) serve(ctx context.Context) error {
	t.workers.Go(func() {
		if err := keepalive(ctx, t.client, t.opts.KeepaliveInterval, t.opts.KeepaliveTimeout); err != nil {
			// Report before closing, so the failure is not attributed to
			// the connection loss that the close causes. Closing also
			// unblocks requests stalled behind the probe, such as ListenTCP.
			t.fail(err)
			t.client.Close()
		}
	})
	t.workers.Go(func() {
		t.client.Wait()
		t.fail(errors.New("SSH connection closed"))
	})

	if t.opts.CleanupListeners {
		if err := cleanupListeners(t.client, t.opts.Forwards, t.opts.ConnectTimeout); err != nil {
			return err
		}
	}
	for i, fwd := range t.opts.Forwards {
		if err := t.listen(ctx, i+1, fwd); err != nil {
			return err
		}
	}
	log.Print("tunnel ready")

	select {
	case <-ctx.Done():
		return nil
	case err := <-t.failure:
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
}

// listen opens the listener for one forward and starts accepting on it.
func (t *tunnel) listen(ctx context.Context, id int, fwd config.Forward) error {
	addr := net.TCPAddrFromAddrPort(fwd.ListenAddr())
	r := route{id: id, target: fwd.Target}
	var err error
	if fwd.Direction == config.Remote {
		r.listener, err = t.client.ListenTCP(addr)
		r.dial = new(net.Dialer).DialContext
	} else {
		r.listener, err = net.ListenTCP("tcp", addr)
		r.dial = t.client.DialContext
	}
	if err != nil {
		return fmt.Errorf("listener start failed for forward %d", id)
	}
	t.listeners = append(t.listeners, r.listener)
	t.workers.Go(func() { t.accept(ctx, r) })
	return nil
}

// route is one active forward: a listener and the target it feeds.
type route struct {
	id       int // 1-based position in the tunnel's forwards, logged instead of addresses
	listener net.Listener
	dial     func(ctx context.Context, network, address string) (net.Conn, error)
	target   string
}

func (t *tunnel) accept(ctx context.Context, r route) {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			// Listeners stop at shutdown or, for x/crypto/ssh listeners,
			// when the connection ends, which the client.Wait watcher in
			// serve reports.
			//
			// Accept can fail while queued channel opens still hold the SSH
			// client's forwarding lock. Keep draining them until shutdown has
			// removed every listener, or Close would deadlock.
			select {
			case <-t.listenersClosed:
				return
			default:
				continue
			}
		}
		t.workers.Go(func() { t.forward(ctx, r, conn) })
	}
}

func (t *tunnel) fail(err error) {
	select {
	case t.failure <- err:
	default:
	}
}

func (t *tunnel) shutdown(cancel context.CancelFunc) {
	cancel()
	// Close the transport before the listeners: an SSH listener's Close
	// waits for the server's reply, which would never come otherwise.
	t.client.Close()
	for _, listener := range t.listeners {
		listener.Close()
	}
	close(t.listenersClosed)
	t.workers.Wait()
}

// closeOnDone closes c when ctx is done. The returned function cancels this.
func closeOnDone(ctx context.Context, c io.Closer) (stop func() bool) {
	return context.AfterFunc(ctx, func() { c.Close() })
}
