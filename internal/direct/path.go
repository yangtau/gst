package direct

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

const (
	probeInterval = 200 * time.Millisecond
	beatInterval  = time.Second       // how often the address in use is probed; also keeps it open through NATs
	staleLimit    = 2 * time.Second   // an address in use this quiet gives way to any other that answers
	silenceLimit  = 3 * time.Second   // a path this quiet has ended
	sweepInterval = 30 * time.Second  // how often all the addresses are measured
	sweepTime     = time.Second       // for how long, at probeInterval
	measureLife   = 2 * sweepInterval // an address this quiet starts its measures again
	maxAddresses  = 32                // accepted in an offer
	maxTargets    = 2 * maxAddresses  // also counts addresses learned from packets

	// A faster address must have this many measures, and beat the one in
	// use by its margin, or paths would hop between equals.
	minSamples = 3
	minGain    = 2 * time.Millisecond
)

// margin is by how much an address must beat one in use that takes rtt:
// minGain, or a tenth when that is more.
func margin(rtt time.Duration) time.Duration {
	return max(minGain, rtt/10)
}

// cost is what addresses compete on: the round trip, less twice its margin
// for a private address. Packets to one stay on a local network, or a VPN
// the machines share, which is steadier and spares the internet link. So a
// private address takes over unless it is slower by more than about its
// margin, and gives way only to one faster by three.
func cost(addr netip.Addr, rtt time.Duration) time.Duration {
	if addr.IsPrivate() {
		return rtt - 2*margin(rtt)
	}
	return rtt
}

// Path is a UDP path to one node: the addresses to reach it at, and the
// pair of addresses in use. It starts as a try, and once an address answers
// it carries packets, until it falls silent or a side closes it.
type Path struct {
	t      *Transport
	token  [tokenLen]byte
	trace  func(string)
	handle atomic.Pointer[func(packet []byte)]
	best   atomic.Pointer[pair]
	heard  atomic.Int64 // when anything last arrived, in Unix nanoseconds
	sent   atomic.Int64 // when this side last sent a packet, in Unix nanoseconds
	silent atomic.Bool  // the path ended as the other side fell silent

	proved chan struct{}
	done   chan struct{}
	once   sync.Once

	mu         sync.Mutex // never held while calling trace
	targets    map[netip.AddrPort]*target
	running    bool
	sweepUntil time.Time
	nextSweep  time.Time
	nextBeat   time.Time
}

// target is an address of the other side.
type target struct {
	rtt     time.Duration // smoothed
	samples int
	heard   time.Time  // of the latest answer
	probed  bool       // a probe came from it
	local   netip.Addr // the address of this machine that its answers come to; not valid until one does
}

// pair is what packets go between: an address of this machine and one of
// the other side's.
type pair struct {
	local   netip.Addr // not valid when the system picks it
	remote  netip.AddrPort
	control []byte // makes packets leave from local
}

func newPair(local netip.Addr, remote netip.AddrPort) *pair {
	return &pair{local: local, remote: remote, control: control(local)}
}

func (w *pair) String() string {
	if !w.local.IsValid() {
		return w.remote.String()
	}
	return fmt.Sprintf("%s from %s", w.remote, w.local)
}

func (t *Transport) newPath(token [tokenLen]byte, trace func(string)) (*Path, error) {
	p := &Path{
		t:       t,
		token:   token,
		trace:   trace,
		proved:  make(chan struct{}),
		done:    make(chan struct{}),
		targets: map[netip.AddrPort]*target{},
	}
	p.sent.Store(time.Now().UnixNano())
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errors.New("UDP paths are off")
	}
	t.tokens[token] = p
	return p, nil
}

// Start begins a try at a path, for the side that offers. trace, when not
// nil, receives what the path does, addresses included, from several
// goroutines.
func (t *Transport) Start(trace func(string)) (*Path, error) {
	var token [tokenLen]byte
	rand.Read(token[:])
	return t.newPath(token, trace)
}

// Join begins the try that offer started, for the side that answers it.
func (t *Transport) Join(offer wire.DirectOffer, trace func(string)) (*Path, error) {
	token, err := hex.DecodeString(offer.Token)
	if err != nil || len(token) != tokenLen {
		return nil, errors.New("the offer has no valid token")
	}
	return t.newPath([tokenLen]byte(token), trace)
}

// Offer describes this side of the path.
func (p *Path) Offer() wire.DirectOffer {
	return wire.DirectOffer{Token: hex.EncodeToString(p.token[:]), Addresses: p.t.offerAddresses()}
}

// Connect probes the addresses of theirs, and returns once one answers,
// or fails when ctx is done first. The caller closes the path if it fails.
func (p *Path) Connect(ctx context.Context, theirs wire.DirectOffer) error {
	targets, err := p.t.addresses(theirs)
	if err != nil {
		return err
	}
	p.mu.Lock()
	for _, addr := range targets {
		p.add(addr)
	}
	start := !p.running
	p.running = true
	p.mu.Unlock()
	p.tracef("probing %d addresses", len(targets))
	if start {
		go p.run()
	}
	select {
	case <-p.proved:
		return nil
	case <-p.done:
		return errors.New("the path ended")
	case <-ctx.Done():
		return errors.New("no path found")
	}
}

// addresses reads the addresses of an offer.
func (t *Transport) addresses(offer wire.DirectOffer) ([]netip.AddrPort, error) {
	if len(offer.Addresses) > maxAddresses {
		return nil, errors.New("the offer has too many addresses")
	}
	list := make([]netip.AddrPort, 0, len(offer.Addresses))
	for _, s := range offer.Addresses {
		addr, err := netip.ParseAddrPort(s)
		if err != nil {
			return nil, errors.New("the offer has an invalid address")
		}
		if addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port()); !t.skip(addr.Addr()) {
			list = append(list, addr)
		}
	}
	return list, nil
}

func (p *Path) tracef(format string, args ...any) {
	if p.trace != nil {
		p.trace(fmt.Sprintf(format, args...))
	}
}

// add keeps addr as a target, making room if it must; p.mu is held.
func (p *Path) add(addr netip.AddrPort) *target {
	if t := p.targets[addr]; t != nil {
		return t
	}
	if len(p.targets) >= maxTargets {
		p.evict()
	}
	t := &target{}
	p.targets[addr] = t
	return t
}

// evict forgets the address that answered longest ago; p.mu is held.
func (p *Path) evict() {
	var oldest netip.AddrPort
	best := p.best.Load()
	for a, t := range p.targets {
		if (best == nil || a != best.remote) && (!oldest.IsValid() || t.heard.Before(p.targets[oldest].heard)) {
			oldest = a
		}
	}
	delete(p.targets, oldest)
	go p.t.disallow(func(a netip.AddrPort, q *Path) bool { return q == p && a == oldest })
}

// knows reports whether addr is an address of the other side.
func (p *Path) knows(addr netip.AddrPort) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.targets[addr] != nil
}

// Refresh adds theirs, the addresses of the other side after it changed
// network, and measures all addresses from scratch.
func (p *Path) Refresh(theirs wire.DirectOffer) error {
	if theirs.Token != hex.EncodeToString(p.token[:]) {
		return errors.New("the update is for another path")
	}
	targets, err := p.t.addresses(theirs)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, addr := range targets {
		p.add(addr)
	}
	p.remeasure()
	return nil
}

// Remeasure makes the path measure its addresses from scratch, after this
// side changed network: probes leave from wherever the system says, and the
// answers tell which addresses of this machine to send from. Packets keep
// the pair in use until then.
func (p *Path) Remeasure() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.remeasure()
}

func (p *Path) remeasure() {
	for _, t := range p.targets {
		t.samples = 0
		t.local = netip.Addr{}
	}
	p.sweepUntil = time.Now().Add(sweepTime)
}

// probed handles a probe from source, which the token proves to be the
// other side: that is how a side learns the address of another behind NAT.
func (p *Path) probed(source netip.AddrPort) {
	p.mu.Lock()
	t := p.targets[source]
	fresh, learned := t == nil || !t.probed, t == nil
	p.add(source).probed = true
	p.mu.Unlock()
	p.t.allow(source, p)
	switch {
	case fresh && learned:
		p.tracef("probe from %s: a new address, answering and probing it too", source)
	case fresh:
		p.tracef("probe from %s: answering", source)
	}
}

// answered handles the answer, from source to local, the address of this
// machine, to a probe sent to addr rtt ago. The address probed gets the
// credit, wherever the answer came from: a NAT on the other side may change
// it. Its answers then come to local, so packets to it leave from there.
//
// The source becomes an address of the path like any other, so that the
// bound on those holds for sources too.
func (p *Path) answered(addr, source netip.AddrPort, local netip.Addr, rtt time.Duration) {
	p.mu.Lock()
	known := p.targets[addr] != nil
	if known {
		p.add(source)
	}
	p.mu.Unlock()
	if !known {
		return
	}
	p.t.allow(source, p)
	p.heard.Store(time.Now().UnixNano())
	p.mu.Lock()
	events := p.record(addr, local, rtt, time.Now())
	p.mu.Unlock()
	for _, e := range events {
		p.tracef("%s", e)
	}
}

// record measures addr, whose answer came to local, and uses it if it is
// the better one; p.mu is held. It returns what happened, for the trace.
//
// An address answered at one address of this machine keeps it while it
// answers there: a late answer to a probe that the system sent from another
// does not move it.
func (p *Path) record(addr netip.AddrPort, local netip.Addr, rtt time.Duration, now time.Time) []string {
	var events []string
	t := p.targets[addr]
	if t == nil {
		return nil
	}
	if t.heard.IsZero() {
		events = append(events, fmt.Sprintf("answer from %s in %s: the path works", addr, ms(rtt)))
	}
	if local.IsValid() && (!t.local.IsValid() || now.Sub(t.heard) > staleLimit) {
		t.local = local
	}
	if now.Sub(t.heard) > measureLife {
		t.samples = 0
	}
	if t.samples == 0 {
		t.rtt = rtt
	} else {
		t.rtt += (rtt - t.rtt) / 4
	}
	t.samples++
	t.heard = now
	best, next := p.best.Load(), newPair(t.local, addr)
	switch {
	case best == nil:
		p.best.Store(next)
		p.sweepUntil, p.nextSweep = now.Add(sweepTime), now.Add(p.t.sweepEvery)
		close(p.proved)
		events = append(events, fmt.Sprintf("up: using %s", next))
	case best.remote == addr:
		if t.local.IsValid() && best.local != t.local {
			p.best.Store(next)
			events = append(events, fmt.Sprintf("using %s: the answers come there", next))
		}
	default:
		current := p.targets[best.remote]
		switch {
		case current == nil || now.Sub(current.heard) > staleLimit:
			p.best.Store(next)
			events = append(events, fmt.Sprintf("moved to %s: the address in use stopped answering", next))
		case t.samples >= minSamples && cost(addr.Addr(), t.rtt)+margin(current.rtt) <= cost(best.remote.Addr(), current.rtt):
			p.best.Store(next)
			events = append(events, fmt.Sprintf("moved to %s: %s%s, was %s", next, ms(t.rtt), privately(addr), ms(current.rtt)))
		}
	}
	return events
}

// privately names a private address in the trace, which explains a move
// to one that is not the fastest.
func privately(addr netip.AddrPort) string {
	if addr.Addr().IsPrivate() {
		return " on a private network"
	}
	return ""
}

func ms(d time.Duration) string {
	return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
}

// run sends the probes until the path ends: to all the addresses during a
// try and a sweep, and otherwise once in a while to the one in use.
func (p *Path) run() {
	tick := time.NewTicker(probeInterval)
	defer tick.Stop()
	for {
		now := time.Now()
		if p.best.Load() != nil && now.Sub(time.Unix(0, p.heard.Load())) > silenceLimit {
			p.tracef("the other side fell silent")
			p.silent.Store(true)
			p.end()
			return
		}
		p.probe(now)
		select {
		case <-p.done:
			return
		case <-tick.C:
		}
	}
}

// probe sends the probes that are due. One that cannot leave from its
// address of this machine, which the machine no longer has, goes again from
// where the system says.
func (p *Path) probe(now time.Time) {
	for _, to := range p.due(now) {
		if err := p.t.probe(p, to); err != nil && to.local.IsValid() && gone(err) {
			p.unpin(to, err)
			p.t.probe(p, *newPair(netip.Addr{}, to.remote))
		}
	}
}

// gone reports whether a send failed for where it was to leave from, as
// when the machine no longer has that address. Systems tell it with several
// errors (EADDRNOTAVAIL, EINVAL, ENETUNREACH), so it is any error but a full
// buffer, which a burst of packets causes.
func gone(err error) bool {
	return err != nil && !errors.Is(err, syscall.ENOBUFS) && !errors.Is(err, syscall.EAGAIN)
}

// unpin lets the system pick where the probes of to.remote leave from, as
// to.local failed: the answer then tells which address to use.
func (p *Path) unpin(to pair, err error) {
	p.mu.Lock()
	if t := p.targets[to.remote]; t != nil && t.local == to.local {
		t.local = netip.Addr{}
	}
	p.mu.Unlock()
	p.tracef("cannot send from %s: %v; probing from where the system says", to.local, err)
}

// due returns the probes to send now. Those to an address that answers
// leave from where its answers come; the others, from where the system
// says, as nothing proves one address of this machine over another.
func (p *Path) due(now time.Time) []pair {
	p.mu.Lock()
	defer p.mu.Unlock()
	probe := func(addr netip.AddrPort) pair {
		var local netip.Addr
		if t := p.targets[addr]; t != nil && now.Sub(t.heard) <= staleLimit {
			local = t.local
		}
		return *newPair(local, addr)
	}
	best := p.best.Load()
	if best != nil {
		if !now.Before(p.nextSweep) {
			p.sweepUntil, p.nextSweep = now.Add(sweepTime), now.Add(p.t.sweepEvery)
		}
		if t := p.targets[best.remote]; t == nil || now.Sub(t.heard) > staleLimit {
			p.sweepUntil = now.Add(probeInterval)
		}
		if !now.Before(p.sweepUntil) {
			if now.Before(p.nextBeat) {
				return nil
			}
			p.nextBeat = now.Add(beatInterval)
			return []pair{probe(best.remote)}
		}
	}
	list := make([]pair, 0, len(p.targets))
	for addr := range p.targets {
		list = append(list, probe(addr))
	}
	return list
}

// Local returns the IP:port that this end sends from; * for the IP while
// the system picks it, as where the system does not say where packets
// arrive.
func (p *Path) Local() string {
	best := p.best.Load()
	if best == nil {
		return ""
	}
	s := p.t.socketFor(best.remote.Addr())
	switch {
	case s == nil:
		return ""
	case best.local.IsValid():
		return netip.AddrPortFrom(best.local, s.port).String()
	default:
		return fmt.Sprintf("*:%d", s.port)
	}
}

// Remote returns the IP:port that this end sends to now.
func (p *Path) Remote() string {
	if best := p.best.Load(); best != nil {
		return best.remote.String()
	}
	return ""
}

// Done is closed when the path ends.
func (p *Path) Done() <-chan struct{} { return p.done }

// FellSilent reports whether the path ended as nothing arrived for too
// long, rather than as a side closed it.
func (p *Path) FellSilent() bool { return p.silent.Load() }

// Idle returns how long ago this side last sent a packet on the path, or
// since the path started. Probes do not count.
func (p *Path) Idle() time.Duration {
	return time.Since(time.Unix(0, p.sent.Load()))
}

// WritePacket sends a WireGuard packet to the other side, between the pair
// of addresses in use. An error leaves the pair: the answers to probes
// decide it. macOS may drop a packet from an address it cannot send from
// without an error; the probes to the address then go unanswered, and once
// it is stale, leave from where the system says.
func (p *Path) WritePacket(packet []byte) error {
	best := p.best.Load()
	if best == nil {
		return errors.New("no address in use")
	}
	s := p.t.socketFor(best.remote.Addr())
	if s == nil {
		return errors.New("no socket for the address")
	}
	p.sent.Store(time.Now().UnixNano())
	return s.write(packet, best.control, best.remote)
}

// HandlePackets makes handle receive the WireGuard packets that arrive,
// on the goroutine that reads the socket: it must not block or keep the
// packet. Packets are not authenticated; WireGuard does that.
func (p *Path) HandlePackets(handle func(packet []byte)) {
	p.handle.Store(&handle)
}

func (p *Path) packet(packet []byte) {
	p.heard.Store(time.Now().UnixNano())
	if handle := p.handle.Load(); handle != nil {
		(*handle)(packet)
	}
}

// Close ends the path, telling the other side.
func (p *Path) Close() error {
	if best := p.best.Load(); best != nil && !p.ended() {
		if s := p.t.socketFor(best.remote.Addr()); s != nil {
			s.write(append([]byte{kindBye}, p.token[:]...), best.control, best.remote)
		}
	}
	p.end()
	return nil
}

func (p *Path) ended() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *Path) end() {
	p.once.Do(func() {
		close(p.done)
		p.t.mu.Lock()
		delete(p.t.tokens, p.token)
		p.t.mu.Unlock()
		p.t.disallow(func(_ netip.AddrPort, q *Path) bool { return q == p })
	})
}
