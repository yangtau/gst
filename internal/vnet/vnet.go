// Package vnet runs this node's part in the virtual network, which is
// how nodes of the mesh reach each other: each gets an IP address in one
// private subnet and a name under .gst, and programs reach them through a
// network interface, with any protocol. A WireGuard device (wireguard-go)
// encrypts the packets end to end, and the mesh carries them, over UDP
// where the network allows and along its SSH links where not (bind.go).
//
// There is no coordinator. The network's table (mesh) holds the subnet of
// the whole network, and each node publishes its address and WireGuard key
// in its own entry; every node works out the same result from the table:
// of two nodes that picked the same address, the one with the lower id
// keeps it. The mesh already says which nodes are in the network, and it is
// what makes the keys in the table trustworthy.
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
	askTimeout    = 3 * time.Second // for a node to report its used prefixes
)

// Mesh is what the network needs of its node; *mesh.Node has it.
type Mesh interface {
	Self() wire.NodeInfo
	Topology() wire.Topology
	Changed() <-chan struct{}
	SetNet(*wire.NodeNet)
	Subnet() (wire.SubnetValue, bool)
	SetSubnet(subnet string, manual bool) error
	SendPacket(node string, packet []byte)
	HandlePackets(func(node string, packet []byte))
	PacketWay(node string) string
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
	StatePath string // holds this identity's private key and its address
	// Open sets up the interface; the network cannot run when it is nil.
	Open func(tun.Spec) (Interface, error)
	// UsedPrefixes returns the prefixes in use on this machine, without the
	// interface called skip; LocalPrefixes when nil.
	UsedPrefixes func(skip string) UsedPrefixes
	// DNS is the UDP address to answer on; no DNS when empty.
	DNS string
	// Echo is the UDP port on this node's address that answers Ping;
	// nothing answers when it is 0.
	Echo int
	// Inside answers on DNS and on Echo at the packets of the interface
	// rather than on sockets, for a system that keeps this process's
	// sockets off the interface (see inside.go). DNS is then an address
	// that the interface routes.
	Inside bool
}

// state is what a node keeps between runs.
type state struct {
	Node       string `json:"node"` // the identity that the key and the address belong to
	PrivateKey string `json:"private_key"`
	Address    string `json:"address,omitempty"`
}

// Manager keeps the network as the configuration and the mesh say.
type Manager struct {
	cfg     Config
	exposes expose.Manager
	config  atomic.Pointer[[]config.Expose]
	wake    chan chan struct{} // for Run to look now, and to close the channel then
	status  atomic.Pointer[wire.NetState]

	running atomic.Bool
	ended   chan struct{} // closed when Run returns

	// Owned by Run.
	state    state
	subnet   string // of the network, as the table has it
	loaded   bool
	bind     *bind
	iface    Interface
	loop     *loopback // the interface as WireGuard sees it
	name     string
	dev      *device.Device
	peers    map[string]peer // by public key; replaced, never changed
	echoing  string          // the address that answers Ping
	stopEcho context.CancelFunc

	live    atomic.Pointer[wireguard] // for Status
	inside  atomic.Pointer[inside]    // for Ping, while the network runs with Config.Inside
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
	if cfg.UsedPrefixes == nil {
		cfg.UsedPrefixes = LocalPrefixes
	}
	return &Manager{cfg: cfg, wake: make(chan chan struct{}), ended: make(chan struct{}), peers: map[string]peer{}}
}

// Configure sets the exposed ports. Once Run runs, it waits until Run
// acted on them, so that Status tells how that went.
func (m *Manager) Configure(ctx context.Context, rules ...config.Expose) {
	rules = slices.Clone(rules)
	m.config.Store(&rules)
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

// UsedPrefixes reports what the subnet must not overlap on this machine.
func (m *Manager) UsedPrefixes() wire.UsedPrefixes {
	n := m.cfg.UsedPrefixes(m.interfaceName())
	list := wire.UsedPrefixes{Interfaces: []string{}, Routes: []string{}}
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

// Run keeps the network going until ctx is done. It answers for .gst all
// along, so that the system's resolver hears an answer while the network
// is down too.
func (m *Manager) Run(ctx context.Context) {
	m.running.Store(true)
	defer close(m.ended)
	defer m.down()
	dns, logged := m.cfg.DNS != "" && !m.cfg.Inside, false
	tick := time.NewTicker(retryInterval)
	defer tick.Stop()
	for {
		if dns {
			// Until it binds, which another program may keep it from.
			if conn, err := net.ListenPacket("udp", m.cfg.DNS); err == nil {
				go serveDNS(ctx, conn, m.resolve)
				dns = false
			} else if !logged {
				m.cfg.Log.Printf("virtual network: no answers for .gst yet: %v", err)
				logged = true
			}
		}
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
	var exposes []config.Expose
	if rules := m.config.Load(); rules != nil {
		exposes = *rules
	}
	ready := false
	defer func() {
		address := ""
		if ready {
			address = m.state.Address
		}
		m.exposes.Apply(ctx, address, exposes)
	}()
	if m.cfg.Open == nil || self.ID == "" {
		return
	}
	st := &wire.NetState{}
	defer func() {
		st.Interface = m.name
		st.Subnet, st.Address = m.subnet, m.state.Address
		m.status.Store(st)
	}()
	if err := m.load(self.ID); err != nil {
		st.Error = err.Error()
		return
	}
	topo := m.cfg.Mesh.Topology()
	if err := m.settle(ctx, self, topo); err != nil {
		st.Error = err.Error()
		return
	}
	err := m.up(ctx)
	if err == nil {
		// Nodes learn of this one once it can answer them.
		m.cfg.Mesh.SetNet(&wire.NodeNet{Address: m.state.Address, PublicKey: m.publicKey()})
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
	subnet, err := netip.ParsePrefix(m.subnet)
	if err != nil {
		return nil
	}
	if p, found := conflict(subnet, m.cfg.UsedPrefixes(m.name)); found {
		return &hintError{fmt.Sprintf("the subnet %s overlaps %s on this machine", subnet, p), "choose another subnet with `gst subnet <cidr>`"}
	}
	return nil
}

// load reads the state of identity id. The private key and the address
// belong to one identity: a new identity is a new node, and two
// entries with one key or address would be taken for one node. So a
// new identity gets its own.
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
	if err := config.WriteFile(m.cfg.StatePath, append(data, '\n')); err != nil {
		return fmt.Errorf("write the state of the network: %w", err)
	}
	return nil
}

func (m *Manager) publicKey() string {
	private, _ := hex.DecodeString(m.state.PrivateKey)
	public, _ := curve25519.X25519(private, curve25519.Basepoint)
	return hex.EncodeToString(public)
}

// settle works out the subnet and this node's address. The subnet is the
// network's, from the table; the first node of a network chooses one. Of
// two nodes that hold the same address, offline or not, the one with
// the lower id keeps it.
func (m *Manager) settle(ctx context.Context, self wire.NodeInfo, topo wire.Topology) error {
	network, ok := m.cfg.Mesh.Subnet()
	if !ok {
		chosen, err := m.chooseSubnet(ctx, self, topo)
		if err != nil {
			return err
		}
		if err := m.cfg.Mesh.SetSubnet(chosen.String(), false); err != nil {
			return err
		}
		// Another choice may have arrived in the meantime, and won.
		if network, ok = m.cfg.Mesh.Subnet(); !ok {
			return errors.New("the subnet of the network is unknown")
		}
	}
	prefix, err := config.ParseSubnet(network.Subnet)
	if err != nil {
		return err
	}
	m.subnet = prefix.String()

	taken := map[netip.Addr]bool{}
	mine, err := netip.ParseAddr(m.state.Address)
	keep := err == nil && prefix.Contains(mine)
	for _, n := range topo.Nodes {
		if n.Net == nil || n.ID == self.ID {
			continue
		}
		a, err := netip.ParseAddr(n.Net.Address)
		if err != nil || !prefix.Contains(a) {
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
	if address == m.state.Address {
		return nil
	}
	m.state.Address = address
	return m.save()
}

// chooseSubnet finds a subnet that conflicts with nothing on the machines
// that answer now, for the first node of the network.
func (m *Manager) chooseSubnet(ctx context.Context, self wire.NodeInfo, topo wire.Topology) (netip.Prefix, error) {
	used := []UsedPrefixes{m.cfg.UsedPrefixes("")}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range topo.Nodes {
		if n.ID == self.ID || !n.Reachable {
			continue
		}
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, askTimeout)
			defer cancel()
			var prefixes wire.UsedPrefixes
			if m.cfg.Mesh.Call(ctx, n.ID, wire.MethodNetPrefixes, nil, &prefixes) == nil {
				mu.Lock()
				used = append(used, parseUsedPrefixes(prefixes))
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
		// What the other nodes call themselves must not stop this one.
		if a, ok := lookup(topo, name); ok && tun.ValidHost(name+".gst") && len(hosts) < tun.MaxHosts {
			hosts[name+".gst"] = a.String()
		}
	}
	return tun.Spec{Subnet: m.subnet, Address: m.state.Address, Hosts: hosts}
}

// names lists what the nodes of topo are called.
func names(topo wire.Topology) []string {
	var list []string
	for _, n := range topo.Nodes {
		list = append(list, n.Name)
	}
	slices.Sort(list)
	return slices.Compact(list)
}

// lookup finds the address of the node that ref names, if it has one
// in the subnet of the network: while a new subnet spreads, or while a
// node is offline since before it, it may have one in the old.
func lookup(topo wire.Topology, ref string) (netip.Addr, bool) {
	n, err := mesh.FindNode(topo, ref)
	if err != nil || n.Net == nil {
		return netip.Addr{}, false
	}
	subnet, err := netip.ParsePrefix(topo.Subnet)
	a, err2 := netip.ParseAddr(n.Net.Address)
	return a, err == nil && err2 == nil && subnet.Contains(a)
}

// resolve answers the DNS: the address of the node called name, and
// whether the network runs, without which there is no answer.
func (m *Manager) resolve(name string) (netip.Addr, bool, bool) {
	if m.live.Load() == nil {
		return netip.Addr{}, false, false
	}
	a, ok := lookup(m.cfg.Mesh.Topology(), name)
	return a, ok, true
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
	iface, err := m.cfg.Open(tun.Spec{Subnet: m.subnet, Address: m.state.Address})
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
	var seen wgtun.Device = m.loop
	if m.cfg.Inside {
		dns, _ := netip.ParseAddrPort(m.cfg.DNS)
		in := newInside(m.loop, m.loop.own, dns, uint16(m.cfg.Echo), m.resolve)
		m.inside.Store(in)
		seen = in
	}
	m.dev = device.NewDevice(seen, m.bind, device.NewLogger(device.LogLevelSilent, ""))
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
	m.cfg.Log.Printf("virtual network: up on %s", m.name)
	return nil
}

// down stops everything that up started.
func (m *Manager) down() {
	m.exposes.Close()
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
	m.inside.Store(nil)
}

// syncPeers makes WireGuard know the other nodes of the network.
func (m *Manager) syncPeers(self wire.NodeInfo, topo wire.Topology) error {
	if m.dev == nil {
		return nil
	}
	// Of the nodes that hold one address, as offline ones may long
	// after a merge, the lower id has it, as in settle; this node's
	// own is its own.
	holder := map[netip.Addr]wire.TopoNode{}
	for _, n := range topo.Nodes {
		a, ok := lookup(topo, n.ID)
		if !ok || n.ID == self.ID || a.String() == m.state.Address {
			continue
		}
		if h, taken := holder[a]; !taken || n.ID < h.ID {
			holder[a] = n
		}
	}
	want := map[string]peer{}
	for a, n := range holder {
		want[n.Net.PublicKey] = peer{n.ID, a.String()}
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
		if i < 0 {
			continue
		}
		out := wire.NetPeer{Node: p.node, Name: topo.Nodes[i].Name, Address: p.address, Way: m.cfg.Mesh.PacketWay(p.node), Endpoint: m.cfg.Mesh.PacketEndpoint(p.node)}
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
