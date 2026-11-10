package vnet

import (
	"net/netip"
	"sync/atomic"

	wgtun "golang.zx2c4.com/wireguard/tun"
)

// loopback is the interface as WireGuard sees it: it sends packets for this
// node's own address straight back in. Linux delivers those itself, but
// macOS routes the address of a utun interface into the interface, and
// WireGuard has no peer for it, so nothing on the machine could reach its
// own address, by name or with gst ping.
type loopback struct {
	wgtun.Device
	address atomic.Pointer[netip.Addr]
}

// setAddress says what this node's address is.
func (l *loopback) setAddress(text string) {
	address, err := netip.ParseAddr(text)
	if err != nil {
		address = netip.Addr{}
	}
	l.address.Store(&address)
}

// own returns this node's address; the zero address while it has none.
func (l *loopback) own() netip.Addr {
	if address := l.address.Load(); address != nil {
		return *address
	}
	return netip.Addr{}
}

func (l *loopback) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	n, err := l.Device.Read(bufs, sizes, offset)
	for i := range n {
		if l.isOwn(bufs[i][offset : offset+sizes[i]]) {
			l.Device.Write([][]byte{bufs[i][:offset+sizes[i]]}, offset)
			sizes[i] = 0 // WireGuard passes over it.
		}
	}
	return n, err
}

// isOwn says whether an IPv4 packet is for this machine.
func (l *loopback) isOwn(packet []byte) bool {
	address := l.address.Load()
	return address != nil && len(packet) >= 20 && packet[0]>>4 == 4 &&
		netip.AddrFrom4([4]byte(packet[16:20])) == *address
}
