// Package direct connects two gst daemons over UDP, without SSH, when
// the network between them allows it. The daemons exchange offers through
// the mesh, which also authenticates them: an offer names the addresses to
// try and the certificate to expect. Each side then probes the other's
// addresses, from one socket per local address, and answers probes.
// Where a probe comes from is a further address to try, so a machine
// behind NAT needs no one to tell it its public address, as long as the
// other side can be reached. A probe that is answered proves a path; the
// offering side connects over it with QUIC, and both sides check the
// certificate.
package direct

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

const (
	alpn          = "gst-direct"
	probeInterval = 200 * time.Millisecond
	dialTimeout   = 5 * time.Second  // to finish the QUIC handshake over a path
	idleTimeout   = 20 * time.Second // connections wait this long on a path that broke
	keepAlive     = 5 * time.Second  // well within how long NATs and firewalls remember UDP
	maxStreams    = 1 << 16
	maxAddresses  = 32 // accepted in an offer

	tokenLen = 16
	// The first byte of a probe. quic-go hands packets whose first two
	// bits are zero to ReadNonQUICPacket.
	kindProbe  = 1
	kindAnswer = 2
)

// Attempt is one try at a direct connection, on one socket per local
// address. Close it when the try ends; the socket of the connection it
// returned stays open.
type Attempt struct {
	token       [tokenLen]byte
	cert        tls.Certificate
	fingerprint string
	sockets     []*socket

	mu      sync.Mutex
	targets map[netip.AddrPort]bool
	proved  map[path]bool
	won     *socket
}

type socket struct {
	addr      netip.AddrPort
	udp       *net.UDPConn
	transport *quic.Transport
}

func (s *socket) close() {
	s.transport.Close()
	s.udp.Close()
}

// path is a way to the other side: from a socket to an address.
type path struct {
	socket *socket
	to     netip.AddrPort
}

// New starts an attempt from addrs, this machine's addresses to try.
func New(addrs []netip.Addr) (*Attempt, error) {
	cert, err := newCertificate()
	if err != nil {
		return nil, err
	}
	a := &Attempt{
		cert:        cert,
		fingerprint: fingerprint(cert.Certificate[0]),
		targets:     map[netip.AddrPort]bool{},
		proved:      map[path]bool{},
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
		a.sockets = append(a.sockets, &socket{
			addr:      udp.LocalAddr().(*net.UDPAddr).AddrPort(),
			udp:       udp,
			transport: &quic.Transport{Conn: udp},
		})
	}
	if len(a.sockets) == 0 {
		return nil, errors.New("no local address to use")
	}
	return a, nil
}

// Close ends the attempt, keeping only what its connection uses.
func (a *Attempt) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sockets {
		if s != a.won {
			s.close()
		}
	}
}

// Offer describes this side of the attempt.
func (a *Attempt) Offer() wire.DirectOffer {
	offer := wire.DirectOffer{Token: hex.EncodeToString(a.token[:]), Fingerprint: a.fingerprint}
	for _, s := range a.sockets {
		offer.Addresses = append(offer.Addresses, s.addr.String())
	}
	return offer
}

// Dial finds a path to the side that answered with theirs and connects
// over it, until ctx is done.
func (a *Attempt) Dial(ctx context.Context, theirs wire.DirectOffer) (*Conn, error) {
	targets, err := addresses(theirs)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	proved := a.probe(ctx, targets)
	conf := a.tlsConfig(theirs.Fingerprint)
	for {
		select {
		case <-ctx.Done():
			return nil, errors.New("no path found")
		case p := <-proved:
			dialCtx, stop := context.WithTimeout(ctx, dialTimeout)
			conn, err := p.socket.transport.Dial(dialCtx, net.UDPAddrFromAddrPort(p.to), conf, quicConfig())
			stop()
			if err == nil {
				return a.keep(p.socket, conn), nil
			}
		}
	}
}

// Accept waits for the side that offered theirs to connect, until ctx is
// done, and helps it find a path meanwhile.
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

	type accepted struct {
		socket *socket
		conn   *quic.Conn
	}
	conns := make(chan accepted)
	conf := a.tlsConfig(theirs.Fingerprint)
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // before the wait, which it ends
	for _, s := range a.sockets {
		listener, err := s.transport.Listen(conf, quicConfig())
		if err != nil {
			continue
		}
		wg.Go(func() {
			defer listener.Close()
			conn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			select {
			case conns <- accepted{s, conn}:
			case <-ctx.Done():
				conn.CloseWithError(0, "")
			}
		})
	}
	a.probe(ctx, targets)
	select {
	case c := <-conns:
		return a.keep(c.socket, c.conn), nil
	case <-ctx.Done():
		return nil, errors.New("no connection arrived")
	}
}

func (a *Attempt) keep(s *socket, conn *quic.Conn) *Conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.won = s
	return &Conn{conn: conn, socket: s}
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
	proved := make(chan path, 1024)
	for _, s := range a.sockets {
		go a.read(ctx, s, proved)
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
	packet := a.packet(kindProbe)
	for _, s := range a.sockets {
		for _, t := range targets {
			if s.addr.Addr().Is4() == t.Addr().Is4() {
				s.transport.WriteTo(packet, net.UDPAddrFromAddrPort(t))
			}
		}
	}
}

func (a *Attempt) packet(kind byte) []byte {
	return append([]byte{kind}, a.token[:]...)
}

// read handles the packets that arrive on s. A probe gets an answer, and
// its source becomes a target: that is how a side learns the address of
// another behind NAT. An answer proves the path it came over.
func (a *Attempt) read(ctx context.Context, s *socket, proved chan<- path) {
	buf := make([]byte, 1+tokenLen+1)
	for {
		n, from, err := s.transport.ReadNonQUICPacket(ctx, buf)
		if err != nil {
			return
		}
		if n != 1+tokenLen || subtle.ConstantTimeCompare(buf[1:n], a.token[:]) != 1 {
			continue
		}
		udp, ok := from.(*net.UDPAddr)
		if !ok {
			continue
		}
		source := udp.AddrPort()
		source = netip.AddrPortFrom(source.Addr().Unmap(), source.Port())
		switch buf[0] {
		case kindProbe:
			s.transport.WriteTo(a.packet(kindAnswer), from)
			a.mu.Lock()
			if len(a.targets) < 2*maxAddresses {
				a.targets[source] = true
			}
			a.mu.Unlock()
		case kindAnswer:
			p := path{s, source}
			a.mu.Lock()
			fresh := !a.proved[p]
			a.proved[p] = true
			a.mu.Unlock()
			if fresh {
				select {
				case proved <- p:
				default:
				}
			}
		}
	}
}

func quicConfig() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout: dialTimeout,
		MaxIdleTimeout:       idleTimeout,
		KeepAlivePeriod:      keepAlive,
		MaxIncomingStreams:   maxStreams,
	}
}

// tlsConfig trusts exactly the certificate with the fingerprint that the
// mesh delivered, for both the client and the server role.
func (a *Attempt) tlsConfig(want string) *tls.Config {
	return &tls.Config{
		Certificates:           []tls.Certificate{a.cert},
		NextProtos:             []string{alpn},
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
		ClientAuth:             tls.RequireAnyClientCert,
		InsecureSkipVerify:     true, // VerifyPeerCertificate replaces the chain.
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 || subtle.ConstantTimeCompare([]byte(fingerprint(raw[0])), []byte(want)) != 1 {
				return errors.New("unexpected certificate")
			}
			return nil
		},
	}
}

func fingerprint(cert []byte) string {
	sum := sha256.Sum256(cert)
	return hex.EncodeToString(sum[:])
}

// newCertificate makes a self-signed certificate for one attempt. Nothing
// but its fingerprint matters, so it names no one, and it does not expire
// while the attempt can use it.
func newCertificate() (tls.Certificate, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create a certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}, nil
}
