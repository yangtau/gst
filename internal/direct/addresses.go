package direct

import (
	"net"
	"net/netip"
)

// maxPerFamily bounds the addresses of an offer, for machines with many
// temporary IPv6 addresses.
const maxPerFamily = 8

// Addresses returns this machine's addresses to offer. Any of them may
// turn out to be a way, a VPN's included; only the virtual network's own
// must stay out, which the Transport's skip sees to.
func Addresses() []netip.Addr {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var v4, v6 []netip.Addr
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
				v4 = append(v4, addr)
			default:
				v6 = append(v6, addr)
			}
		}
	}
	return append(v4[:min(len(v4), maxPerFamily)], v6[:min(len(v6), maxPerFamily)]...)
}
