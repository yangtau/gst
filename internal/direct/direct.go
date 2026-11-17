// Package direct finds UDP paths between gst daemons, when the network
// between them allows one. A daemon has one Transport: one wildcard socket
// per IP family, on one port, that all its paths share. The daemons exchange
// offers over SSH through the mesh: an offer names the addresses to try and
// a token. Each side then probes the other's addresses, and answers probes.
// Where a probe comes from is a further address to try, so a machine behind
// NAT needs no one to tell it its public address, as long as the other side
// can be reached; an answer also says where its probe came from, which tells
// a machine that address, for its offers to others.
//
// Packets go the way the system's routes say, but around VPNs and proxies:
// on macOS and Linux the sockets send through the interface that reaches
// the internet, because a proxy that takes the routes sends UDP again from
// another port, which holes in firewalls do not fit (see bind_darwin.go and
// bind_linux.go), and the addresses of tunnels are not offered. The address
// they leave from is this package's, though, not the system's: a firewall
// or NAT between the machines lets through only what comes from the address
// that it saw before, and a machine with several addresses, such as temporary
// IPv6 ones, may pick another for any packet. So an answer leaves from the
// address its probe came to, and a path sends from the address that an
// answer came to: the two make a pair, an address of this machine and one
// of the other side's.
//
// The system's pick may also be an address that nothing returns to, and it
// is the same at every try. So until an address of the other side answers,
// the probes leave from each address of this machine besides: only an
// answer makes a pair, and the system's pick is one try among them.
//
// A pair carries packets once the two sides confirmed it to each other, in
// three steps (see Path): a side sends only on a pair that carried packets
// both ways and that the other side knows of. One side's probes suffice
// for both, as the other takes the pair from them.
//
// Each side sends on the fastest of the pairs that both confirmed, so the
// two directions may differ. The path then carries the packets of the
// virtual network, which WireGuard encrypts and authenticates. Nothing
// here protects them: the token only keeps stray packets out.
package direct

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Port is the UDP port a daemon asks for. Several users of a machine each
// have a daemon, so a daemon that does not get it takes any other; offers
// tell the port every time.
const Port = 15355

const (
	socketBuffer = 4 << 20 // in bytes; a frame of video arrives as a burst of hundreds of packets
	maxPacket    = 1 << 16
	maxPublic    = 4                // addresses that others saw this machine at, kept for offers
	maxSeen      = 4                // addresses that the SSH links show this machine at, kept for offers
	publicLife   = 10 * time.Minute // for how long such an address counts
	maxRTT       = 10 * time.Second // an answer that took longer is no measure
	watchEvery   = 5 * time.Second  // how often the machine's network is checked

	tokenLen = 16
	idLen    = 8
	addrLen  = 18 // an IP address as 16 bytes and a port
)

// The first byte of the packets of this package, then the token. WireGuard
// uses 1 to 4, for packets that this package hands on.
const (
	kindProbe  = 0x11 // token, sender's id, time, address it goes to, flags; asks for an answer
	kindAnswer = 0x12 // token, the probe's time, address, and flags, where the probe came from
	kindBye    = 0x13 // token; the path ends now
)

// flagWorks, in a probe, says that the pair of addresses that it goes
// between works both ways: an answer of the other side came back there. See
// Path for what the two sides make of it.
const flagWorks = 1

const (
	echoLen   = 8 + addrLen + 1 // what an answer repeats of its probe
	probeLen  = 1 + tokenLen + idLen + echoLen
	answerLen = 1 + tokenLen + echoLen + addrLen
)

// Network is this machine's network, as UDP paths see it.
type Network struct {
	// Addresses returns the addresses to offer.
	Addresses func() []netip.Addr
	// Interfaces returns the interfaces for the sockets to send through,
	// for IPv4 and for IPv6, 0 where the routes decide; when it is nil, the
	// routes decide all, as for tests on the loopback interface, which a
	// socket bound to another cannot reach on Linux.
	Interfaces func() (v4, v6 int)
	// Has reports whether an interface of this machine has addr; when it
	// is nil, none has.
	Has func(addr netip.Addr) bool
}

// System returns this machine's network: its addresses, and on macOS and
// Linux the interfaces that reach the internet around any VPN or proxy.
func System() Network {
	return Network{
		Addresses:  Addresses,
		Interfaces: func() (v4, v6 int) { return lookAtInterfaces().defaultInterfaces() },
		Has:        func(addr netip.Addr) bool { return lookAtInterfaces().of(addr) != nil },
	}
}

// Transport is this daemon's end of all its UDP paths.
type Transport struct {
	network Network
	skip    func(netip.Addr) bool
	v4      *socket
	v6      *socket
	id      [idLen]byte // tells this daemon's probes, which may reach itself
	start   time.Time   // what the times in probes count from

	sweepEvery time.Duration // how often a path measures all its addresses

	mu      sync.Mutex
	tokens  map[[tokenLen]byte]*Path
	public  map[netip.AddrPort]time.Time // this machine as others see it
	seen    []netip.Addr                 // this machine as its SSH links show it; replaced, never changed
	closed  bool
	done    chan struct{}                            // closed by Close
	changed chan struct{}                            // for Changed
	moved   chan struct{}                            // for Moved
	known   []netip.Addr                             // what Addresses said last; owned by watch
	place   place                                    // where the machine was last; owned by watch
	locals  atomic.Pointer[[]netip.Addr]             // the same, for the paths to probe from; replaced, never changed
	strays  atomic.Uint64                            // WireGuard packets from unknown sources
	sources atomic.Pointer[map[netip.AddrPort]*Path] // by the addresses that proved themselves; replaced, never changed
}

type socket struct {
	udp     *net.UDPConn
	network string
	port    uint16
	bound   atomic.Int64 // the interface it sends through; 0 when the routes decide
}

// controlSize is room for the control message that says where a packet
// arrived.
var controlSize = max(len(ipv4.NewControlMessage(ipv4.FlagDst)), len(ipv6.NewControlMessage(ipv6.FlagDst)))

// control returns the control message that makes a packet leave from local;
// nil, which leaves it to the system, when local is not valid. It names no
// interface: on macOS the socket's own decides.
func control(local netip.Addr) []byte {
	switch {
	case !local.IsValid():
		return nil
	case local.Is4():
		return (&ipv4.ControlMessage{Src: local.AsSlice()}).Marshal()
	default:
		return (&ipv6.ControlMessage{Src: local.AsSlice()}).Marshal()
	}
}

// write sends packet to addr, from the address that oob names.
func (s *socket) write(packet, oob []byte, to netip.AddrPort) error {
	_, _, err := s.udp.WriteMsgUDPAddrPort(packet, oob, to)
	return err
}

// arrivedAt reads the address of this machine that a packet came to from
// its control message; not valid when the system did not say.
func (s *socket) arrivedAt(oob []byte) netip.Addr {
	var ip net.IP
	if s.network == "udp4" {
		var cm ipv4.ControlMessage
		if cm.Parse(oob) == nil {
			ip = cm.Dst
		}
	} else {
		var cm ipv6.ControlMessage
		if cm.Parse(oob) == nil {
			ip = cm.Dst
		}
	}
	addr, _ := netip.AddrFromSlice(ip)
	if addr = addr.Unmap(); addr.IsUnspecified() {
		return netip.Addr{}
	}
	return addr
}

// Listen opens the sockets, on port if it is free, on network. skip says
// which addresses to have nothing to do with, in offers, in what the other
// side offers, and as the source of a packet: those of the virtual network,
// whose packets would take the path that they make up.
func Listen(port int, network Network, skip func(netip.Addr) bool) (*Transport, error) {
	t := &Transport{
		network:    network,
		skip:       skip,
		start:      time.Now(),
		sweepEvery: sweepInterval,
		tokens:     map[[tokenLen]byte]*Path{},
		public:     map[netip.AddrPort]time.Time{},
		done:       make(chan struct{}),
		changed:    make(chan struct{}, 1),
		moved:      make(chan struct{}, 1),
	}
	rand.Read(t.id[:])
	t.sources.Store(&map[netip.AddrPort]*Path{})
	t.locals.Store(&[]netip.Addr{})
	if t.v4 = listen("udp4", port); t.v4 != nil {
		port = int(t.v4.port)
	}
	t.v6 = listen("udp6", port)
	if t.v4 == nil && t.v6 == nil {
		return nil, errors.New("no UDP socket")
	}
	t.bind()
	t.addressesChanged()
	t.place = placeOf(t.known)
	go t.watch()
	for _, s := range []*socket{t.v4, t.v6} {
		if s != nil {
			go t.read(s)
		}
	}
	return t, nil
}

// bind points the sockets at the interfaces that reach the internet now,
// and reports whether that changed anything.
func (t *Transport) bind() bool {
	if t.network.Interfaces == nil {
		return false
	}
	changed := false
	v4, v6 := t.network.Interfaces()
	for _, b := range []struct {
		s     *socket
		index int
	}{{t.v4, v4}, {t.v6, v6}} {
		if b.s == nil || b.s.bound.Load() == int64(b.index) {
			continue
		}
		c, err := b.s.udp.SyscallConn()
		if err == nil {
			err = bindSocket(c, b.s.network, b.index)
		}
		if err == nil {
			b.s.bound.Store(int64(b.index))
			changed = true
		}
	}
	return changed
}

// addressesChanged reports whether this machine's addresses are not the
// ones of the last look, in the same order: the first says where packets
// leave from, which a new default route changes while the addresses stay.
// What others saw of the machine then says nothing any more.
func (t *Transport) addressesChanged() bool {
	list := slices.DeleteFunc(slices.Clone(t.network.Addresses()), t.skip)
	if slices.Equal(list, t.known) {
		return false
	}
	t.known = list
	t.locals.Store(&list)
	t.mu.Lock()
	clear(t.public)
	t.mu.Unlock()
	return true
}

// watch follows the machine from one network to another.
func (t *Transport) watch() {
	tick := time.NewTicker(watchEvery)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			t.look()
		case <-t.done:
			return
		}
	}
}

// look tells Moved if the machine moved to another network, and else
// Changed if its addresses or interfaces changed.
func (t *Transport) look() {
	rebound := t.bind()
	changed := t.addressesChanged()
	switch {
	case t.movedAway():
		signal(t.moved)
	case changed || rebound:
		signal(t.changed)
	}
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// Changed receives when this machine's addresses or interfaces changed,
// on the same network.
func (t *Transport) Changed() <-chan struct{} { return t.changed }

// Moved receives when this machine moved to another network, as a phone
// does from Wi-Fi to cellular. What was opened on the network before may
// be stuck on it, as a socket is on iOS: the sockets then need to be new,
// which takes a new Transport.
func (t *Transport) Moved() <-chan struct{} { return t.moved }

// place is where this machine is on the internet, as far as moving goes:
// the IPv4 address that packets leave from, and the /64 of the IPv6 one,
// in which temporary addresses come and go without a move.
type place struct {
	v4 netip.Addr
	v6 netip.Prefix
}

// placeOf returns the place of addrs, which Addresses lists with the
// addresses that packets leave from first.
func placeOf(addrs []netip.Addr) place {
	var p place
	for _, addr := range addrs {
		switch {
		case addr.Is4() && !p.v4.IsValid():
			p.v4 = addr
		case addr.Is6() && !p.v6.IsValid():
			p.v6, _ = addr.Prefix(64)
		}
	}
	return p
}

// movedAway reports whether the machine moved since the last look: an
// address family that it had then and has now changed. On the way from
// one network to another, the machine may have no address for a moment,
// or one family before the other; neither is a move of its own, so that
// one move counts once.
func (t *Transport) movedAway() bool {
	now := placeOf(t.known)
	if !now.v4.IsValid() && !now.v6.IsValid() {
		return false // between networks: the next look tells where to
	}
	was := t.place
	t.place = now
	return was.v4.IsValid() && now.v4.IsValid() && was.v4 != now.v4 ||
		was.v6.IsValid() && now.v6.IsValid() && was.v6 != now.v6
}

// Stats reports the sockets, and what they could not place: the fields of
// wire.DirectStats that belong to this machine's UDP sockets.
func (t *Transport) Stats() wire.DirectStats {
	stats := wire.DirectStats{Strays: t.strays.Load()}
	for _, s := range []*socket{t.v4, t.v6} {
		if s == nil {
			continue
		}
		stats.Port = int(s.port)
		if i, err := net.InterfaceByIndex(int(s.bound.Load())); err == nil && !slices.Contains(stats.Interfaces, i.Name) {
			stats.Interfaces = append(stats.Interfaces, i.Name)
		}
		if c, err := s.udp.SyscallConn(); err == nil {
			c.Control(func(fd uintptr) {
				stats.ReadBuffer, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
				stats.WriteBuffer, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF)
			})
		}
	}
	return stats
}

func listen(network string, port int) *socket {
	for _, p := range []int{port, 0} {
		udp, err := net.ListenUDP(network, &net.UDPAddr{Port: p})
		if err != nil {
			continue
		}
		udp.SetReadBuffer(socketBuffer)
		udp.SetWriteBuffer(socketBuffer)
		// Where packets arrive; without it, they leave from wherever the
		// system says.
		if network == "udp4" {
			ipv4.NewPacketConn(udp).SetControlMessage(ipv4.FlagDst, true)
		} else {
			ipv6.NewPacketConn(udp).SetControlMessage(ipv6.FlagDst, true)
		}
		return &socket{udp: udp, network: network, port: udp.LocalAddr().(*net.UDPAddr).AddrPort().Port()}
	}
	return nil
}

// Close ends every path and closes the sockets.
func (t *Transport) Close() {
	t.mu.Lock()
	if !t.closed {
		close(t.done)
	}
	t.closed = true
	paths := make([]*Path, 0, len(t.tokens))
	for _, p := range t.tokens {
		paths = append(paths, p)
	}
	t.mu.Unlock()
	for _, p := range paths {
		p.Close()
	}
	for _, s := range []*socket{t.v4, t.v6} {
		if s != nil {
			s.udp.Close()
		}
	}
}

// socketFor returns the socket for addr's family; nil when there is none.
func (t *Transport) socketFor(addr netip.Addr) *socket {
	if addr.Is4() {
		return t.v4
	}
	return t.v6
}

// offerAddresses lists where the other side can try to reach this machine.
func (t *Transport) offerAddresses() []string {
	list := []netip.AddrPort{}
	kept := 0
	for _, a := range t.network.Addresses() {
		if s := t.socketFor(a); s != nil && !t.skip(a) {
			list = append(list, netip.AddrPortFrom(a, s.port))
		}
	}
	t.mu.Lock()
	seen := t.seen
	t.mu.Unlock()
	for _, a := range seen {
		// An address of this machine's own says nothing new: it is in the
		// list already, or it is a tunnel's, which is left out on purpose.
		s := t.socketFor(a)
		if s == nil || !a.IsGlobalUnicast() || t.skip(a) || (t.network.Has != nil && t.network.Has(a)) {
			continue
		}
		if kept++; kept > maxSeen {
			break
		}
		if a := netip.AddrPortFrom(a, s.port); !slices.Contains(list, a) {
			list = append(list, a)
		}
	}
	now := time.Now()
	t.mu.Lock()
	for a, seen := range t.public {
		switch {
		case now.Sub(seen) > publicLife:
			delete(t.public, a)
		case !slices.Contains(list, a):
			list = append(list, a)
		}
	}
	t.mu.Unlock()
	texts := make([]string, 0, len(list))
	for _, a := range list[:min(len(list), maxAddresses)] {
		texts = append(texts, a.String())
	}
	return texts
}

// SetSeen sets the addresses that the SSH links show this machine at: where
// their connections come from or go to, as the machines at their other
// ends see it. Behind a NAT that is the public address, which no interface
// has, and with the port of the sockets it is an address to offer where
// the NAT keeps the port, or passes it on, as one that maps a public
// address to one machine does. Unlike what sawPublic notes, it needs no
// UDP path to have worked before, and it holds for as long as the links
// say so. Offers name the first maxSeen of them that are of use.
func (t *Transport) SetSeen(addrs []netip.Addr) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seen = slices.Clone(addrs) // replaced, never changed
}

// sawPublic notes that another machine saw this one at addr. Behind a NAT
// that keeps one public port per socket, that address serves for all. An
// address of this machine's own says nothing new: the offers name it, and
// the probes that leave from each would fill the few that are kept.
func (t *Transport) sawPublic(addr netip.AddrPort) {
	if !addr.Addr().IsGlobalUnicast() || t.skip(addr.Addr()) || slices.Contains(*t.locals.Load(), addr.Addr()) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.public[addr] = time.Now()
	if len(t.public) > maxPublic {
		var oldest netip.AddrPort
		for a, seen := range t.public {
			if !oldest.IsValid() || seen.Before(t.public[oldest]) {
				oldest = a
			}
		}
		delete(t.public, oldest)
	}
}

// allow lets WireGuard packets from addr in, for p.
func (t *Transport) allow(addr netip.AddrPort, p *Path) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p.ended() || (*t.sources.Load())[addr] == p {
		return
	}
	sources := make(map[netip.AddrPort]*Path, len(*t.sources.Load())+1)
	for a, q := range *t.sources.Load() {
		sources[a] = q
	}
	sources[addr] = p
	t.sources.Store(&sources)
}

// disallow undoes allow for the addresses that drop reports.
func (t *Transport) disallow(drop func(netip.AddrPort, *Path) bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	sources := make(map[netip.AddrPort]*Path, len(*t.sources.Load()))
	for a, q := range *t.sources.Load() {
		if !drop(a, q) {
			sources[a] = q
		}
	}
	t.sources.Store(&sources)
}

// read hands on the packets of the socket until it closes.
func (t *Transport) read(s *socket) {
	buf, oob := make([]byte, maxPacket), make([]byte, controlSize)
	for {
		n, oobn, _, from, err := s.udp.ReadMsgUDPAddrPort(buf, oob)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // Such as the ICMP error of a port that nobody listens on.
		}
		var local netip.Addr
		if !wireGuard(buf[:n]) {
			local = s.arrivedAt(oob[:oobn])
		}
		t.receive(s, buf[:n], netip.AddrPortFrom(from.Addr().Unmap(), from.Port()), local)
	}
}

// wireGuard reports whether packet is one of WireGuard's, by its message
// type.
func wireGuard(packet []byte) bool {
	return len(packet) > 0 && packet[0] >= 1 && packet[0] <= 4
}

// receive handles a packet that came from source to local, the address of
// this machine, which is not valid when the system did not say; it is not
// read for WireGuard's packets. Those belong to the path that their source
// proved itself on, and the others carry the token of their path.
func (t *Transport) receive(s *socket, packet []byte, source netip.AddrPort, local netip.Addr) {
	if wireGuard(packet) {
		if p := (*t.sources.Load())[source]; p != nil {
			p.packet(packet)
		} else {
			t.strays.Add(1)
		}
		return
	}
	if len(packet) < 1+tokenLen || t.skip(source.Addr()) {
		return
	}
	t.mu.Lock()
	p := t.tokens[[tokenLen]byte(packet[1:1+tokenLen])]
	t.mu.Unlock()
	if p == nil {
		return
	}
	body := packet[1+tokenLen:]
	switch {
	case packet[0] == kindProbe && len(packet) == probeLen:
		if [idLen]byte(body[:idLen]) == t.id {
			return // A probe of this daemon, to an address that another machine has too.
		}
		// From the address it came to, which is the one the other side
		// and what lies between know. macOS may refuse an address of
		// another interface than the one the socket is bound to; an answer
		// to a probe that came to one leaves from where the system says.
		packet := answer(p.token, body[idLen:], source)
		if err := s.write(packet, control(local), source); err != nil && local.IsValid() && gone(err) {
			s.write(packet, nil, source)
			local = netip.Addr{} // Nothing leaves from there, so it makes no pair.
		}
		p.probed(source, local, body[idLen+echoLen-1]&flagWorks != 0)
	case packet[0] == kindAnswer && len(packet) == answerLen:
		sent := time.Duration(binary.BigEndian.Uint64(body))
		to, ok := parseAddr(body[8:])
		seen, seenOK := parseAddr(body[echoLen:])
		rtt := time.Since(t.start) - sent
		if !ok || !seenOK || rtt < 0 || rtt > maxRTT {
			return
		}
		p.answered(to, source, local, rtt, body[echoLen-1]&flagWorks != 0)
		t.sawPublic(seen)
	case packet[0] == kindBye:
		if p.knows(source) && !p.ended() {
			p.tracef("bye from %s: the other side closed the path", source)
			p.end()
		}
	}
}

// probe sends a probe of p between the addresses of to; works says that
// the pair works both ways.
func (t *Transport) probe(p *Path, to pair, works bool) error {
	s := t.socketFor(to.remote.Addr())
	if s == nil {
		return nil
	}
	packet := make([]byte, 0, probeLen)
	packet = append(packet, kindProbe)
	packet = append(packet, p.token[:]...)
	packet = append(packet, t.id[:]...)
	packet = binary.BigEndian.AppendUint64(packet, uint64(time.Since(t.start)))
	packet = appendAddr(packet, to.remote)
	var flags byte
	if works {
		flags |= flagWorks
	}
	packet = append(packet, flags)
	return s.write(packet, to.control, to.remote)
}

// answer makes the answer to a probe, whose time, address, and flags are
// echo.
func answer(token [tokenLen]byte, echo []byte, source netip.AddrPort) []byte {
	packet := make([]byte, 0, answerLen)
	packet = append(packet, kindAnswer)
	packet = append(packet, token[:]...)
	packet = append(packet, echo...)
	return appendAddr(packet, source)
}

func appendAddr(packet []byte, addr netip.AddrPort) []byte {
	ip := addr.Addr().As16()
	packet = append(packet, ip[:]...)
	return binary.BigEndian.AppendUint16(packet, addr.Port())
}

func parseAddr(b []byte) (netip.AddrPort, bool) {
	addr := netip.AddrPortFrom(netip.AddrFrom16([16]byte(b[:16])).Unmap(), binary.BigEndian.Uint16(b[16:addrLen]))
	return addr, addr.IsValid() && addr.Port() != 0
}
