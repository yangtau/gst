// Package vnet runs this machine's part in the virtual network, which is
// how machines of the mesh reach each other: each gets an IP address in one
// private subnet and a name under .gst, and programs reach them through a
// network interface, with any protocol. A WireGuard device (wireguard-go)
// encrypts the packets end to end, and the mesh carries them, over UDP
// where the network allows and along its SSH links where not (bind.go).
//
// There is no coordinator. Each node publishes its subnet, address, and
// WireGuard key in its link-state record, and every node works out the
// same result from the records: the latest choice of subnet counts, and of
// two nodes that picked the same address, the one with the lower id keeps
// it. The mesh already says which machines belong, and it is what makes
// the keys in the records trustworthy.
package vnet

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/device"
	wgtun "golang.zx2c4.com/wireguard/tun"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/expose"
	"github.com/yangtau/ssh-tunnel/internal/mesh"
	"github.com/yangtau/ssh-tunnel/internal/tun"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

const (
	retryInterval = 5 * time.Second // to look at everything again, and to retry what failed
	askTimeout    = 3 * time.Second // for a machine to report its networks
)

// Mesh is what the network needs of its node; *mesh.Node has it.
type Mesh interface {
	Self() wire.NodeInfo
	Topology() wire.Topology
	Changed() <-chan struct{}
	SetNet(*wire.NetInfo)
	SendPacket(node string, packet []byte)
	HandlePackets(func(node string, packet []byte))
	PacketPath(node string) string
	PacketEndpoint(node string) string
	Call(ctx context.Context, ref, method string, params, result any) error
}

// Interface is the network interface of a running network.
type Interface interface {
	Device() wgtun.Device
	Set(tun.Spec) (name string, err error)
	Close() error
}

// Config configures a Manager.
type Config struct {
	Mesh      Mesh
	Log       *log.Logger
	StatePath string // holds this identity's private key and what the node settled on
	// Open sets up the interface; the network cannot run when it is nil.
	Open func(tun.Spec) (Interface, error)
	// Networks returns the networks of this machine, without the
	// interface called skip; LocalNetworks when nil.
	Networks func(skip string) Networks
	// DNS is the UDP address to answer on; no DNS when empty.
	DNS string
	// Echo is the UDP port on this machine's address that answers Ping;
	// nothing answers when it is 0.
	Echo int
}

// state is what a node keeps between runs.
type state struct {
	Node       string `json:"node"` // the identity that the key and the address belong to
	PrivateKey string `json:"private_key"`
	Subnet     string `json:"subnet,omitempty"`
	Since      int64  `json:"since,omitempty"`
	Address    string `json:"address,omitempty"`
}

type settings struct {
	net     *config.Net
	exposes []config.Expose
}

// Manager keeps the network as the configuration and the mesh say.
type Manager struct {
	cfg     Config
	exposes expose.Manager
	config  atomic.Pointer[settings]
	wake    chan chan struct{} // for Run to look now, and to close the channel then
	status  atomic.Pointer[wire.NetState]

	running atomic.Bool
	ended   chan struct{} // closed when Run returns

	// Owned by Run.
	state    state
	loaded   bool
	bind     *bind
	iface    Interface
	loop     *loopback // the interface as WireGuard sees it
	name     string
	dev      *device.Device
	peers    map[string]peer // by public key; replaced, never changed
	stopDNS  context.CancelFunc
	echoing  string // the address that answers Ping
	stopEcho context.CancelFunc

	live    atomic.Pointer[wireguard] // for Status
	dropped atomic.Uint64             // packets lost on the way to WireGuard
}

// wireguard is the device and what it was told, while it runs.
type wireguard struct {
	dev   *device.Device
	peers map[string]peer
}

// peer is a peer that WireGuard knows.
type peer struct {
	node    string
	address string
}

// Dropped counts the packets lost on the way to WireGuard, as it could not
// keep up.
func (m *Manager) Dropped() uint64 { return m.dropped.Load() }

// New returns a manager; Run does the work.
func New(cfg Config) *Manager {
	if cfg.Networks == nil {
		cfg.Networks = LocalNetworks
	}
	return &Manager{cfg: cfg, wake: make(chan chan struct{}), ended: make(chan struct{}), peers: map[string]peer{}}
}

// Configure sets the chosen subnet (which may be nil) and exposed ports.
// Once Run runs, it waits until Run acted on c, so that Status tells how
// that went.
func (m *Manager) Configure(ctx context.Context, c *config.Net, rules ...config.Expose) {
	m.config.Store(&settings{net: c, exposes: slices.Clone(rules)})
	if !m.running.Load() {
		return
	}
	done := make(chan struct{})
	select {
	case m.wake <- done:
	case <-m.ended:
	case <-ctx.Done():
	}
	select {
	case <-done:
	case <-m.ended:
	case <-ctx.Done():
	}
}

// Status reports the network; nil when it cannot run here.
func (m *Manager) Status() *wire.NetState {
	st := m.status.Load()
	if st == nil {
		return nil
	}
	now := *st
	now.Peers = []wire.NetPeer{}
	if live := m.live.Load(); live != nil {
		now.Peers = m.peerStates(live)
	}
	return &now
}

// Networks reports what the subnet must not overlap on this machine.
func (m *Manager) Networks() wire.Networks {
	n := m.cfg.Networks(m.interfaceName())
	list := wire.Networks{Interfaces: []string{}, Routes: []string{}}
	for _, p := range n.Interfaces {
		list.Interfaces = append(list.Interfaces, p.String())
	}
	for _, p := range n.Routes {
		list.Routes = append(list.Routes, p.String())
	}
	return list
}

func (m *Manager) interfaceName() string {
	if st := m.status.Load(); st != nil {
		return st.Interface
	}
	return ""
}

// Run keeps the network going until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	m.running.Store(true)
	defer close(m.ended)
	defer m.down()
	tick := time.NewTicker(retryInterval)
	defer tick.Stop()
	for {
		changed := m.cfg.Mesh.Changed()
		m.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case done := <-m.wake:
			m.reconcile(ctx)
			close(done)
		case <-tick.C:
		}
	}
}

func (m *Manager) reconcile(ctx context.Context) {
	self := m.cfg.Mesh.Self()
	cfg := m.config.Load()
	if cfg == nil {
		cfg = &settings{}
	}
	ready := false
	defer func() {
		address := ""
		if ready {
			address = m.state.Address
		}
		m.exposes.Apply(ctx, address, cfg.exposes)
	}()
	if m.cfg.Open == nil || self.ID == "" {
		return
	}
	st := &wire.NetState{}
	defer func() {
		st.Interface = m.name
		st.Subnet, st.Address = m.state.Subnet, m.state.Address
		m.status.Store(st)
	}()
	if err := m.load(self.ID); err != nil {
		st.Error = err.Error()
		return
	}
	topo := m.cfg.Mesh.Topology()
	if err := m.settle(ctx, cfg.net, self, topo); err != nil {
		st.Error = err.Error()
		return
	}
	err := m.up(ctx)
	if err == nil {
		// Machines learn of this one once it can answer them.
		m.cfg.Mesh.SetNet(&wire.NetInfo{Subnet: m.state.Subnet, Since: m.state.Since, Address: m.state.Address, PublicKey: m.publicKey()})
		topo = m.cfg.Mesh.Topology()
		err = m.apply(self, topo)
	}
	if err == nil {
		ready = true
		err = m.answerPings(ctx)
	}
	if err == nil {
		err = m.conflict()
	}
	if m.iface == nil {
		m.cfg.Mesh.SetNet(nil)
	}
	if err != nil {
		st.Error = err.Error()
		var hinted *hintError
		if errors.As(err, &hinted) {
			st.Hint = hinted.hint
		}
	}
}

// hintError is an error with what to do about it, for wire.NetState.
type hintError struct {
	text, hint string
}

func (e *hintError) Error() string { return e.text }

// conflict says what on this machine takes addresses of the subnet.
func (m *Manager) conflict() error {
	subnet, err := netip.ParsePrefix(m.state.Subnet)
	if err != nil {
		return nil
	}
	if p, found := conflict(subnet, m.cfg.Networks(m.name)); found {
		return &hintError{fmt.Sprintf("the subnet %s overlaps %s on this machine", subnet, p), "choose another subnet with `gst subnet <cidr>`"}
	}
	return nil
}

// load reads the state of identity id. The private key and the address
// belong to one identity: records of the old one stay around for a while,
// and two records with one key or address would be taken for one machine.
// So a new identity gets its own, and keeps only the subnet, which belongs
// to the network.
func (m *Manager) load(id string) error {
	if !m.loaded {
		data, err := os.ReadFile(m.cfg.StatePath)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("read the state of the network: %w", err)
		}
		if err == nil && json.Unmarshal(data, &m.state) != nil {
			m.state = state{}
		}
	}
	key, err := hex.DecodeString(m.state.PrivateKey)
	if m.state.Node == id && err == nil && len(key) == 32 {
		m.loaded = true
		return nil
	}
	var private [32]byte
	if _, err := rand.Read(private[:]); err != nil {
		return err
	}
	private[0] &= 248
	private[31] = private[31]&127 | 64
	old := m.state
	m.state.Node, m.state.PrivateKey, m.state.Address = id, hexKey(private), ""
	if err := m.save(); err != nil {
		m.state = old // For the next look to try again, with WireGuard too.
		return err
	}
	m.loaded = true
	if m.dev != nil {
		if err := m.dev.IpcSet("private_key=" + m.state.PrivateKey + "\nreplace_peers=true\n"); err != nil {
			m.down()
			return fmt.Errorf("configure WireGuard: %w", err)
		}
		m.peers = map[string]peer{}
	}
	return nil
}

func (m *Manager) save() error {
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.cfg.StatePath), 0o700); err != nil {
		return err
	}
	tmp := m.cfg.StatePath + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write the state of the network: %w", err)
	}
	return os.Rename(tmp, m.cfg.StatePath)
}

func (m *Manager) publicKey() string {
	private, _ := hex.DecodeString(m.state.PrivateKey)
	public, _ := curve25519.X25519(private, curve25519.Basepoint)
	return hex.EncodeToString(public)
}

// settle works out the subnet and this node's address.
func (m *Manager) settle(ctx context.Context, c *config.Net, self wire.NodeInfo, topo wire.Topology) error {
	subnet, since := m.state.Subnet, m.state.Since
	consider := func(s string, t int64) {
		if s != "" && (subnet == "" || newer(s, t, subnet, since)) {
			subnet, since = s, t
		}
	}
	if c != nil {
		consider(c.Subnet, c.Since)
	}
	for _, n := range topo.Nodes {
		if n.Net != nil && n.ID != self.ID {
			consider(n.Net.Subnet, n.Net.Since)
		}
	}
	if subnet == "" {
		chosen, err := m.chooseSubnet(ctx, self, topo)
		if err != nil {
			return err
		}
		// Negative, so that it yields to any choice of a person, and among
		// automatic choices the network that came first stays.
		subnet, since = chosen.String(), -time.Now().UnixNano()
	}
	prefix, err := config.ParseSubnet(subnet)
	if err != nil {
		return err
	}

	taken := map[netip.Addr]bool{}
	mine, err := netip.ParseAddr(m.state.Address)
	keep := err == nil && prefix.Contains(mine)
	for _, n := range topo.Nodes {
		if n.Net == nil || n.ID == self.ID || n.Net.Subnet != subnet {
			continue
		}
		a, err := netip.ParseAddr(n.Net.Address)
		if err != nil {
			continue
		}
		taken[a] = true
		if a == mine && n.ID < self.ID {
			keep = false
		}
	}
	address := m.state.Address
	if !keep {
		a, err := chooseAddress(self.ID, prefix, taken)
		if err != nil {
			return err
		}
		address = a.String()
	}
	if subnet == m.state.Subnet && since == m.state.Since && address == m.state.Address {
		return nil
	}
	m.state.Subnet, m.state.Since, m.state.Address = subnet, since, address
	return m.save()
}

// chooseSubnet finds a subnet that conflicts with nothing on the machines
// that answer now, for the first node of the network.
func (m *Manager) chooseSubnet(ctx context.Context, self wire.NodeInfo, topo wire.Topology) (netip.Prefix, error) {
	used := []Networks{m.cfg.Networks("")}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range topo.Nodes {
		if n.ID == self.ID || !n.Reachable {
			continue
		}
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, askTimeout)
			defer cancel()
			var networks wire.Networks
			if m.cfg.Mesh.Call(ctx, n.ID, wire.MethodNetNetworks, nil, &networks) == nil {
				mu.Lock()
				used = append(used, usedNetworks(networks))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	subnet, ok := chooseSubnet(self.ID, used)
	if !ok {
		return netip.Prefix{}, &hintError{"no subnet is free on the machines of the network", "choose one with `gst subnet <cidr>`"}
	}
	return subnet, nil
}

// wanted is the interface as it should be now.
func (m *Manager) wanted(topo wire.Topology) tun.Spec {
	hosts := map[string]string{}
	for _, name := range names(topo) {
		// What the other machines call themselves must not stop this one.
		if a, ok := lookup(topo, name); ok && tun.ValidHost(name+".gst") && len(hosts) < tun.MaxHosts {
			hosts[name+".gst"] = a.String()
		}
	}
	return tun.Spec{Subnet: m.state.Subnet, Address: m.state.Address, Hosts: hosts}
}

// names lists what the machines of topo are called.
func names(topo wire.Topology) []string {
	var list []string
	for _, n := range topo.Nodes {
		list = append(list, n.Name)
	}
	slices.Sort(list)
	return slices.Compact(list)
}

// lookup finds the address of the machine that ref names, if it has one
// in the subnet of this machine: while a new subnet spreads, the others
// may be on the old one.
func lookup(topo wire.Topology, ref string) (netip.Addr, bool) {
	n, err := mesh.FindNode(topo, ref)
	if err != nil || n.Net == nil {
		return netip.Addr{}, false
	}
	self := slices.IndexFunc(topo.Nodes, func(v wire.TopoNode) bool { return v.ID == topo.Self })
	if self < 0 || topo.Nodes[self].Net == nil || topo.Nodes[self].Net.Subnet != n.Net.Subnet {
		return netip.Addr{}, false
	}
	a, err := netip.ParseAddr(n.Net.Address)
	return a, err == nil
}

// apply makes the interface and WireGuard match topo.
func (m *Manager) apply(self wire.NodeInfo, topo wire.Topology) error {
	m.loop.setAddress(m.state.Address)
	name, err := m.iface.Set(m.wanted(topo))
	if err != nil {
		m.down()
		return fmt.Errorf("the network helper failed: %w", err)
	}
	m.name = name
	return m.syncPeers(self, topo)
}

// up starts the interface and the WireGuard device, unless they run.
func (m *Manager) up(ctx context.Context) error {
	if m.dev != nil {
		select {
		case <-m.dev.Wait():
			m.down() // Its interface went away.
		default:
		}
	}
	if m.iface != nil {
		return nil
	}
	iface, err := m.cfg.Open(tun.Spec{Subnet: m.state.Subnet, Address: m.state.Address})
	if err != nil {
		if errors.Is(err, tun.ErrNoHelper) {
			return &hintError{"the network helper is not running", "run `gst up` on this machine"}
		}
		return fmt.Errorf("set up the interface: %w", err)
	}
	m.iface = iface
	m.name, _ = iface.Device().Name()
	m.bind = newBind(m.cfg.Mesh.SendPacket, &m.dropped)
	m.loop = &loopback{Device: iface.Device()}
	m.loop.setAddress(m.state.Address)
	m.dev = device.NewDevice(m.loop, m.bind, device.NewLogger(device.LogLevelSilent, ""))
	// An endpoint is a node id, set from the records, and stays right
	// wherever a machine moves. The node that a packet seems to come from
	// is less sure: two machines behind one NAT can have the same source
	// on the shared UDP socket.
	m.dev.DisableSomeRoamingForBrokenMobileSemantics()
	if err := m.dev.IpcSet("private_key=" + m.state.PrivateKey + "\n"); err != nil {
		m.down()
		return fmt.Errorf("start WireGuard: %w", err)
	}
	m.cfg.Mesh.HandlePackets(m.bind.deliver)
	if err := m.dev.Up(); err != nil {
		m.down()
		return fmt.Errorf("start WireGuard: %w", err)
	}
	if m.cfg.DNS != "" {
		conn, err := net.ListenPacket("udp", m.cfg.DNS)
		if err != nil {
			m.down()
			return fmt.Errorf("answer for .gst: %w", err)
		}
		dnsCtx, stop := context.WithCancel(ctx)
		m.stopDNS = stop
		go serveDNS(dnsCtx, conn, func(name string) (netip.Addr, bool) { return lookup(m.cfg.Mesh.Topology(), name) })
	}
	m.cfg.Log.Printf("virtual network: up on %s", m.name)
	return nil
}

// down stops everything that up started.
func (m *Manager) down() {
	m.exposes.Close()
	if m.stopDNS != nil {
		m.stopDNS()
		m.stopDNS = nil
	}
	m.stopAnswering()
	m.cfg.Mesh.HandlePackets(nil)
	if m.dev != nil {
		m.dev.Close()
		m.dev = nil
		m.cfg.Log.Print("virtual network: down")
	}
	if m.iface != nil {
		m.iface.Close()
		m.iface = nil
	}
	m.name = ""
	m.peers = map[string]peer{}
	m.live.Store(nil)
}

// syncPeers makes WireGuard know the other machines of the network.
func (m *Manager) syncPeers(self wire.NodeInfo, topo wire.Topology) error {
	if m.dev == nil {
		return nil
	}
	want := map[string]peer{}
	for _, n := range topo.Nodes {
		if n.Net != nil && n.ID != self.ID && n.Net.Subnet == m.state.Subnet {
			want[n.Net.PublicKey] = peer{n.ID, n.Net.Address}
		}
	}
	var config strings.Builder
	for key := range m.peers {
		if _, ok := want[key]; !ok {
			fmt.Fprintf(&config, "public_key=%s\nremove=true\n", key)
		}
	}
	for key, p := range want {
		if m.peers[key] != p {
			fmt.Fprintf(&config, "public_key=%s\nreplace_allowed_ips=true\nallowed_ip=%s/32\nendpoint=%s\n", key, p.address, p.node)
		}
	}
	if config.Len() > 0 {
		if err := m.dev.IpcSet(config.String()); err != nil {
			return fmt.Errorf("configure WireGuard: %w", err)
		}
	}
	m.peers = want
	m.live.Store(&wireguard{m.dev, want})
	return nil
}

// peerStates reports the peers as WireGuard and the mesh know them.
func (m *Manager) peerStates(live *wireguard) []wire.NetPeer {
	type counters struct {
		handshake time.Time
		rx, tx    uint64
	}
	seen := map[string]*counters{}
	if text, err := live.dev.IpcGet(); err == nil {
		var at *counters
		lines := bufio.NewScanner(strings.NewReader(text))
		for lines.Scan() {
			key, value, _ := strings.Cut(lines.Text(), "=")
			switch key {
			case "public_key":
				at = &counters{}
				seen[value] = at
			case "last_handshake_time_sec":
				if s, err := strconv.ParseInt(value, 10, 64); err == nil && s > 0 && at != nil {
					at.handshake = time.Unix(s, 0)
				}
			case "rx_bytes":
				if at != nil {
					at.rx, _ = strconv.ParseUint(value, 10, 64)
				}
			case "tx_bytes":
				if at != nil {
					at.tx, _ = strconv.ParseUint(value, 10, 64)
				}
			}
		}
	}
	topo := m.cfg.Mesh.Topology()
	list := []wire.NetPeer{}
	for key, p := range live.peers {
		i := slices.IndexFunc(topo.Nodes, func(n wire.TopoNode) bool { return n.ID == p.node })
		if i < 0 || !topo.Nodes[i].Reachable && !topo.Nodes[i].Direct {
			continue // Only a record that has not expired yet.
		}
		out := wire.NetPeer{Node: p.node, Name: topo.Nodes[i].Name, Address: p.address, Path: m.cfg.Mesh.PacketPath(p.node), Endpoint: m.cfg.Mesh.PacketEndpoint(p.node)}
		if c := seen[key]; c != nil {
			out.Handshake, out.Received, out.Sent = c.handshake, c.rx, c.tx
		}
		list = append(list, out)
	}
	slices.SortFunc(list, func(a, b wire.NetPeer) int { return strings.Compare(a.Name, b.Name) })
	return list
}

// Exposes reports the local port forwards, independently of network health.
func (m *Manager) Exposes() []wire.ExposeState { return m.exposes.Status() }
