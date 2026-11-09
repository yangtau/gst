package vnet

import (
	"net"
	"net/netip"
	"slices"
)

// Networks are the IPv4 networks in use on a machine, which the subnet
// must leave alone (see conflict).
type Networks struct {
	// Interfaces are the networks of its interfaces, where other machines
	// may have any of the addresses.
	Interfaces []netip.Prefix
	// Routes are the destinations of its routes, which may be as wide as
	// half of all addresses, as VPNs route them.
	Routes []netip.Prefix
}

// LocalNetworks returns the networks of this machine, apart from those of
// the interface called skip, which is the virtual network's own.
func LocalNetworks(skip string) Networks {
	var out Networks
	add := func(list *[]netip.Prefix, p netip.Prefix) {
		p = p.Masked()
		a := p.Addr()
		// A default route says nothing of the addresses it leads to, and
		// the rest are never private addresses.
		if !a.Is4() || p.Bits() == 0 || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || slices.Contains(*list, p) {
			return
		}
		*list = append(*list, p)
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
				add(&out.Interfaces, netip.PrefixFrom(p.Addr().Unmap(), p.Bits()))
			}
		}
	}
	for _, p := range routes(skip) {
		add(&out.Routes, p)
	}
	return out
}
