package config

import (
	"errors"
	"net/netip"
	"strconv"
	"strings"
)

// Map listens on this machine and connects each connection to Target on
// Machine through the mesh.
type Map struct {
	Listen  string `json:"listen"`  // IP:port or socket path on this machine
	Machine string `json:"machine"` // a machine of the mesh; see ParseMachine
	Target  string `json:"target"`  // host:port or socket path on Machine
}

func (m Map) validate() error {
	if _, ok := parseListenAddress(m.Listen); !ok && !isSocketPath(m.Listen) {
		return errors.New("invalid listen address")
	}
	if !namePattern.MatchString(m.Machine) {
		return errors.New("invalid machine")
	}
	if !ValidTarget(Network(m.Target), m.Target) {
		return errors.New("invalid target")
	}
	return nil
}

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

// ParseMachine reads a reference to a machine of the mesh: a node name,
// id, id prefix, or link name, optionally followed by ".gst".
func ParseMachine(ref string) (string, error) {
	machine := strings.TrimSuffix(strings.ToLower(ref), ".gst")
	if !namePattern.MatchString(machine) {
		return "", errors.New("invalid machine " + quote(ref) + ": use a name or id from gst status")
	}
	return machine, nil
}

// ParseMachineTarget reads "<machine>:<target>", such as devbox:5432,
// devbox:db.internal:5432, or devbox:/run/app.sock.
func ParseMachineTarget(spec string) (machine, target string, err error) {
	ref, rest, ok := strings.Cut(spec, ":")
	if !ok {
		return "", "", errors.New("invalid target " + quote(spec) + ": want <machine>:<port | host:port | socket>")
	}
	if machine, err = ParseMachine(ref); err != nil {
		return "", "", err
	}
	if target, err = ParseTarget(rest); err != nil {
		return "", "", err
	}
	return machine, target, nil
}

// DefaultRoute matches every host.
const DefaultRoute = "default"

// Route sends the connections to the hosts that Match matches out through
// the machine Via, which must be an exit.
type Route struct {
	Match string `json:"match"` // "default", a domain, an IP, or a CIDR prefix
	Via   string `json:"via"`
}

// ParseRouteMatch reads what a route matches: "default", a domain, which
// also matches its subdomains, or an IP address or CIDR prefix, which
// match IP addresses only.
func ParseRouteMatch(spec string) (string, error) {
	m := strings.TrimSuffix(strings.ToLower(spec), ".")
	if m == DefaultRoute {
		return m, nil
	}
	if p, err := netip.ParsePrefix(m); err == nil {
		return p.Masked().String(), nil
	}
	if a, ok := parseIP(m); ok {
		return a.String(), nil
	}
	if isHostname(m) {
		return m, nil
	}
	return "", errors.New("invalid match " + quote(spec) + ": use default, a domain such as example.com (which includes its subdomains), an IP address, or a CIDR prefix such as 10.0.0.0/8")
}

func (r Route) validate() error {
	if m, err := ParseRouteMatch(r.Match); err != nil || m != r.Match {
		return errors.New("invalid match")
	}
	if !namePattern.MatchString(r.Via) {
		return errors.New("invalid machine")
	}
	return nil
}

// Routes are the routes of a configuration.
type Routes []Route

// Lookup returns the machine through which to connect to host, a domain
// name or IP address, by the most specific route that matches it.
func (rs Routes) Lookup(host string) (via string, ok bool) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")), ".")
	ip, isIP := parseIP(host)
	best := -1
	for _, r := range rs {
		if s := r.specificity(host, ip, isIP); s > best {
			best, via = s, r.Via
		}
	}
	return via, best >= 0
}

// specificity ranks how closely r matches host: -1 when it does not, and
// higher for longer domains and prefixes.
func (r Route) specificity(host string, ip netip.Addr, isIP bool) int {
	switch {
	case r.Match == DefaultRoute:
		return 0
	case isIP:
		p, err := netip.ParsePrefix(r.Match)
		if err != nil {
			a, ok := parseIP(r.Match)
			if !ok {
				return -1
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p.Contains(ip.Unmap()) || p.Contains(ip) {
			return 1 + p.Bits()
		}
	case host == r.Match || strings.HasSuffix(host, "."+r.Match):
		return 1 + len(r.Match)
	}
	return -1
}

func quote(s string) string { return strconv.Quote(s) }
