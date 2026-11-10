package config

import (
	"errors"
	"net/netip"
	"strconv"
	"strings"
)

// Limits on the subnet of the virtual network: it has room for the
// nodes of a mesh, and it cannot take over the routes of a whole machine.
const (
	minSubnetBits = 8
	maxSubnetBits = 28
)

var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
}

// ParseSubnet reads the subnet of the virtual network, an IPv4 prefix
// inside the private ranges (RFC 1918 and RFC 6598).
func ParseSubnet(spec string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(spec)
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, errors.New("invalid subnet " + quote(spec) + ": want an IPv4 prefix such as 10.77.0.0/16")
	}
	p = p.Masked()
	if p.Bits() < minSubnetBits || p.Bits() > maxSubnetBits {
		return netip.Prefix{}, errors.New("invalid subnet " + quote(spec) + ": its prefix length must be between 8 and 28")
	}
	for _, r := range privateRanges {
		if r.Bits() <= p.Bits() && r.Contains(p.Addr()) {
			return p, nil
		}
	}
	return netip.Prefix{}, errors.New("invalid subnet " + quote(spec) + ": use a private range: 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, or 100.64.0.0/10")
}

// ParseNodeRef reads a reference to a node of the network, as a person
// gives it: its name, id, or id prefix, optionally followed by ".gst".
func ParseNodeRef(ref string) (string, error) {
	name := strings.TrimSuffix(strings.ToLower(ref), ".gst")
	if !namePattern.MatchString(name) {
		return "", errors.New("invalid machine " + quote(name) + ": use a name or id from gst status")
	}
	return name, nil
}

func quote(s string) string { return strconv.Quote(s) }
