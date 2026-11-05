package direct

import (
	"net"
	"net/netip"
)

// maxPerFamily bounds the sockets of an attempt, for machines with many
// temporary IPv6 addresses.
const maxPerFamily = 8

// Addresses returns this machine's addresses that a direct connection can
// use. It leaves out point-to-point interfaces, which are VPNs and proxies
// rather than a way of their own to the other machine.
func Addresses() []netip.Addr {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var v4, v6 []netip.Addr
	for _, i := range interfaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
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
				v4 = append(v4, addr)
			default:
				v6 = append(v6, addr)
			}
		}
	}
	return append(v4[:min(len(v4), maxPerFamily)], v6[:min(len(v6), maxPerFamily)]...)
}
