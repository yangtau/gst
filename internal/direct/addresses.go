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

// Addresses returns this machine's addresses to offer: those of its
// interfaces, but not of a tunnel's, such as a VPN's or a proxy's. When
// one takes the routes, the sockets send around it (see defaultInterfaces),
// so nothing would answer from them; and the virtual network's own, which
// the Transport's skip leaves out, would carry the path itself.
//
// The address that packets leave from comes first, and is never cut off:
// the first probes leave from it, and a firewall on the other side lets
// through only what comes from an address it sent to. It stays on a
// point-to-point interface, which may be the way out, as a phone's
// cellular one is.
func Addresses() []netip.Addr {
	// Android keeps the list of interfaces from apps; the address that
	// packets leave from is all there is to offer there.
	interfaces, _ := net.Interfaces()
	index4, index6 := defaultInterfaces()
	v4 := sendingAddress("udp4", internet4, index4)
	v6 := sendingAddress("udp6", internet6, index6)
	for _, i := range interfaces {
		if i.Flags&net.FlagUp == 0 || tunnel(i) {
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

// internet4 and internet6 stand for the internet, at the port of UDP paths,
// which the rules of a proxy may name: where packets to them go is where
// the packets of UDP paths go.
const (
	internet4 = "203.0.113.1:15355"
	internet6 = "[2001:db8::1]:15355"
)

// sendingAddress asks the system where packets to target, an address on the
// internet, would leave from, through the interface at index unless it is
// 0. Connecting a UDP socket sends nothing.
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
	if i := interfaceOf(addr); index != 0 && (i == nil || i.Index != index) {
		// A kernel that does not apply the interface to a connected
		// socket reports the address that the routes pick, a tunnel's;
		// the sockets send from the interface's own.
		addr = firstAddress(index, addr.Is4())
	}
	if !addr.IsGlobalUnicast() {
		return nil
	}
	return []netip.Addr{addr}
}

// intoTunnel reports whether the routes send packets to target, an address
// on the internet, into a tunnel, as when a VPN or a proxy took them.
func intoTunnel(network, target string) bool {
	addrs := sendingAddress(network, target, 0)
	if len(addrs) == 0 {
		return false
	}
	i := interfaceOf(addrs[0])
	return i != nil && tunnel(*i)
}

// interfaceOf returns the interface that has addr; nil when none has.
func interfaceOf(addr netip.Addr) *net.Interface {
	interfaces, _ := net.Interfaces()
	for _, i := range interfaces {
		if slices.Contains(interfaceAddresses(i), addr) {
			return &i
		}
	}
	return nil
}

// firstAddress returns the first global address of the interface at index,
// of IPv4 or of IPv6; not valid when it has none.
func firstAddress(index int, v4 bool) netip.Addr {
	i, err := net.InterfaceByIndex(index)
	if err != nil {
		return netip.Addr{}
	}
	for _, addr := range interfaceAddresses(*i) {
		if addr.Is4() == v4 && addr.IsGlobalUnicast() {
			return addr
		}
	}
	return netip.Addr{}
}

func interfaceAddresses(i net.Interface) []netip.Addr {
	addrs, err := i.Addrs()
	if err != nil {
		return nil
	}
	var list []netip.Addr
	for _, a := range addrs {
		if prefix, err := netip.ParsePrefix(a.String()); err == nil {
			list = append(list, prefix.Addr().Unmap())
		}
	}
	return list
}

// tunnel says whether an interface leads to another machine alone, as a
// VPN's does, rather than to a network; the loopback interface counts too.
func tunnel(i net.Interface) bool {
	return i.Flags&(net.FlagPointToPoint|net.FlagLoopback) != 0
}

// tunnelAt says whether the interface at index is a tunnel, or gone.
func tunnelAt(index int) bool {
	i, err := net.InterfaceByIndex(index)
	return err != nil || tunnel(*i)
}
