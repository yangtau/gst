package direct

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"syscall"
)

// maxPerFamily bounds the addresses of an offer, for machines with many
// temporary IPv6 addresses.
const maxPerFamily = 8

// Addresses returns this machine's addresses to offer. Any of them may
// turn out to be a way, a VPN's included; only the virtual network's own
// must stay out, which the Transport's skip sees to.
//
// The address that packets leave from comes first, and is never cut off:
// the sockets are not bound to an address, so answers leave from it whatever
// address the probe came to, and a firewall on the other side lets through
// only what comes from an address it sent to.
func Addresses() []netip.Addr {
	// Android keeps the list of interfaces from apps; the address that
	// packets leave from is all there is to offer there.
	interfaces, _ := net.Interfaces()
	index4, index6 := defaultInterfaces()
	v4 := sendingAddress("udp4", "203.0.113.1:9", index4)
	v6 := sendingAddress("udp6", "[2001:db8::1]:9", index6)
	for _, i := range interfaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			prefix, err := netip.ParsePrefix(a.String())
			if err != nil {
				continue
			}
			addr := prefix.Addr().Unmap()
			switch {
			case !addr.IsGlobalUnicast():
			case addr.Is4():
				if !slices.Contains(v4, addr) {
					v4 = append(v4, addr)
				}
			case !slices.Contains(v6, addr):
				v6 = append(v6, addr)
			}
		}
	}
	return append(v4[:min(len(v4), maxPerFamily)], v6[:min(len(v6), maxPerFamily)]...)
}

// sendingAddress asks the system where packets to target, an address on the
// internet, would leave from. Connecting a UDP socket sends nothing.
func sendingAddress(network, target string, index int) []netip.Addr {
	dialer := net.Dialer{ControlContext: func(_ context.Context, _, _ string, c syscall.RawConn) error {
		if index == 0 {
			return nil
		}
		return bindSocket(c, network, index)
	}}
	conn, err := dialer.Dial(network, target)
	if err != nil {
		return nil
	}
	defer conn.Close()
	addr := conn.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
	if !addr.IsGlobalUnicast() {
		return nil
	}
	return []netip.Addr{addr}
}
