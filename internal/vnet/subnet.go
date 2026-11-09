package vnet

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// conflict returns what in n takes addresses of subnet: the network of an
// interface that shares addresses with it, or a route inside it. A wider
// route loses to the subnet's own, which is narrower, so it takes nothing;
// VPNs route all addresses in halves, and some route all private networks
// to the local router.
func conflict(subnet netip.Prefix, n Networks) (netip.Prefix, bool) {
	for _, p := range n.Interfaces {
		if p.Overlaps(subnet) {
			return p, true
		}
	}
	for _, p := range n.Routes {
		if p.Overlaps(subnet) && p.Bits() >= subnet.Bits() {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// overlaps reports whether any network in n shares addresses with subnet.
func overlaps(subnet netip.Prefix, n Networks) bool {
	return slices.ContainsFunc(slices.Concat(n.Interfaces, n.Routes), subnet.Overlaps)
}

// chooseSubnet finds a /16 that conflicts with nothing in used, and
// overlaps nothing if it can: a wide route may still lead to machines of
// some network. The choice depends on seed, so that machines of different
// meshes that see the same network do not all take the same subnet. It
// prefers 10.0.0.0/8, which has the most room, and leaves out the parts
// that home routers, containers, and VPNs often take.
func chooseSubnet(seed string, used []Networks) (netip.Prefix, bool) {
	sum := sha256.Sum256([]byte(seed))
	start := int(binary.BigEndian.Uint16(sum[:2]))
	var candidates []netip.Prefix
	for i := range 192 { // 10.64.0.0 to 10.255.0.0
		candidates = append(candidates, netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(64 + (start+i)%192), 0, 0}), 16))
	}
	for i := range 12 { // 172.20.0.0 to 172.31.0.0
		candidates = append(candidates, netip.PrefixFrom(netip.AddrFrom4([4]byte{172, byte(20 + (start+i)%12), 0, 0}), 16))
	}
	taken := func(c netip.Prefix, n Networks, strict bool) bool {
		if strict {
			return overlaps(c, n)
		}
		_, found := conflict(c, n)
		return found
	}
	for _, strict := range []bool{true, false} {
		for _, c := range candidates {
			if !slices.ContainsFunc(used, func(n Networks) bool { return taken(c, n, strict) }) {
				return c, true
			}
		}
	}
	return netip.Prefix{}, false
}

// chooseAddress picks an address for node id in subnet that is not in
// taken. It depends on id alone as far as it can, so that a node keeps its
// address when it loses its state.
func chooseAddress(id string, subnet netip.Prefix, taken map[netip.Addr]bool) (netip.Addr, error) {
	base := binary.BigEndian.Uint32(subnet.Addr().AsSlice())
	size := uint32(1) << (32 - subnet.Bits())
	for try := range 1 << 16 {
		sum := sha256.Sum256(fmt.Appendf(nil, "%s/%d", id, try))
		// Not the first and last, which are the network and its broadcast.
		offset := 1 + binary.BigEndian.Uint32(sum[:4])%(size-2)
		var raw [4]byte
		binary.BigEndian.PutUint32(raw[:], base+offset)
		if a := netip.AddrFrom4(raw); !taken[a] {
			return a, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("%s has no free address", subnet)
}

// newer reports whether choice a of the network's subnet beats b: the one
// with the higher Since (see wire.NetInfo), and for two with the same, either,
// as long as every node picks the same.
func newer(aSubnet string, aSince int64, bSubnet string, bSince int64) bool {
	if aSince != bSince {
		return aSince > bSince
	}
	return aSubnet < bSubnet
}

// hexKey writes a key as WireGuard's interface wants it.
func hexKey(key [32]byte) string { return hex.EncodeToString(key[:]) }

// usedNetworks reads the networks that a machine reported, ignoring what
// it should not have sent.
func usedNetworks(n wire.Networks) Networks {
	return Networks{Interfaces: usedPrefixes(n.Interfaces), Routes: usedPrefixes(n.Routes)}
}

func usedPrefixes(list []string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range list {
		if p, err := netip.ParsePrefix(s); err == nil && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}
