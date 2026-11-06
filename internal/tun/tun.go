// Package tun gives the gst daemon a network interface. Creating one,
// and the addresses, routes, and name resolution that go with it, take
// root, while the daemon runs as its user, so a small service does that
// part: the helper, which `gst net install` sets up. The daemon asks it for
// an interface with Dial, over a Unix socket that only its user can open,
// and gets the open device back; everything that handles packets stays in
// the daemon. The interface lives as long as that connection: when the
// daemon goes, the helper takes down what it set up.
//
// The helper does exactly what a Spec says after checking it, so a daemon
// cannot make it do more than number one interface inside the private
// ranges and resolve names under .gst.
package tun

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

const (
	// Socket is where the helper listens.
	Socket = "/var/run/gst-net.sock"
	// MTU makes every WireGuard packet, with its 32 bytes of overhead, a
	// UDP payload of at most 1200 bytes. QUIC needs a path to carry that
	// much, so it fits any direct link, without path MTU discovery. Direct
	// links do not fragment: a larger packet would be lost.
	MTU = 1168
	// DNSAddress is where the daemon answers for .gst, and where the
	// helper points the system's resolver.
	DNSAddress = "127.0.0.1:15353"
	dnsHost    = "127.0.0.1"
	dnsPort    = "15353"

	// MaxHosts is how many hosts a Spec can hold.
	MaxHosts = 4096
)

// Spec is what the interface should be.
type Spec struct {
	Subnet  string `json:"subnet"`  // routed to the interface
	Address string `json:"address"` // of this machine, inside Subnet
	// Hosts maps names under .gst to their addresses, for systems without
	// a resolver that can ask the daemon.
	Hosts map[string]string `json:"hosts,omitempty"`
}

// Reply answers a Spec.
type Reply struct {
	Interface string `json:"interface,omitempty"`
	Error     string `json:"error,omitempty"`
}

var hostPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}\.gst$`)

// ValidHost reports whether a Spec can hold name.
func ValidHost(name string) bool { return hostPattern.MatchString(name) }

// parsed is a Spec after checking.
type parsed struct {
	subnet  netip.Prefix
	address netip.Addr
	hosts   map[string]netip.Addr
}

func (s Spec) parse() (parsed, error) {
	subnet, err := config.ParseSubnet(s.Subnet)
	if err != nil {
		return parsed{}, err
	}
	address, err := netip.ParseAddr(s.Address)
	if err != nil || !subnet.Contains(address) {
		return parsed{}, fmt.Errorf("address %q is not in %s", s.Address, subnet)
	}
	if len(s.Hosts) > MaxHosts {
		return parsed{}, errors.New("too many hosts")
	}
	p := parsed{subnet: subnet, address: address, hosts: make(map[string]netip.Addr, len(s.Hosts))}
	for name, value := range s.Hosts {
		ip, err := netip.ParseAddr(value)
		if err != nil || !subnet.Contains(ip) || !hostPattern.MatchString(name) {
			return parsed{}, fmt.Errorf("invalid host %q", name)
		}
		p.hosts[name] = ip
	}
	return p, nil
}
