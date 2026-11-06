// Package direct finds a UDP path between two gst daemons, when the
// network between them allows one. The daemons exchange offers over SSH
// through the mesh: an offer names the addresses to try and a token. Each
// side then probes the other's addresses, from one socket per local
// address, and answers probes. Where a probe comes from is a further
// address to try, so a machine behind NAT needs no one to tell it its
// public address, as long as the other side can be reached. A probe that
// is answered proves a path; the offering side picks one, the other side
// confirms it, and the path then carries the packets of the virtual
// network, which WireGuard encrypts and authenticates. Nothing here
// protects them: the token only keeps stray packets out of the probes.
package direct

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

const (
	probeInterval = 200 * time.Millisecond
	useTimeout    = 2 * time.Second // for the other side to confirm a path
	maxAddresses  = 32              // accepted in an offer
	socketBuffer  = 4 << 20         // in bytes; a frame of video arrives as a burst of hundreds of packets
	maxPacket     = 1 << 16

	tokenLen = 16
)

// The first byte of the packets of this package, then the token. WireGuard
// uses 1 to 4, for packets that this package hands on.
const (
	kindProbe  = 0x11 // is anyone there? asks for an answer
	kindAnswer = 0x12 // the path to the side that answers works
	kindUse    = 0x13 // the offering side picks this path
	kindUsed   = 0x14 // and the other side confirms
	kindBeat   = 0x15 // the path still works
	kindBye    = 0x16 // the path ends now
)

// Attempt is one try at a path, on one socket per local address. Close it
// when the try ends; the socket of the path it found stays open.
type Attempt struct {
	// Trace, when set before Dial or Accept, receives what the attempt
	// does, addresses included, from several goroutines.
	Trace func(string)

	token   [tokenLen]byte
	sockets []*socket
	used    chan path // paths that the other side confirmed
	picked  chan path // paths that the other side picked

	mu      sync.Mutex
	targets map[netip.AddrPort]bool
	proved  map[path]bool
	heard   map[path]bool
	won     *socket
}

// socket is a UDP socket and what reads it now.
type socket struct {
	addr    netip.AddrPort
	udp     *net.UDPConn
	receive atomic.Pointer[func(packet []byte, from netip.AddrPort)]
}

// read hands on the packets of the socket until it closes.
func (s *socket) read() {
	buf := make([]byte, maxPacket)
	for {
		n, from, err := s.udp.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // Such as the ICMP error of a port that nobody listens on.
		}
		(*s.receive.Load())(buf[:n], netip.AddrPortFrom(from.Addr().Unmap(), from.Port()))
	}
}

func (s *socket) send(kind byte, token [tokenLen]byte, to netip.AddrPort) {
	s.udp.WriteToUDPAddrPort(append([]byte{kind}, token[:]...), to)
}

// path is a way to the other side: from a socket to an address.
type path struct {
	socket *socket
	to     netip.AddrPort
}

// New starts an attempt from addrs, this machine's addresses to try.
func New(addrs []netip.Addr) (*Attempt, error) {
	a := &Attempt{
		used:    make(chan path, 1),
		picked:  make(chan path, 1),
		targets: map[netip.AddrPort]bool{},
		proved:  map[path]bool{},
		heard:   map[path]bool{},
	}
	if _, err := rand.Read(a.token[:]); err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		// A socket bound to one address answers from that address, and
		// on macOS it leaves through that address's interface, whatever
		// the default route says. A wildcard socket does neither, which
		// loses the answers of a machine with several IPv6 addresses or
		// a VPN that takes the default route.
		udp, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(addr, 0)))
		if err != nil {
			continue // The address went away.
		}
		udp.SetReadBuffer(socketBuffer)
		udp.SetWriteBuffer(socketBuffer)
		a.sockets = append(a.sockets, &socket{addr: udp.LocalAddr().(*net.UDPAddr).AddrPort(), udp: udp})
	}
	if len(a.sockets) == 0 {
		return nil, errors.New("no local address to use")
	}
	return a, nil
}

func (a *Attempt) trace(format string, args ...any) {
	if a.Trace != nil {
		a.Trace(fmt.Sprintf(format, args...))
	}
}

// Close ends the attempt, keeping only the socket of the path it found.
func (a *Attempt) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sockets {
		if s != a.won {
			s.udp.Close()
		}
	}
}

// Offer describes this side of the attempt.
func (a *Attempt) Offer() wire.DirectOffer {
	offer := wire.DirectOffer{Token: hex.EncodeToString(a.token[:])}
	for _, s := range a.sockets {
		offer.Addresses = append(offer.Addresses, s.addr.String())
	}
	return offer
}

// Dial finds a path to the side that answered with theirs and picks it,
// until ctx is done.
func (a *Attempt) Dial(ctx context.Context, theirs wire.DirectOffer) (*Conn, error) {
	targets, err := addresses(theirs)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	proved := a.probe(ctx, targets)
	for {
		select {
		case <-ctx.Done():
			return nil, errors.New("no path found")
		case p := <-proved:
			a.trace("picking the path from %s to %s", p.socket.addr, p.to)
			if a.use(ctx, p) {
				a.trace("the other side confirmed the path")
				return a.keep(p), nil
			}
			a.trace("no confirmation from %s", p.to)
		}
	}
}

// use tells the other side to use p, until it confirms or useTimeout
// passes, and reports whether it confirmed.
func (a *Attempt) use(ctx context.Context, p path) bool {
	ctx, cancel := context.WithTimeout(ctx, useTimeout)
	defer cancel()
	tick := time.NewTicker(probeInterval)
	defer tick.Stop()
	for {
		p.socket.send(kindUse, a.token, p.to)
		select {
		case <-ctx.Done():
			return false
		case used := <-a.used:
			if used == p {
				return true
			}
		case <-tick.C:
		}
	}
}

// Accept waits for the side that offered theirs to pick a path, until ctx
// is done, and helps it find one meanwhile.
func (a *Attempt) Accept(ctx context.Context, theirs wire.DirectOffer) (*Conn, error) {
	token, err := hex.DecodeString(theirs.Token)
	if err != nil || len(token) != tokenLen {
		return nil, errors.New("the offer has no valid token")
	}
	targets, err := addresses(theirs)
	if err != nil {
		return nil, err
	}
	copy(a.token[:], token)
	a.probe(ctx, targets)
	select {
	case p := <-a.picked:
		a.trace("the other side picked the path from %s to %s", p.socket.addr, p.to)
		return a.keep(p), nil
	case <-ctx.Done():
		return nil, errors.New("no path picked")
	}
}

func (a *Attempt) keep(p path) *Conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.won = p.socket
	return newConn(p.socket, p.to, a.token)
}

// addresses reads the addresses of an offer.
func addresses(offer wire.DirectOffer) ([]netip.AddrPort, error) {
	if len(offer.Addresses) > maxAddresses {
		return nil, errors.New("the offer has too many addresses")
	}
	list := make([]netip.AddrPort, 0, len(offer.Addresses))
	for _, s := range offer.Addresses {
		addr, err := netip.ParseAddrPort(s)
		if err != nil {
			return nil, errors.New("the offer has an invalid address")
		}
		list = append(list, addr)
	}
	return list, nil
}

// probe probes targets from every socket and answers probes until ctx is
// done. It returns the paths that answers prove, as they arrive.
func (a *Attempt) probe(ctx context.Context, targets []netip.AddrPort) <-chan path {
	a.mu.Lock()
	for _, t := range targets {
		a.targets[t] = true
	}
	a.mu.Unlock()
	a.trace("probing %d addresses from %d sockets", len(targets), len(a.sockets))
	proved := make(chan path, 1024)
	for _, s := range a.sockets {
		receive := func(packet []byte, from netip.AddrPort) { a.receive(s, packet, from, proved) }
		s.receive.Store(&receive)
		go s.read()
	}
	go func() {
		tick := time.NewTicker(probeInterval)
		defer tick.Stop()
		for {
			a.sendProbes()
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return proved
}

func (a *Attempt) sendProbes() {
	a.mu.Lock()
	targets := make([]netip.AddrPort, 0, len(a.targets))
	for t := range a.targets {
		targets = append(targets, t)
	}
	a.mu.Unlock()
	for _, s := range a.sockets {
		for _, t := range targets {
			if s.addr.Addr().Is4() == t.Addr().Is4() {
				s.send(kindProbe, a.token, t)
			}
		}
	}
}

// signed reports whether packet is one of this package's, with token.
func signed(packet []byte, token [tokenLen]byte) bool {
	return len(packet) == 1+tokenLen && subtle.ConstantTimeCompare(packet[1:], token[:]) == 1
}

// receive handles a packet of the try. A probe gets an answer, and its
// source becomes a target: that is how a side learns the address of
// another behind NAT. An answer proves the path it came over.
func (a *Attempt) receive(s *socket, packet []byte, source netip.AddrPort, proved chan<- path) {
	if !signed(packet, a.token) {
		return
	}
	p := path{s, source}
	switch packet[0] {
	case kindProbe:
		s.send(kindAnswer, a.token, source)
		a.mu.Lock()
		fresh, learned := !a.heard[p], !a.targets[source]
		a.heard[p] = true
		if len(a.targets) < 2*maxAddresses {
			a.targets[source] = true
		}
		a.mu.Unlock()
		switch {
		case fresh && learned:
			a.trace("probe from %s on %s: a new address, answering and probing it too", source, s.addr)
		case fresh:
			a.trace("probe from %s on %s: answering", source, s.addr)
		}
	case kindAnswer:
		a.mu.Lock()
		fresh := !a.proved[p]
		a.proved[p] = true
		a.mu.Unlock()
		if fresh {
			a.trace("answer from %s on %s: the path works", source, s.addr)
			select {
			case proved <- p:
			default:
			}
		}
	case kindUse:
		s.send(kindUsed, a.token, source)
		select {
		case a.picked <- p:
		default:
		}
	case kindUsed:
		select {
		case a.used <- p:
		default:
		}
	}
}
