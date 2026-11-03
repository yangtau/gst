// Package mesh links gst daemons with each other. A tunnel with mesh on
// runs "gst peer-bridge" on its server over the tunnel's SSH connection;
// the bridge connects to the server's daemon, the two daemons shake hands
// with the gst protocol in the peer role, and the connection continues as
// a yamux session that carries one call per stream in both directions.
package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/crypto/ssh"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Node is this machine's place in the mesh: its identity and its links.
type Node struct {
	software string
	log      *log.Logger // the daemon's log
	retry    time.Duration

	mu    sync.Mutex
	info  wire.NodeInfo // ID is empty while the mesh is disabled
	links map[*link]struct{}
}

// New returns a node, disabled until Configure enables it.
func New(software string, daemonLog *log.Logger) *Node {
	return &Node{software: software, log: daemonLog, retry: 10 * time.Second, links: map[*link]struct{}{}}
}

// SetRetry sets the wait before an outgoing link that failed retries.
func (n *Node) SetRetry(d time.Duration) { n.retry = d }

// Configure applies the mesh settings; nil disables the mesh and closes
// every link.
func (n *Node) Configure(m *config.Mesh) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if m == nil {
		n.info = wire.NodeInfo{}
		for l := range n.links {
			l.closeSession()
		}
		return
	}
	n.info = wire.NodeInfo{ID: m.ID, Name: m.Name, Software: n.software}
}

func (n *Node) self() wire.NodeInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.info
}

// Enabled reports whether this machine takes part in the mesh.
func (n *Node) Enabled() bool { return n.self().ID != "" }

// Peers reports this node and its links.
func (n *Node) Peers() wire.Peers {
	n.mu.Lock()
	p := wire.Peers{Enabled: n.info.ID != "", Node: n.info, Links: []wire.PeerLink{}}
	links := make([]*link, 0, len(n.links))
	for l := range n.links {
		links = append(links, l)
	}
	n.mu.Unlock()
	for _, l := range links {
		p.Links = append(p.Links, l.state())
	}
	slices.SortFunc(p.Links, func(a, b wire.PeerLink) int {
		return strings.Compare(a.Direction+a.Tunnel+a.Peer.Name+a.Peer.ID, b.Direction+b.Tunnel+b.Peer.Name+b.Peer.ID)
	})
	return p
}

// CheckHello accepts a peer's Hello on the control socket when the mesh is
// enabled and the peer speaks yamux.
func (n *Node) CheckHello(h wire.Hello) *wire.Error {
	if !n.Enabled() {
		return wire.Errorf(wire.CodeUnsupportedRole, "gst mesh is not enabled on this machine; run `gst mesh enable` there")
	}
	if !slices.Contains(h.Features, wire.FeatureYamux) {
		return wire.Errorf(wire.CodeUnsupportedRole, "the peer does not support yamux")
	}
	return nil
}

// Hello is this node's Hello for peer connections.
func (n *Node) Hello() wire.Hello {
	return wire.Hello{Software: n.software, Role: wire.RolePeer, Features: []string{wire.FeatureYamux}}
}

// Accept serves a peer link whose handshake succeeded on conn, until it
// ends or ctx is done.
func (n *Node) Accept(ctx context.Context, conn io.ReadWriteCloser) {
	session, err := yamux.Server(conn, yamuxConfig())
	if err != nil {
		conn.Close()
		return
	}
	l := n.add(wire.LinkIn, "")
	defer n.remove(l)
	if err := n.run(ctx, l, session, nil); err != nil && ctx.Err() == nil && n.Enabled() {
		n.log.Printf("incoming peer link ended: %v", err)
	}
}

// Link keeps a peer link to the server of tunnel over client until ctx is
// done, retrying after failures. remoteGst is the server's gst command.
// tunnelLog receives the tunnel's log, which omits the reasons, as they
// may quote the server; gst mesh peers shows them.
func (n *Node) Link(ctx context.Context, tunnel string, client *ssh.Client, remoteGst string, tunnelLog *log.Logger) {
	l := n.add(wire.LinkOut, tunnel)
	defer n.remove(l)
	lastErr := ""
	for {
		l.set(wire.LinkConnecting, wire.NodeInfo{}, "")
		var err error
		if n.Enabled() {
			err = n.dial(ctx, l, client, remoteGst, func() {
				tunnelLog.Print("peer link up")
				lastErr = ""
			})
		} else {
			err = errors.New("gst mesh is not enabled on this machine; run `gst mesh enable`")
		}
		if ctx.Err() != nil {
			return
		}
		// Log once per reason rather than at every retry.
		if err.Error() != lastErr {
			tunnelLog.Print("peer link down")
			lastErr = err.Error()
		}
		l.set(wire.LinkDown, wire.NodeInfo{}, err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(n.retry):
		}
	}
}

func (n *Node) dial(ctx context.Context, l *link, client *ssh.Client, remoteGst string, onUp func()) error {
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("open an SSH session: %w", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		return err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return err
	}
	stderr := &limitedBuffer{max: 512}
	session.Stderr = stderr
	if remoteGst == "" {
		remoteGst = "gst"
	}
	if err := session.Start(shellQuote(remoteGst) + " peer-bridge"); err != nil {
		session.Close()
		return fmt.Errorf("start the remote gst: %w", err)
	}
	conn := &sessionConn{Reader: stdout, Writer: stdin, session: session}
	err = n.linkOver(ctx, l, conn, onUp)
	if errors.Is(err, errNoAnswer) && stderr.String() != "" {
		return fmt.Errorf("the remote gst failed: %s", stderr.String())
	}
	return err
}

var errNoAnswer = errors.New("the remote gst did not answer; is gst 0.2 or later installed there? See --remote-gst")

// linkOver runs the connecting side of a peer link over conn, which leads
// to the peer's daemon, and closes conn when done.
func (n *Node) linkOver(ctx context.Context, l *link, conn io.ReadWriteCloser, onUp func()) error {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	hello, err := wire.Dial(conn, n.Hello())
	if err != nil {
		var refused *wire.Error
		if errors.As(err, &refused) {
			return fmt.Errorf("the remote gst refused the link: %s", refused.Message)
		}
		return errNoAnswer
	}
	if !slices.Contains(hello.Features, wire.FeatureYamux) {
		return errors.New("the remote gst does not support yamux")
	}
	mux, err := yamux.Client(conn, yamuxConfig())
	if err != nil {
		return err
	}
	return n.run(ctx, l, mux, onUp)
}

// run serves the peer's calls, learns who the peer is, and waits until the
// session ends, which it reports as an error unless ctx is done.
func (n *Node) run(ctx context.Context, l *link, session *yamux.Session, onUp func()) error {
	defer session.Close()
	l.setSession(session)
	go n.serve(session)

	peer, err := n.nodeInfo(ctx, session)
	if err != nil {
		return err
	}
	if peer.ID == n.self().ID {
		// The connecting side reports this and closes the link; the
		// accepting side waits for that, so the report is not lost.
		if l.state().Direction == wire.LinkOut {
			return errors.New("the server is this machine")
		}
		<-session.CloseChan()
		return nil
	}
	l.set(wire.LinkUp, peer, "")
	if onUp != nil {
		onUp()
	}
	select {
	case <-ctx.Done():
		return nil
	case <-session.CloseChan():
		return errors.New("the peer link closed")
	}
}

func (n *Node) nodeInfo(ctx context.Context, session *yamux.Session) (wire.NodeInfo, error) {
	stream, err := session.OpenStream()
	if err != nil {
		return wire.NodeInfo{}, err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	stream.SetDeadline(time.Now().Add(30 * time.Second))
	var info wire.NodeInfo
	if err := wire.Call(stream, wire.MethodNodeInfo, nil, &info); err != nil {
		return info, fmt.Errorf("ask the peer who it is: %w", err)
	}
	if info.ID == "" {
		return info, errors.New("the peer sent no node id")
	}
	return info, nil
}

// serve answers the calls that the peer opens streams for.
func (n *Node) serve(session *yamux.Session) {
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		go func() {
			defer stream.Close()
			wire.Serve(stream, n.handle)
		}()
	}
}

func (n *Node) handle(method string, _ json.RawMessage) (any, *wire.Error) {
	switch method {
	case wire.MethodNodeInfo:
		return n.self(), nil
	default:
		return nil, wire.Errorf(wire.CodeUnsupportedMethod, "method %q is not supported", method)
	}
}

func (n *Node) add(direction, tunnel string) *link {
	l := &link{info: wire.PeerLink{Direction: direction, Tunnel: tunnel, State: wire.LinkConnecting, Since: time.Now()}}
	n.mu.Lock()
	n.links[l] = struct{}{}
	n.mu.Unlock()
	return l
}

func (n *Node) remove(l *link) {
	n.mu.Lock()
	delete(n.links, l)
	n.mu.Unlock()
}

func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.ConnectionWriteTimeout = 30 * time.Second
	return cfg
}

// link is one peer link and its state.
type link struct {
	mu      sync.Mutex
	info    wire.PeerLink
	session *yamux.Session
}

func (l *link) set(state string, peer wire.NodeInfo, errText string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.info.State != state {
		l.info.State, l.info.Since = state, time.Now()
	}
	l.info.Peer, l.info.Error = peer, errText
}

func (l *link) setSession(s *yamux.Session) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.session = s
}

func (l *link) closeSession() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.session != nil {
		l.session.Close()
	}
}

func (l *link) state() wire.PeerLink {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.info
}

// sessionConn is the remote gst's standard input and output.
type sessionConn struct {
	io.Reader
	io.Writer
	session *ssh.Session
	once    sync.Once
}

func (c *sessionConn) Close() error {
	c.once.Do(func() { c.session.Close() })
	return nil
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

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
