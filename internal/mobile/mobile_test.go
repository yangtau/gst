package mobile

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/dns/dnsmessage"
	wgtun "golang.zx2c4.com/wireguard/tun"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/daemon"
	"github.com/yangtau/ssh-tunnel/internal/sshconn"
	"github.com/yangtau/ssh-tunnel/internal/tun"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// server is a machine that a phone adds: a gst daemon behind an SSH
// server, which runs the links' command as gst peer-bridge does, by
// joining the session to the daemon's socket.
type server struct {
	address string
	hostKey ssh.Signer
	socket  string

	mu         sync.Mutex
	authorized []byte // the key that may log in
}

func startServer(t *testing.T) *server {
	t.Helper()
	// Unix socket paths are short on macOS, so avoid t.TempDir's long names.
	dir, err := os.MkdirTemp("", "gst")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s := &server{socket: filepath.Join(dir, "gst.sock")}
	cfg := filepath.Join(dir, "config.json")
	err = config.Update(cfg, func(f *config.File) error {
		f.Node = &config.Node{ID: strings.Repeat("5e", 16), Name: "server"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	d := daemon.New(daemon.Config{ConfigPath: cfg, Socket: s.socket, Software: "gst test", Log: log.New(io.Discard, "", 0)})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx) }()

	_, private, _ := ed25519.GenerateKey(rand.Reader)
	s.hostKey, _ = ssh.NewSignerFromKey(private)
	sshConfig := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !bytes.Equal(key.Marshal(), s.authorized) {
			return nil, io.EOF
		}
		return nil, nil
	}}
	sshConfig.AddHostKey(s.hostKey)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.address = listener.Addr().String()
	var conns sync.WaitGroup
	go func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			conns.Go(func() {
				defer raw.Close()
				defer context.AfterFunc(ctx, func() { raw.Close() })()
				s.serve(raw, sshConfig)
			})
		}
	}()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		conns.Wait()
		<-served
	})
	return s
}

func (s *server) serve(raw net.Conn, sshConfig *ssh.ServerConfig) {
	conn, channels, requests, err := ssh.NewServerConn(raw, sshConfig)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(requests)
	for ch := range channels {
		channel, reqs, err := ch.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer channel.Close()
			for req := range reqs {
				var exec struct{ Command string }
				if req.Type != "exec" || ssh.Unmarshal(req.Payload, &exec) != nil || exec.Command != sshconn.LinkCommand {
					req.Reply(false, nil)
					continue
				}
				req.Reply(true, nil)
				daemon, err := net.Dial("unix", s.socket)
				if err != nil {
					return
				}
				defer daemon.Close()
				go io.Copy(daemon, channel)
				io.Copy(channel, daemon)
				return
			}
		}()
	}
}

// authorize lets the key of an authorized_keys line log in.
func (s *server) authorize(t *testing.T, line string) {
	t.Helper()
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.authorized = key.Marshal()
	s.mu.Unlock()
}

// fakeDevice is the system's interface, whose packets the test reads and
// writes.
type fakeDevice struct {
	in, out chan []byte
	closed  chan struct{}
	events  chan wgtun.Event
	once    sync.Once
}

func newFakeDevice() *fakeDevice {
	f := &fakeDevice{in: make(chan []byte, 16), out: make(chan []byte, 16), closed: make(chan struct{}), events: make(chan wgtun.Event, 1)}
	f.events <- wgtun.EventUp
	return f
}

func (f *fakeDevice) File() *os.File             { return nil }
func (f *fakeDevice) MTU() (int, error)          { return tun.MTU, nil }
func (f *fakeDevice) Name() (string, error)      { return "utun9", nil }
func (f *fakeDevice) Events() <-chan wgtun.Event { return f.events }
func (f *fakeDevice) BatchSize() int             { return 1 }
func (f *fakeDevice) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}
func (f *fakeDevice) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case p := <-f.in:
		sizes[0] = copy(bufs[0][offset:], p)
		return 1, nil
	case <-f.closed:
		return 0, os.ErrClosed
	}
}
func (f *fakeDevice) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		select {
		case f.out <- append([]byte(nil), b[offset:]...):
		case <-f.closed:
			return 0, os.ErrClosed
		}
	}
	return len(bufs), nil
}

// call calls a method of a, decodes its result into result, and returns
// its error.
func call(t *testing.T, a *App, method string, params, result any) *wire.Error {
	t.Helper()
	req := wire.Request{Method: method}
	if params != nil {
		req.Params, _ = json.Marshal(params)
	}
	resp := a.Call(t.Context(), req)
	if resp.Error == nil && result != nil {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	return resp.Error
}

func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out: " + msg)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// udpQuery is a DNS query for name in a UDP packet over IPv4.
func udpQuery(t *testing.T, from, to netip.AddrPort, name string) []byte {
	t.Helper()
	m := dnsmessage.Message{Header: dnsmessage.Header{ID: 7}, Questions: []dnsmessage.Question{
		{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
	}}
	query, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	src, dst := from.Addr().As4(), to.Addr().As4()
	p := []byte{0x45, 0, 0, 0, 0, 0, 0, 0, 64, 17, 0, 0}
	binary.BigEndian.PutUint16(p[2:], uint16(28+len(query)))
	p = append(append(p, src[:]...), dst[:]...)
	p = binary.BigEndian.AppendUint16(p, from.Port())
	p = binary.BigEndian.AppendUint16(p, to.Port())
	p = binary.BigEndian.AppendUint16(p, uint16(8+len(query)))
	return append(append(p, 0, 0), query...)
}

// A phone joins a network as a person does it in the app: it checks the
// server, trusts its host key, has its own key authorized there, adds the
// link, and turns the VPN on.
func TestAPhoneJoinsANetwork(t *testing.T) {
	s := startServer(t)
	dir := filepath.Join(t.TempDir(), "gst")
	app, err := Open(dir, "My iPhone")
	if err != nil {
		t.Fatal(err)
	}
	var info Info
	if err := call(t, app, MethodInfo, nil, &info); err != nil {
		t.Fatal(err)
	}
	if info.Node.Name != "my-iphone" || !config.ValidNodeID(info.Node.ID) || len(info.Links) != 0 ||
		!strings.HasPrefix(info.PublicKey, "ssh-ed25519 ") || !strings.HasSuffix(info.PublicKey, " gst@my-iphone") {
		t.Fatalf("info = %+v", info)
	}
	// Opening it again, as the VPN's process does, keeps all of it.
	vpn, err := Open(dir, "another name")
	if err != nil {
		t.Fatal(err)
	}
	var again Info
	if err := call(t, vpn, MethodInfo, nil, &again); err != nil || again.Node != info.Node || again.PublicKey != info.PublicKey {
		t.Fatalf("opened again: %+v, %v", again, err)
	}

	link := LinkParams{Destination: "ssh://tester@" + s.address}
	var check Check
	if err := call(t, app, MethodCheck, link, &check); err != nil {
		t.Fatal(err)
	}
	if check.State != CheckUnknownHost || check.HostKey == nil || check.HostKey.Fingerprint != ssh.FingerprintSHA256(s.hostKey.PublicKey()) ||
		check.HostKey.Type != "ssh-ed25519" || check.Server != s.address || check.User != "tester" {
		t.Fatalf("an unknown server: %+v", check)
	}
	if err := call(t, app, MethodAdd, link, nil); err == nil {
		t.Fatal("added a link to a server whose host key nobody checked")
	}
	if err := call(t, app, MethodTrust, TrustParams{link.Destination, check.HostKey.Key}, nil); err != nil {
		t.Fatal(err)
	}
	check = Check{}
	if err := call(t, app, MethodCheck, link, &check); err != nil || check.State != CheckUnauthorized || check.HostKey != nil {
		t.Fatalf("an unauthorized key: %+v, %v", check, err)
	}
	s.authorize(t, info.PublicKey)
	if err := call(t, app, MethodCheck, link, &check); err != nil || check.State != CheckReady {
		t.Fatalf("an authorized key: %+v, %v", check, err)
	}
	if err := call(t, app, MethodAdd, link, &info); err != nil || len(info.Links) != 1 || info.Links[0].Destination != link.Destination {
		t.Fatalf("add: %+v, %v", info, err)
	}
	for _, bad := range []string{"", "ssh.example.com", "-oProxyCommand=x", "tester@ssh example.com"} {
		if err := call(t, app, MethodCheck, LinkParams{bad}, nil); err == nil || err.Code != wire.CodeInvalidParams {
			t.Errorf("check %q: %v", bad, err)
		}
	}

	// The daemon's methods are the VPN's.
	if err := call(t, app, wire.MethodStatus, nil, nil); err == nil || !strings.Contains(err.Message, "not running") {
		t.Fatalf("status without the VPN: %v", err)
	}
	device := newFakeDevice()
	vpn.device = func() (Device, error) { return device, nil }
	settings := make(chan Settings, 16)
	if err := vpn.Start(func(s Settings) error { settings <- s; return nil }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vpn.Stop)
	if err := vpn.Start(nil); err == nil {
		t.Fatal("started twice")
	}
	var set Settings
	select {
	case set = <-settings:
	case <-time.After(10 * time.Second):
		t.Fatal("the system got no settings")
	}
	address, err := netip.ParseAddr(set.Address)
	if err != nil || len(set.Routes) != 2 || set.Routes[0].Mask != "255.255.0.0" || !strings.HasSuffix(set.Routes[0].Address, ".0.0") ||
		set.Routes[1] != (Route{"198.18.0.53", "255.255.255.255"}) || set.DNS != "198.18.0.53" || set.Domain != "gst" || set.MTU != tun.MTU {
		t.Fatalf("settings = %+v", set)
	}
	subnet := netip.MustParsePrefix(set.Routes[0].Address + "/16")
	if !subnet.Contains(address) || subnet.Contains(netip.MustParseAddr(set.DNS)) {
		t.Fatalf("settings = %+v", set)
	}
	var st wire.Status
	waitFor(t, "the link runs", func() bool {
		return call(t, vpn, wire.MethodStatus, nil, &st) == nil && len(st.Links) == 1 && st.Links[0].State == wire.StateRunning
	})
	if st.Links[0].Peer.Name != "server" || st.Node.Name != "my-iphone" {
		t.Fatalf("status = %+v", st)
	}
	waitFor(t, "the network is up", func() bool {
		return call(t, vpn, wire.MethodStatus, nil, &st) == nil && st.Net != nil && st.Net.Interface == "utun9" && st.Net.Address == set.Address
	})
	var topo wire.Topology
	if err := call(t, vpn, wire.MethodTopology, nil, &topo); err != nil || len(topo.Nodes) != 2 {
		t.Fatalf("topology = %+v, %v", topo, err)
	}
	waitFor(t, "the link keeps the machine it reached", func() bool {
		return call(t, app, MethodInfo, nil, &info) == nil && info.Links[0].Target == strings.Repeat("5e", 16)
	})

	// The system asks the interface for names under .gst.
	own, dns := netip.AddrPortFrom(address, 50000), netip.MustParseAddrPort("198.18.0.53:53")
	device.in <- udpQuery(t, own, dns, "my-iphone.gst.")
	select {
	case p := <-device.out:
		var m dnsmessage.Message
		if err := m.Unpack(p[28:]); err != nil || len(m.Answers) != 1 || netip.AddrFrom4(m.Answers[0].Body.(*dnsmessage.AResource).A) != address {
			t.Fatalf("the answer is %+v, %v", m, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no answer for a name under .gst")
	}

	// What the app changes, the daemon takes up when told.
	if err := call(t, app, MethodName, NameParams{"phone"}, &info); err != nil || info.Node.Name != "phone" || !strings.HasSuffix(info.PublicKey, " gst@phone") {
		t.Fatalf("name: %+v, %v", info, err)
	}
	if err := call(t, app, MethodName, NameParams{"My Phone"}, nil); err == nil || err.Code != wire.CodeInvalidParams {
		t.Fatalf("an invalid name: %v", err)
	}
	// A new key replaces the one that the server authorized.
	old := info.PublicKey
	if err := call(t, app, MethodNewKey, nil, &info); err != nil || info.PublicKey == old ||
		!strings.HasPrefix(info.PublicKey, "ssh-ed25519 ") || !strings.HasSuffix(info.PublicKey, " gst@phone") {
		t.Fatalf("new key: %q after %q, %v", info.PublicKey, old, err)
	}
	if err := call(t, app, MethodCheck, link, &check); err != nil || check.State != CheckUnauthorized {
		t.Fatalf("check with the new key: %+v, %v", check, err)
	}
	if err := call(t, app, MethodUnlink, link, &info); err != nil || len(info.Links) != 0 {
		t.Fatalf("unlink: %+v, %v", info, err)
	}
	if err := call(t, app, MethodUnlink, link, nil); err == nil || err.Code != wire.CodeNotFound {
		t.Fatalf("unlink again: %v", err)
	}
	if err := call(t, vpn, wire.MethodReload, nil, &st); err != nil || len(st.Links) != 0 || st.Node.Name != "phone" {
		t.Fatalf("reload: %+v, %v", st, err)
	}

	vpn.Stop()
	select {
	case <-device.closed:
	default:
		t.Fatal("the interface is still open")
	}
	if err := call(t, vpn, wire.MethodStatus, nil, nil); err == nil {
		t.Fatal("status after Stop")
	}
	// The log is for the app to show, and names no server.
	var logged Log
	if err := call(t, app, MethodLog, nil, &logged); err != nil || !strings.Contains(logged.Text, "daemon ready") ||
		!strings.Contains(logged.Text, "link "+config.LinkID(link.Destination)+": up") {
		t.Fatalf("log = %q, %v", logged.Text, err)
	}
	if host, _, _ := net.SplitHostPort(s.address); strings.Contains(logged.Text, host) || strings.Contains(logged.Text, "tester") {
		t.Fatalf("the log names the server: %q", logged.Text)
	}
}
