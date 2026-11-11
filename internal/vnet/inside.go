package vnet

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	wgtun "golang.zx2c4.com/wireguard/tun"

	"github.com/yangtau/ssh-tunnel/internal/tun"
)

// Some systems keep the sockets of the process that runs a VPN off the
// VPN's own interface, so that it cannot send to itself what it should
// carry: iOS does so for a packet tunnel. There the daemon can neither
// listen on its address in the network nor send from it, and the
// system's resolver asks only an address, on port 53. So inside does at
// the packets of the interface what sockets do elsewhere: it answers the
// DNS for .gst at an address that the interface routes, answers the
// pings of other nodes, and sends this node's own.

const (
	insideQueue    = 512  // packets on their way to WireGuard; more wait in the interface
	insideHeadroom = 16   // the room that devices want in front of a packet
	insidePacket   = 2048 // more than the interface's MTU
	// maxInsidePing is the largest packet of a ping from here: nothing
	// here cuts a packet into fragments.
	maxInsidePing = tun.MTU - 28
	// maxForwarding is how many queries for names outside .gst wait for
	// the system's resolver at once; more go unanswered, and the asker
	// tries again.
	maxForwarding = 64
)

// inside is the interface as WireGuard sees it on such a system.
type inside struct {
	wgtun.Device                                       // the interface
	own          func() netip.Addr                     // this node's address
	dns          netip.AddrPort                        // answers for .gst; zero for no DNS
	echo         uint16                                // the port that answers pings; 0 for none
	resolve      func(string) (netip.Addr, bool, bool) // as serveDNS takes it
	forward      func([]byte) ([]byte, error)          // answers for the names outside .gst; see Config.Forward
	forwarding   chan struct{}                         // a token for each query on its way to forward

	out    chan insidePacketBuf // from the interface and from here, to WireGuard
	pool   sync.Pool
	closed chan struct{} // closed by Close
	once   sync.Once
	ended  chan struct{} // closed when the interface gave its last packet
	err    error         // why it ended; set before ended is closed

	id    atomic.Uint32 // of the IP packets made here
	mu    sync.Mutex
	next  uint16
	pings map[uint16]*insidePing // by the port they went out from
}

type insidePacketBuf struct {
	buf    *[]byte
	length int
}

// insidePing is a ping on its way.
type insidePing struct {
	to      netip.Addr
	payload []byte
	back    chan struct{}
}

func newInside(device wgtun.Device, own func() netip.Addr, dns netip.AddrPort, echo uint16, resolve func(string) (netip.Addr, bool, bool), forward func([]byte) ([]byte, error)) *inside {
	d := &inside{
		Device: device, own: own, dns: dns, echo: echo, resolve: resolve,
		forward: forward, forwarding: make(chan struct{}, maxForwarding),
		out:    make(chan insidePacketBuf, insideQueue),
		closed: make(chan struct{}),
		ended:  make(chan struct{}),
		pings:  map[uint16]*insidePing{},
	}
	d.pool.New = func() any {
		buf := make([]byte, insidePacket)
		return &buf
	}
	go d.pump()
	return d
}

// pump reads the interface, so that Read can wait for its packets and for
// the ones made here at once.
func (d *inside) pump() {
	defer close(d.ended)
	count := max(d.Device.BatchSize(), 1)
	bufs, sizes := make([][]byte, count), make([]int, count)
	for i := range bufs {
		bufs[i] = make([]byte, insideHeadroom+insidePacket)
	}
	for {
		n, err := d.Device.Read(bufs, sizes, insideHeadroom)
		for i := range n {
			packet := bufs[i][insideHeadroom : insideHeadroom+sizes[i]]
			if len(packet) == 0 || d.answerDNS(packet) {
				continue
			}
			select {
			case d.out <- d.buffer(packet):
			case <-d.closed:
			}
		}
		if err != nil {
			d.err = err
			return
		}
	}
}

func (d *inside) buffer(packet []byte) insidePacketBuf {
	buf := d.pool.Get().(*[]byte)
	return insidePacketBuf{buf, copy(*buf, packet)}
}

// send hands WireGuard a packet made here, or drops it.
func (d *inside) send(packet []byte) {
	p := d.buffer(packet)
	select {
	case d.out <- p:
	default:
		d.pool.Put(p.buf)
	}
}

func (d *inside) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	var p insidePacketBuf
	select {
	case p = <-d.out:
	case <-d.ended:
		return 0, d.err
	}
	count := 0
	for {
		sizes[count] = copy(bufs[count][offset:], (*p.buf)[:p.length])
		d.pool.Put(p.buf)
		count++
		if count == len(bufs) {
			return count, nil
		}
		select {
		case p = <-d.out:
		default:
			return count, nil
		}
	}
}

func (d *inside) Write(bufs [][]byte, offset int) (int, error) {
	for i, buf := range bufs {
		if !d.take(buf[offset:]) {
			continue
		}
		// Rare: the rest goes on without it.
		rest := append(make([][]byte, 0, len(bufs)-1), bufs[:i]...)
		for _, buf := range bufs[i+1:] {
			if !d.take(buf[offset:]) {
				rest = append(rest, buf)
			}
		}
		if len(rest) > 0 {
			if _, err := d.Device.Write(rest, offset); err != nil {
				return 0, err
			}
		}
		return len(bufs), nil
	}
	return d.Device.Write(bufs, offset)
}

func (d *inside) Close() error {
	d.once.Do(func() { close(d.closed) })
	return d.Device.Close()
}

// answerDNS answers a packet from this machine if it is a query to the
// DNS address, and reports whether the packet was for that address.
func (d *inside) answerDNS(packet []byte) bool {
	if !d.dns.IsValid() {
		return false
	}
	u, ok := parseUDP(packet)
	if !ok || u.to != d.dns {
		return false
	}
	if d.forward != nil && outside(u.payload) {
		select {
		case d.forwarding <- struct{}{}:
			query := bytes.Clone(u.payload)
			go func() {
				defer func() { <-d.forwarding }()
				if reply, err := d.forward(query); err == nil && len(reply) > 0 {
					d.reply(u, reply)
				}
			}()
		default:
		}
		return true
	}
	if reply := answer(u.payload, d.resolve); reply != nil {
		d.reply(u, reply)
	}
	return true
}

// reply writes the answer to the DNS query u into the interface.
func (d *inside) reply(u udpPacket, reply []byte) {
	out := make([]byte, insideHeadroom, insideHeadroom+28+len(reply))
	out = d.appendUDP(out, u.to, u.from, reply)
	d.Device.Write([][]byte{out}, insideHeadroom)
}

// take handles a packet from another node if it is for here: a ping to
// answer, or the answer to a ping from here. It reports whether it was.
func (d *inside) take(packet []byte) bool {
	u, ok := parseUDP(packet)
	if !ok || d.echo == 0 || u.to.Addr() != d.own() {
		return false
	}
	switch {
	case u.to.Port() == d.echo:
		d.send(d.appendUDP(nil, u.to, u.from, u.payload))
		return true
	case u.from.Port() == d.echo:
		d.mu.Lock()
		p := d.pings[u.to.Port()]
		d.mu.Unlock()
		if p == nil || p.to != u.from.Addr() || !bytes.Equal(p.payload, u.payload) {
			return false
		}
		select {
		case p.back <- struct{}{}:
		default:
		}
		return true
	}
	return false
}

// ping sends a packet of size bytes to the node at address and waits for
// it to come back, as Manager.Ping does through a socket.
func (d *inside) ping(ctx context.Context, address string, size int) (time.Duration, error) {
	to, err := netip.ParseAddr(address)
	if err != nil {
		return 0, err
	}
	if size <= 0 {
		size = pingSize
	}
	if size > maxInsidePing {
		return 0, fmt.Errorf("a ping from this machine can be at most %d bytes", maxInsidePing)
	}
	own := d.own()
	if !own.IsValid() || d.echo == 0 {
		return 0, errors.New("this machine has no address in the network yet")
	}
	start := time.Now()
	if to == own {
		return time.Since(start), nil // It never leaves the machine.
	}
	p := &insidePing{to: to, payload: make([]byte, size), back: make(chan struct{}, 1)}
	rand.Read(p.payload)
	d.mu.Lock()
	for range 1 << 14 {
		d.next++
		if d.pings[49152+d.next%(1<<14)] == nil {
			break
		}
	}
	port := 49152 + d.next%(1<<14)
	d.pings[port] = p
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.pings, port)
		d.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	d.send(d.appendUDP(nil, netip.AddrPortFrom(own, port), netip.AddrPortFrom(to, d.echo), p.payload))
	select {
	case <-p.back:
		return time.Since(start), nil
	case <-d.ended:
		return 0, errors.New("the network went down")
	case <-ctx.Done():
		return 0, fmt.Errorf("timed out after %s", pingTimeout)
	}
}

// udpPacket is a UDP packet over IPv4.
type udpPacket struct {
	from, to netip.AddrPort
	payload  []byte
}

// parseUDP reads a whole UDP packet over IPv4; fragments are none.
func parseUDP(packet []byte) (udpPacket, bool) {
	if len(packet) < 28 || packet[0]>>4 != 4 || packet[9] != 17 {
		return udpPacket{}, false
	}
	header := int(packet[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(packet[2:]))
	if header < 20 || total > len(packet) || total < header+8 || binary.BigEndian.Uint16(packet[6:])&0x3fff != 0 {
		return udpPacket{}, false
	}
	udp := packet[header:total]
	if length := int(binary.BigEndian.Uint16(udp[4:])); length < 8 || length > len(udp) {
		return udpPacket{}, false
	} else {
		udp = udp[:length]
	}
	return udpPacket{
		from:    netip.AddrPortFrom(netip.AddrFrom4([4]byte(packet[12:16])), binary.BigEndian.Uint16(udp[0:])),
		to:      netip.AddrPortFrom(netip.AddrFrom4([4]byte(packet[16:20])), binary.BigEndian.Uint16(udp[2:])),
		payload: udp[8:],
	}, true
}

// appendUDP appends a UDP packet over IPv4 to b.
func (d *inside) appendUDP(b []byte, from, to netip.AddrPort, payload []byte) []byte {
	start := len(b)
	src, dst := from.Addr().As4(), to.Addr().As4()
	b = append(b, 0x45, 0)
	b = binary.BigEndian.AppendUint16(b, uint16(28+len(payload)))
	b = binary.BigEndian.AppendUint16(b, uint16(d.id.Add(1)))
	b = append(b, 0x40, 0, 64, 17, 0, 0) // do not fragment; TTL; UDP; the checksum
	b = append(append(b, src[:]...), dst[:]...)
	ip := b[start:]
	binary.BigEndian.PutUint16(ip[10:], ^checksum(0, ip))

	b = binary.BigEndian.AppendUint16(b, from.Port())
	b = binary.BigEndian.AppendUint16(b, to.Port())
	b = binary.BigEndian.AppendUint16(b, uint16(8+len(payload)))
	b = append(append(b, 0, 0), payload...)
	udp := b[start+20:]
	// The checksum covers the addresses, the protocol, and the length too.
	sum := checksum(uint32(17)+uint32(len(udp)), b[start+12:start+20])
	if sum = ^checksum(uint32(sum), udp); sum == 0 {
		sum = 0xffff
	}
	binary.BigEndian.PutUint16(udp[6:], sum)
	return b
}

// checksum is the internet checksum of data, before its complement,
// starting from sum.
func checksum(sum uint32, data []byte) uint16 {
	for ; len(data) >= 2; data = data[2:] {
		sum += uint32(binary.BigEndian.Uint16(data))
	}
	if len(data) == 1 {
		sum += uint32(data[0]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return uint16(sum)
}
