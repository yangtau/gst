package vnet

import (
	"net"
	"net/netip"
	"slices"
)

// LocalNetworks returns the IPv4 prefixes in use on this machine: those
// of its interfaces and routes, apart from the interface called skip,
// which is the virtual network's own.
func LocalNetworks(skip string) []netip.Prefix {
	var out []netip.Prefix
	add := func(p netip.Prefix) {
		p = p.Masked()
		a := p.Addr()
		// A default route says nothing of the addresses it leads to, and
		// the rest are never private addresses.
		if !a.Is4() || p.Bits() == 0 || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || slices.Contains(out, p) {
			return
		}
		out = append(out, p)
	}
	interfaces, _ := net.Interfaces()
	for _, i := range interfaces {
		if i.Name == skip || i.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				add(netip.PrefixFrom(p.Addr().Unmap(), p.Bits()))
			}
		}
	}
	for _, p := range routes(skip) {
		add(p)
	}
	return out
}
