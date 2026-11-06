package vnet

import (
	"math/bits"
	"net"
	"net/netip"
	"syscall"

	"golang.org/x/net/route"
)

// routes returns the IPv4 routes of this machine that do not go through
// the interface called skip.
func routes(skip string) []netip.Prefix {
	raw, err := route.FetchRIB(syscall.AF_INET, route.RIBTypeRoute, 0)
	if err != nil {
		return nil
	}
	messages, err := route.ParseRIB(route.RIBTypeRoute, raw)
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for _, m := range messages {
		r, ok := m.(*route.RouteMessage)
		if !ok || len(r.Addrs) <= syscall.RTAX_NETMASK {
			continue
		}
		if skip != "" {
			if i, err := net.InterfaceByIndex(r.Index); err == nil && i.Name == skip {
				continue
			}
		}
		dst, ok := r.Addrs[syscall.RTAX_DST].(*route.Inet4Addr)
		if !ok {
			continue
		}
		length := 32 // host routes carry no mask
		if mask, ok := r.Addrs[syscall.RTAX_NETMASK].(*route.Inet4Addr); ok {
			length = 0
			for _, b := range mask.IP {
				length += bits.OnesCount8(b)
			}
		}
		out = append(out, netip.PrefixFrom(netip.AddrFrom4(dst.IP), length))
	}
	return out
}
