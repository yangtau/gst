package direct

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
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
// pair of addresses in use. It starts as a try, and carries packets once the
// two sides confirmed a pair, until it falls silent or a side closes it.
//
// A pair is an address of this machine and one of the other side's. The two
// sides confirm it in three steps, which either may begin:
//
//  1. A probe goes one way and its answer comes back: the side that sent
//     the probe knows that the pair works both ways.
//  2. Its next probe of the pair says so (flagWorks). The other side, which
//     gets it, knows the same, as its answer arrived, and that the first
//     side knows. It may send packets on the pair from now on.
//  3. It answers, and the answer repeats what the probe said. The first
//     side, which gets it, knows that the other knows, and may send too.
//
// The pair is confirmed then. So a side begins to send only on a pair that
// carried packets both ways and that the other side knows of, which is
// then ready for the packets: a side hands on what arrives once it knows
// of a pair that works. A pair is the same for both, each seeing it from
// its end: the side that gets the probe of step 2 takes the address that
// it came to and the one that it came from. So one side's probes suffice:
// the side whose own probes all fail, as from an address that nothing
// returns to, learns the pair from the other's. Among the confirmed pairs,
// each side uses the one that serves it best.
//
// A pair that gave no sign for staleLimit is not known to work any more,
// and the two sides confirm it again. The pair in use carries the packets
// meanwhile, until another is confirmed or the path falls silent: it is
// the best guess, and the links take over once the path ends.
type Path struct {
	t      *Transport
	token  [tokenLen]byte
	trace  func(string)
	handle atomic.Pointer[func(packet []byte)]
	relay  atomic.Pointer[func(kind Relay, node [NodeLen]byte, packet []byte)]
	best   atomic.Pointer[pair]
	heard  atomic.Int64 // when anything last arrived, in Unix nanoseconds
	sent   atomic.Int64 // when this side last sent a packet, in Unix nanoseconds
	silent atomic.Bool  // the path ended as the other side fell silent
	works  atomic.Bool  // this side knew a pair to work; it hands on packets from then on

	confirmed chan struct{} // closed once a pair is confirmed
	done      chan struct{}
	wake      chan struct{} // makes run look again at once at what is due
	once      sync.Once

	mu         sync.Mutex // never held while calling trace
	targets    map[netip.AddrPort]*target
	running    bool
	sweepUntil time.Time
	nextSweep  time.Time
	nextBeat   time.Time
	sendErrors map[string]bool // the kinds of error of probes that did not leave, each told once
}

// target is an address of the other side.
type target struct {
	rtt       time.Duration // smoothed
	samples   int
	heard     time.Time  // of the latest sign that the pair works: an answer, or a probe that says so
	probed    bool       // a probe came from it
	local     netip.Addr // the address of this machine that makes the pair with it: where those signs arrive; not valid until one does
	confirmed bool       // both sides know that the pair works, so it may carry packets
}

// pair is what packets go between: an address of this machine and one of
// the other side's.
type pair struct {
	local   netip.Addr // not valid when the system picks it
	remote  netip.AddrPort
	control []byte // makes packets leave from local
}

// bestRemote returns the other side's address of the pair in use; not valid
// when there is none.
func bestRemote(best *pair) netip.AddrPort {
	if best == nil {
		return netip.AddrPort{}
	}
	return best.remote
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
		t:         t,
		token:     token,
		trace:     trace,
		confirmed: make(chan struct{}),
		done:      make(chan struct{}),
		wake:      make(chan struct{}, 1),
		targets:   map[netip.AddrPort]*target{},
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

// Connect probes the addresses of theirs, and returns once the two sides
// confirmed a pair, which the other side's probes may have done before, or
// fails when ctx is done first. The caller closes the path if it fails.
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
	} else {
		p.poke()
	}
	select {
	case <-p.confirmed:
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
// network, and finds the pairs from scratch.
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

// Remeasure makes the path find its pairs from scratch, after this side
// changed network: no pair is known to work any more, so the probes leave
// as at the start of a try, and the two sides confirm what answers. Packets
// keep the pair in use until then.
func (p *Path) Remeasure() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.remeasure()
}

func (p *Path) remeasure() {
	for _, t := range p.targets {
		t.samples = 0
		t.local = netip.Addr{}
		t.confirmed = false
		t.heard = time.Time{}
	}
	p.sweepUntil = time.Now().Add(sweepTime)
	p.poke()
}

// poke makes run look at once at what is due, after something changed it.
func (p *Path) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// probed handles a probe from source, which the token proves to be the
// other side, to local, the address of this machine: that is how a side
// learns the address of another behind NAT. A probe that says that the pair
// works, as an answer of this side came back there, confirms the pair.
func (p *Path) probed(source netip.AddrPort, local netip.Addr, works bool) {
	p.mu.Lock()
	t := p.targets[source]
	fresh, learned := t == nil || !t.probed, t == nil
	p.add(source).probed = true
	var events []string
	if works {
		events = p.record(source, local, noMeasure, true, time.Now())
	}
	p.mu.Unlock()
	p.t.allow(source, p)
	if works {
		p.heard.Store(time.Now().UnixNano())
	}
	switch {
	case fresh && learned:
		p.tracef("probe from %s: a new address, answering and probing it too", source)
	case fresh:
		p.tracef("probe from %s: answering", source)
	}
	for _, e := range events {
		p.tracef("%s", e)
	}
}

// answered handles the answer, from source to local, the address of this
// machine, to a probe sent to addr rtt ago. The address probed gets the
// credit, wherever the answer came from: a NAT on the other side may change
// it. Its answers then come to local, which makes the pair with it.
// confirmed says that the probe told that the pair works, which the other
// side then knows; otherwise a probe that tells it follows at once.
//
// The source becomes an address of the path like any other, so that the
// bound on those holds for sources too.
func (p *Path) answered(addr, source netip.AddrPort, local netip.Addr, rtt time.Duration, confirmed bool) {
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
	events := p.record(addr, local, rtt, confirmed, time.Now())
	t := p.targets[addr]
	tell := t != nil && !t.confirmed && (t.local == local || !local.IsValid())
	p.mu.Unlock()
	for _, e := range events {
		p.tracef("%s", e)
	}
	if tell {
		p.t.probe(p, *newPair(local, addr), true)
	}
}

// noMeasure is the round trip of a sign that measures none: a probe.
const noMeasure = -1

// record notes a sign that the pair of local and addr works both ways: an
// answer that took rtt, or, with noMeasure, a probe that says so. confirmed
// tells that the other side knows it too. A confirmed pair may carry
// packets, and is used if it is the better one; p.mu is held. It returns
// what happened, for the trace.
//
// An address that makes a pair with one address of this machine keeps it
// while the pair works: a sign at another, as the first probes leave from
// several, neither moves it nor counts as a measure. Once the pair stopped
// working, the next sign decides, and the two sides confirm it again.
func (p *Path) record(addr netip.AddrPort, local netip.Addr, rtt time.Duration, confirmed bool, now time.Time) []string {
	var events []string
	t := p.targets[addr]
	if t == nil {
		return nil
	}
	stale := now.Sub(t.heard) > staleLimit
	if local.IsValid() && t.local.IsValid() && local != t.local && !stale {
		return nil
	}
	if t.heard.IsZero() && rtt != noMeasure {
		events = append(events, fmt.Sprintf("answer from %s in %s: it works, telling the other side", addr, ms(rtt)))
	}
	if stale {
		t.confirmed = false
	}
	if local.IsValid() && (!t.local.IsValid() || stale) {
		t.local = local
	}
	if now.Sub(t.heard) > measureLife {
		t.samples = 0
	}
	if rtt != noMeasure {
		if t.samples == 0 {
			t.rtt = rtt
		} else {
			t.rtt += (rtt - t.rtt) / 4
		}
		t.samples++
	}
	t.heard = now
	p.works.Store(true)
	if confirmed && !t.confirmed {
		t.confirmed = true
		events = append(events, fmt.Sprintf("confirmed %s: both sides know that it works", newPair(t.local, addr)))
	}
	if !t.confirmed {
		return events
	}
	best, next := p.best.Load(), newPair(t.local, addr)
	switch {
	case best == nil:
		p.best.Store(next)
		p.sweepUntil, p.nextSweep = now.Add(sweepTime), now.Add(p.t.sweepEvery)
		close(p.confirmed)
		events = append(events, fmt.Sprintf("up: using %s", next))
	case best.remote == addr:
		if t.local.IsValid() && best.local != t.local {
			p.best.Store(next)
			events = append(events, fmt.Sprintf("using %s: the pair works there", next))
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
// try and a sweep, and otherwise once in a while to the one in use. In
// between it sleeps until something is due: each wakeup costs a phone
// battery, and a path in use needs one only for its beats.
func (p *Path) run() {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-timer.C:
		case <-p.wake:
		}
		now := time.Now()
		if p.best.Load() != nil && now.Sub(time.Unix(0, p.heard.Load())) > silenceLimit {
			p.tracef("the other side fell silent")
			p.silent.Store(true)
			p.end()
			return
		}
		p.probe(now)
		timer.Reset(p.wakeAt(now).Sub(now))
	}
}

// wakeAt returns when the path next has something to do, after the probes
// due at now went: probes again, at probeInterval during a try and a
// sweep, and otherwise the next beat or sweep, the time the address in use
// goes stale, which starts probing all the addresses, or the time the path
// falls silent. Those last two count from signs that may come before, which
// only makes it look early.
func (p *Path) wakeAt(now time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	best := p.best.Load()
	if best == nil || now.Before(p.sweepUntil) {
		return now.Add(probeInterval)
	}
	next := earliest(p.nextBeat, p.nextSweep, time.Unix(0, p.heard.Load()).Add(silenceLimit+time.Millisecond))
	if t := p.targets[best.remote]; t != nil {
		next = earliest(next, t.heard.Add(staleLimit+time.Millisecond))
	}
	if soonest := now.Add(time.Millisecond); next.Before(soonest) {
		return soonest // never in the past, which would spin
	}
	return next
}

// earliest returns the earliest of times.
func earliest(times ...time.Time) time.Time {
	first := times[0]
	for _, t := range times[1:] {
		if t.Before(first) {
			first = t
		}
	}
	return first
}

// probe sends the probes that are due. One that cannot leave from the
// address of this machine that the answers came to, which the machine no
// longer has, goes again from where the system says. One that tries an
// address of this machine fails in silence: only answers tell which serve.
// One from where the system says that cannot leave goes into the trace.
func (p *Path) probe(now time.Time) {
	for _, to := range p.due(now) {
		err := p.t.probe(p, to.pair, to.works)
		if err != nil && to.local.IsValid() && gone(err) && p.unpin(to.pair, err) {
			err = p.t.probe(p, *newPair(netip.Addr{}, to.remote), false)
			to.local = netip.Addr{}
		}
		if err != nil && !to.local.IsValid() {
			p.sendFailed(to.remote, err)
		}
	}
}

// sendFailed tells that a probe from where the system says did not leave,
// once for each kind of error, as several addresses fail alike: otherwise
// nothing tells a try that sent nothing from one whose probes got lost on
// the way.
func (p *Path) sendFailed(to netip.AddrPort, err error) {
	kind := err.Error()
	var op *net.OpError // which names the address, unlike its cause
	if errors.As(err, &op) {
		kind = op.Err.Error()
	}
	p.mu.Lock()
	news := !p.sendErrors[kind]
	if news {
		if p.sendErrors == nil {
			p.sendErrors = map[string]bool{}
		}
		p.sendErrors[kind] = true
	}
	p.mu.Unlock()
	if news {
		p.tracef("a probe to %s did not leave: %v", to, err)
	}
}

// gone reports whether a send failed for where it was to leave from, as
// when the machine no longer has that address. Systems tell it with several
// errors (EADDRNOTAVAIL, EINVAL, ENETUNREACH), so it is any error but a full
// buffer, which a burst of packets causes.
func gone(err error) bool {
	return err != nil && !errors.Is(err, syscall.ENOBUFS) && !errors.Is(err, syscall.EAGAIN)
}

// unpin gives up the pair of to, as nothing leaves from to.local: the
// probes of to.remote leave as at the start of a try, and an answer then
// tells which address makes a pair with it. It reports whether to was that
// pair; a probe that only tried to.local went along with one from where
// the system says.
func (p *Path) unpin(to pair, err error) bool {
	p.mu.Lock()
	t := p.targets[to.remote]
	pinned := t != nil && t.local == to.local
	if pinned {
		*t = target{rtt: t.rtt, samples: t.samples, probed: t.probed} // the pair is gone
	}
	p.mu.Unlock()
	if pinned {
		p.tracef("cannot send from %s: %v; probing from where the system says", to.local, err)
	}
	return pinned
}

// probe is a probe to send: between which addresses, and whether it says
// that the pair works.
type probe struct {
	pair
	works bool
}

// due returns the probes to send now. Those to an address whose pair works
// leave from the pair's address of this machine, and say so. The others
// leave from where the system says, which follows the routes, to a VPN that
// the machines share too.
//
// While the path has no address that answers, they also leave from each
// address of this machine: the system's pick may be one that nothing
// returns to, as an IPv6 address of a prefix that the network gave up, and
// it picks the same at every try. The first answer decides which address
// makes the pair, so a pair is one that carried a probe and its answer,
// whoever picked it. They also open the firewall in front of this machine
// for what the other side sends to each of its addresses.
func (p *Path) due(now time.Time) []probe {
	p.mu.Lock()
	defer p.mu.Unlock()
	var list []probe
	var locals []netip.Addr // to try besides the system's pick
	add := func(addr netip.AddrPort) {
		if t := p.targets[addr]; t != nil && now.Sub(t.heard) <= staleLimit {
			list = append(list, probe{*newPair(t.local, addr), true})
			return
		}
		list = append(list, probe{pair: *newPair(netip.Addr{}, addr)})
		for _, local := range locals {
			if local.Is4() == addr.Addr().Is4() {
				list = append(list, probe{pair: *newPair(local, addr)})
			}
		}
	}
	best := p.best.Load()
	if t := p.targets[bestRemote(best)]; t == nil || now.Sub(t.heard) > staleLimit {
		locals = *p.t.locals.Load()
	}
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
			add(best.remote)
			return list
		}
	}
	for addr := range p.targets {
		add(addr)
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

// Answering reports whether the other side answered on the path lately,
// which says that it still uses the path.
func (p *Path) Answering() bool {
	return p.best.Load() != nil && !p.ended() && time.Since(time.Unix(0, p.heard.Load())) <= staleLimit
}

// Remote returns the IP:port that this end sends to now.
func (p *Path) Remote() string {
	if best := p.best.Load(); best != nil {
		return best.remote.String()
	}
	return ""
}

// RTT returns the round trip between the pair of addresses in use, smoothed;
// false while there is no pair, or nothing measured it yet, as when the
// other side's probes confirmed it.
func (p *Path) RTT() (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.targets[bestRemote(p.best.Load())]
	if t == nil || t.samples == 0 {
		return 0, false
	}
	return t.rtt, true
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

// WriteRelay sends a WireGuard packet that a relay carries to the other
// side, as WritePacket does: with RelayTo, to the relay there, for node,
// and with RelayFrom, from the relay here, as node sent it.
func (p *Path) WriteRelay(kind Relay, node [NodeLen]byte, packet []byte) error {
	relayed := make([]byte, 0, 1+NodeLen+len(packet))
	relayed = append(append(append(relayed, byte(kind)), node[:]...), packet...)
	return p.WritePacket(relayed)
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
// packet. Packets are not authenticated; WireGuard does that. They arrive
// once this side knows a pair to work, when the other side may have
// confirmed it and send. That may be before Connect returns, so the caller
// sets handle before it sends its offer or answer.
func (p *Path) HandlePackets(handle func(packet []byte)) {
	p.handle.Store(&handle)
}

func (p *Path) packet(packet []byte) {
	if !p.works.Load() {
		return // The other side sends only on a pair that this side knows too.
	}
	p.heard.Store(time.Now().UnixNano())
	if handle := p.handle.Load(); handle != nil {
		(*handle)(packet)
	}
}

// HandleRelay makes handle receive the packets that a relay carries, with
// their kind and the node that they name, as HandlePackets does WireGuard's.
func (p *Path) HandleRelay(handle func(kind Relay, node [NodeLen]byte, packet []byte)) {
	p.relay.Store(&handle)
}

func (p *Path) relayed(kind Relay, node [NodeLen]byte, packet []byte) {
	if !p.works.Load() {
		return
	}
	p.heard.Store(time.Now().UnixNano())
	if handle := p.relay.Load(); handle != nil {
		(*handle)(kind, node, packet)
	}
}

// Close ends the path, telling the other side: at the address in use, or,
// when the try found none, wherever its probes came from, so that the other
// side stops trying too. Nothing proved an address of this machine then, so
// the system picks where that leaves from.
func (p *Path) Close() error {
	if !p.ended() {
		for _, to := range p.farewells() {
			if s := p.t.socketFor(to.remote.Addr()); s != nil {
				s.write(append([]byte{kindBye}, p.token[:]...), to.control, to.remote)
			}
		}
	}
	p.end()
	return nil
}

// farewells returns where to tell the other side that the path ends.
func (p *Path) farewells() []pair {
	if best := p.best.Load(); best != nil {
		return []pair{*best}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var list []pair
	for addr, t := range p.targets {
		if t.probed {
			list = append(list, *newPair(netip.Addr{}, addr))
		}
	}
	return list
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
