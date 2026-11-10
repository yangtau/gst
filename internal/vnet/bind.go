package vnet

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"

	"golang.zx2c4.com/wireguard/conn"
)

const (
	inboundQueue = 2048 // packets on their way to WireGuard; more are dropped
	// maxInbound is more than WireGuard makes of a packet of the
	// interface's MTU, which is all that nodes send each other. The
	// queue holds that much of each packet, and a phone gives a VPN
	// little memory.
	maxInbound = 2048
)

// endpoint is a peer as WireGuard sees it: a node of the mesh, not an
// address. The mesh knows how to reach the node now, so WireGuard never
// has to follow a peer that moves between UDP and SSH.
type endpoint string

func (e endpoint) ClearSrc()           {}
func (e endpoint) SrcToString() string { return "" }
func (e endpoint) DstToString() string { return string(e) }
func (e endpoint) DstToBytes() []byte  { return []byte(e) }
func (e endpoint) DstIP() netip.Addr   { return netip.Addr{} }
func (e endpoint) SrcIP() netip.Addr   { return netip.Addr{} }

type inbound struct {
	from   endpoint
	buf    *[]byte
	length int
}

// bind connects WireGuard to the mesh. It is WireGuard's network: what
// WireGuard sends goes to mesh.SendPacket, and what the mesh hands to
// deliver comes out of the receive function.
type bind struct {
	send    func(node string, packet []byte)
	dropped *atomic.Uint64 // packets that found the queue full
	in      chan inbound
	pool    sync.Pool

	mu   sync.Mutex
	done chan struct{} // closed by Close; nil while closed
}

func newBind(send func(node string, packet []byte), dropped *atomic.Uint64) *bind {
	b := &bind{send: send, dropped: dropped, in: make(chan inbound, inboundQueue)}
	b.pool.New = func() any {
		buf := make([]byte, maxInbound)
		return &buf
	}
	return b
}

// deliver hands WireGuard a packet from node, or drops it.
func (b *bind) deliver(node string, packet []byte) {
	if len(packet) > maxInbound {
		b.dropped.Add(1)
		return
	}
	buf := b.pool.Get().(*[]byte)
	length := copy(*buf, packet)
	select {
	case b.in <- inbound{endpoint(node), buf, length}:
	default:
		b.pool.Put(buf)
		b.dropped.Add(1)
	}
}

func (b *bind) Open(uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done != nil {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	done := make(chan struct{})
	b.done = done
	receive := func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		var p inbound
		select {
		case p = <-b.in:
		case <-done:
			return 0, net.ErrClosed
		}
		count := 0
		for {
			sizes[count] = copy(packets[count], (*p.buf)[:p.length])
			eps[count] = p.from
			b.pool.Put(p.buf)
			count++
			if count == len(packets) {
				return count, nil
			}
			select {
			case p = <-b.in:
			default:
				return count, nil
			}
		}
	}
	return []conn.ReceiveFunc{receive}, 1, nil
}

func (b *bind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done != nil {
		close(b.done)
		b.done = nil
	}
	return nil
}

func (b *bind) SetMark(uint32) error { return nil }

func (b *bind) Send(bufs [][]byte, ep conn.Endpoint) error {
	e, ok := ep.(endpoint)
	if !ok {
		return conn.ErrWrongEndpointType
	}
	for _, packet := range bufs {
		b.send(string(e), packet)
	}
	return nil
}

func (b *bind) ParseEndpoint(s string) (conn.Endpoint, error) {
	if s == "" {
		return nil, errors.New("empty endpoint")
	}
	return endpoint(s), nil
}

func (b *bind) BatchSize() int { return conn.IdealBatchSize }
