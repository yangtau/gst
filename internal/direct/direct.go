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
// Packets go the way the system's routes say, with one exception: on macOS
// the sockets send through the interface that reaches the internet, because
// a proxy that takes the routes sends UDP again from another port, which
// holes in firewalls do not fit (see bind_darwin.go). Each side measures its addresses of the other and
// sends to the fastest, so the two directions may differ. The path then
// carries the packets of the virtual network, which WireGuard encrypts and
// authenticates. Nothing here protects them: the token only keeps stray
// packets out.
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
	kindProbe  = 0x11 // token, sender's id, time, address it goes to; asks for an answer
	kindAnswer = 0x12 // token, the probe's time and address, where the probe came from
	kindBye    = 0x13 // token; the path ends now
)

const (
	probeLen  = 1 + tokenLen + idLen + 8 + addrLen
	answerLen = 1 + tokenLen + 8 + 2*addrLen
)

// Transport is this daemon's end of all its UDP paths.
type Transport struct {
	addrs func() []netip.Addr
	skip  func(netip.Addr) bool
	v4    *socket
	v6    *socket
	id    [idLen]byte // tells this daemon's probes, which may reach itself
	start time.Time   // what the times in probes count from

	sweepEvery time.Duration // how often a path measures all its addresses

	mu      sync.Mutex
	tokens  map[[tokenLen]byte]*Path
	public  map[netip.AddrPort]time.Time // this machine as others see it
	closed  bool
	done    chan struct{}                            // closed by Close
	changed chan struct{}                            // for Changed
	known   []netip.Addr                             // what addrs said last; owned by watch
	strays  atomic.Uint64                            // WireGuard packets from unknown sources
	sources atomic.Pointer[map[netip.AddrPort]*Path] // by the addresses that proved themselves; replaced, never changed
}

type socket struct {
	udp     *net.UDPConn
	network string
	port    uint16
	bound   atomic.Int64 // the interface it sends through; 0 when the routes decide
}

// Listen opens the sockets, on port if it is free. addrs returns this
// machine's addresses to offer. skip says which addresses to have nothing
// to do with, in offers, in what the other side offers, and as the source
// of a packet: those of the virtual network, whose packets would take the
// path that they make up.
func Listen(port int, addrs func() []netip.Addr, skip func(netip.Addr) bool) (*Transport, error) {
	t := &Transport{
		addrs:      addrs,
		skip:       skip,
		start:      time.Now(),
		sweepEvery: sweepInterval,
		tokens:     map[[tokenLen]byte]*Path{},
		public:     map[netip.AddrPort]time.Time{},
		done:       make(chan struct{}),
		changed:    make(chan struct{}, 1),
	}
	rand.Read(t.id[:])
	t.sources.Store(&map[netip.AddrPort]*Path{})
	if t.v4 = listen("udp4", port); t.v4 != nil {
		port = int(t.v4.port)
	}
	t.v6 = listen("udp6", port)
	if t.v4 == nil && t.v6 == nil {
		return nil, errors.New("no UDP socket")
	}
	t.bind()
	t.addressesChanged()
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
	changed := false
	v4, v6 := defaultInterfaces()
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
	list := slices.DeleteFunc(slices.Clone(t.addrs()), t.skip)
	if slices.Equal(list, t.known) {
		return false
	}
	t.known = list
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

// look tells Changed if the machine is on another network now.
func (t *Transport) look() {
	moved := t.bind()
	if t.addressesChanged() || moved {
		select {
		case t.changed <- struct{}{}:
		default:
		}
	}
}

// Changed receives when this machine's addresses or interfaces changed.
func (t *Transport) Changed() <-chan struct{} { return t.changed }

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
	for _, a := range t.addrs() {
		if s := t.socketFor(a); s != nil && !t.skip(a) {
			list = append(list, netip.AddrPortFrom(a, s.port))
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

// sawPublic notes that another machine saw this one at addr. Behind a NAT
// that keeps one public port per socket, that address serves for all.
func (t *Transport) sawPublic(addr netip.AddrPort) {
	if !addr.Addr().IsGlobalUnicast() || t.skip(addr.Addr()) {
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
	buf := make([]byte, maxPacket)
	for {
		n, from, err := s.udp.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // Such as the ICMP error of a port that nobody listens on.
		}
		t.receive(s, buf[:n], netip.AddrPortFrom(from.Addr().Unmap(), from.Port()))
	}
}

// wireGuard reports whether packet is one of WireGuard's, by its message
// type.
func wireGuard(packet []byte) bool {
	return len(packet) > 0 && packet[0] >= 1 && packet[0] <= 4
}

// receive handles a packet. WireGuard's belong to the path that their
// source proved itself on, and the others carry the token of their path.
func (t *Transport) receive(s *socket, packet []byte, source netip.AddrPort) {
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
		s.udp.WriteToUDPAddrPort(answer(p.token, body[idLen:], source), source)
		p.probed(source)
	case packet[0] == kindAnswer && len(packet) == answerLen:
		sent := time.Duration(binary.BigEndian.Uint64(body))
		to, ok := parseAddr(body[8:])
		seen, seenOK := parseAddr(body[8+addrLen:])
		rtt := time.Since(t.start) - sent
		if !ok || !seenOK || rtt < 0 || rtt > maxRTT {
			return
		}
		p.answered(to, source, rtt)
		t.sawPublic(seen)
	case packet[0] == kindBye:
		if p.knows(source) && !p.ended() {
			p.tracef("bye from %s: the other side closed the path", source)
			p.end()
		}
	}
}

func (t *Transport) probe(p *Path, to netip.AddrPort) {
	s := t.socketFor(to.Addr())
	if s == nil {
		return
	}
	packet := make([]byte, 0, probeLen)
	packet = append(packet, kindProbe)
	packet = append(packet, p.token[:]...)
	packet = append(packet, t.id[:]...)
	packet = binary.BigEndian.AppendUint64(packet, uint64(time.Since(t.start)))
	packet = appendAddr(packet, to)
	s.udp.WriteToUDPAddrPort(packet, to)
}

// answer makes the answer to a probe, whose time and address are echo.
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
