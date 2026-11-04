package daemon

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/proxy"
	"github.com/yangtau/ssh-tunnel/internal/relay"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// listener is the proxy or a map: a listen address and what serves the
// connections accepted on it.
type listener struct {
	listen string
	spec   config.Map // for maps
	err    error      // why it does not listen
	cancel context.CancelFunc
	done   chan struct{}
}

func (l *listener) state() wire.Listener {
	s := wire.Listener{Listen: l.listen}
	if l.err != nil {
		s.Error = l.err.Error()
	}
	return s
}

// close stops listening and waits until the listener has shut down.
// Connections that it accepted keep going.
func (l *listener) close() {
	if l.cancel != nil {
		l.cancel()
		<-l.done
	}
}

// open listens on address, a host:port or a socket path, and serves the
// connections it accepts with serve until close. A failure to listen is
// recorded in the listener.
func open(ctx context.Context, address string, serve func(ctx context.Context, ln net.Listener)) *listener {
	l := &listener{listen: address}
	var ln net.Listener
	if config.IsSocket(address) {
		ln, l.err = listenUnix(address)
	} else {
		ln, l.err = net.Listen("tcp", address)
	}
	if l.err != nil {
		var op *net.OpError
		if errors.As(l.err, &op) {
			l.err = op.Err
		}
		return l
	}
	ctx, l.cancel = context.WithCancel(ctx)
	l.done = make(chan struct{})
	go func() {
		defer close(l.done)
		defer ln.Close()
		defer context.AfterFunc(ctx, func() { ln.Close() })()
		serve(ctx, ln)
	}()
	return l
}

// listenUnix listens on a socket path. A socket file that nothing accepts
// on is left over from an earlier run and is replaced. The socket is
// accessible to its owner only.
func listenUnix(path string) (net.Listener, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode().Type() == fs.ModeSocket {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return nil, errors.New("socket in use")
		}
		os.Remove(path)
	}
	// The umask applies to the whole process; nothing else creates files
	// meanwhile, and a stricter mask would be harmless if something did.
	defer syscall.Umask(syscall.Umask(0o177))
	return net.Listen("unix", path)
}

// listen makes the proxy and the maps listen as f says. Listeners whose
// settings did not change keep running; ones that failed try again.
func (d *Daemon) listen(f *config.File) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ctx := d.serveCtx
	var closing []*listener
	if p := d.proxy; p != nil && (p.listen != f.Proxy || p.err != nil) {
		closing = append(closing, p)
		d.proxy = nil
	}
	want := map[string]config.Map{}
	for _, m := range f.Maps {
		want[m.Listen] = m
	}
	for addr, l := range d.maps {
		if m, ok := want[addr]; !ok || m != l.spec || l.err != nil {
			closing = append(closing, l)
			delete(d.maps, addr)
		}
	}
	// Close before opening, so that a listener can move to the address
	// that another leaves.
	var wg sync.WaitGroup
	for _, l := range closing {
		wg.Go(l.close)
	}
	wg.Wait()
	if d.proxy == nil && f.Proxy != "" {
		srv := &proxy.Server{Dial: d.dialProxy}
		d.proxy = open(ctx, f.Proxy, func(ctx context.Context, ln net.Listener) { srv.Serve(ctx, ln) })
	}
	for addr, m := range want {
		if d.maps[addr] == nil {
			l := open(ctx, addr, func(ctx context.Context, ln net.Listener) { d.serveMap(ctx, ln, m) })
			l.spec = m
			d.maps[addr] = l
		}
	}
}

// serveMap connects each connection that ln accepts to m's target through
// the mesh. Failures are not logged, as they would name the target.
func (d *Daemon) serveMap(ctx context.Context, ln net.Listener, m config.Map) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			target, err := d.mesh.Dial(ctx, m.Machine, config.Network(m.Target), m.Target)
			if err != nil {
				conn.Close()
				return
			}
			relay.Pipe(conn, target)
		}()
	}
}

// dialProxy connects for a client of the proxy: to a port of a machine
// under .gst, through the exit that a route names, or else directly.
func (d *Daemon) dialProxy(ctx context.Context, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, wire.Errorf(wire.CodeInvalidParams, "invalid address %q", address)
	}
	if machine, ok := strings.CutSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".gst"); ok {
		return d.mesh.Dial(ctx, machine, "tcp", net.JoinHostPort("localhost", port))
	}
	if via, ok := d.config.Load().Routes.Lookup(host); ok {
		return d.mesh.Dial(ctx, via, "tcp", address)
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", address)
}
