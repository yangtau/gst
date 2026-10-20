package config

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Forward relays connections accepted on Listen to Target. A Remote
// forward listens on the server and dials Target from this machine, like
// "ssh -R"; a Local forward does the reverse, like "ssh -L".
type Forward struct {
	Direction string `json:"direction"` // Remote or Local
	Listen    string `json:"listen"`    // IP:port bound on the listening side
	Target    string `json:"target"`    // host:port dialed from the other side
}

// Forward directions.
const (
	Remote = "remote"
	Local  = "local"
)

// ListenAddr returns the parsed Listen address; it is valid once the
// forward has passed validation.
func (f Forward) ListenAddr() netip.AddrPort {
	addr, _ := parseListenAddress(f.Listen)
	return addr
}

// Flag returns the ssh option for f's direction, "-R" or "-L".
func (f Forward) Flag() string {
	if f.Direction == Remote {
		return "-R"
	}
	return "-L"
}

// Spec formats f the way ssh takes it, such as "-R 0.0.0.0:2222:127.0.0.1:22".
func (f Forward) Spec() string {
	return f.Flag() + " " + f.Listen + ":" + f.Target
}

// SameListener reports whether f and g listen on the same address on the
// same side, treating ::ffff:a.b.c.d and a.b.c.d as equal.
func (f Forward) SameListener(g Forward) bool {
	return f.Direction == g.Direction && listenerKey(f.ListenAddr()) == listenerKey(g.ListenAddr())
}

func listenerKey(addr netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
}

const specSyntax = "[bind_address:]port:host:hostport"

// ParseForward parses an ssh forward specification "[bind_address:]port:host:hostport"
// for direction. As in ssh, an omitted bind address means loopback, while
// an empty one or "*" means all interfaces. IPv6 addresses use brackets.
func ParseForward(direction, spec string) (Forward, error) {
	fields := splitSpec(spec)
	if len(fields) == 3 {
		fields = append([]string{"localhost"}, fields...)
	}
	if len(fields) != 4 {
		return Forward{}, fmt.Errorf("invalid forward %q: want %s", spec, specSyntax)
	}
	listen, err := parseListener(fields[0], fields[1])
	if err != nil {
		return Forward{}, fmt.Errorf("invalid forward %q: %w", spec, err)
	}
	host := strings.TrimSuffix(strings.TrimPrefix(fields[2], "["), "]")
	target := net.JoinHostPort(host, fields[3])
	if !isDialAddress(target) {
		return Forward{}, fmt.Errorf("invalid forward %q: invalid target %s", spec, target)
	}
	return Forward{Direction: direction, Listen: listen.String(), Target: target}, nil
}

// ParseListener parses the listener of a forward, "[bind_address:]port",
// with the same bind address rules as ParseForward. A full specification
// is accepted too; its target is then returned in the Forward.
func ParseListener(direction, spec string) (Forward, error) {
	fields := splitSpec(spec)
	switch len(fields) {
	case 1, 2:
		bind := "localhost"
		if len(fields) == 2 {
			bind = fields[0]
		}
		listen, err := parseListener(bind, fields[len(fields)-1])
		if err != nil {
			return Forward{}, fmt.Errorf("invalid listener %q: %w", spec, err)
		}
		return Forward{Direction: direction, Listen: listen.String()}, nil
	default:
		return ParseForward(direction, spec)
	}
}

// splitSpec splits spec at colons outside brackets.
func splitSpec(spec string) []string {
	var fields []string
	depth, start := 0, 0
	for i, r := range spec {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
		case ':':
			if depth == 0 {
				fields = append(fields, spec[start:i])
				start = i + 1
			}
		}
	}
	return append(fields, spec[start:])
}

func parseListener(bind, port string) (netip.AddrPort, error) {
	var addr netip.Addr
	switch bind {
	case "localhost":
		addr = netip.MustParseAddr("127.0.0.1")
	case "", "*":
		addr = netip.IPv4Unspecified()
	default:
		var ok bool
		if addr, ok = parseIP(strings.TrimSuffix(strings.TrimPrefix(bind, "["), "]")); !ok {
			return netip.AddrPort{}, fmt.Errorf("bind address %q is not an IP address, localhost, or *", bind)
		}
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return netip.AddrPort{}, fmt.Errorf("invalid port %q", port)
	}
	return netip.AddrPortFrom(addr, uint16(n)), nil
}

func validateForwards(forwards []Forward) error {
	for i, fwd := range forwards {
		n := i + 1
		if fwd.Direction != Remote && fwd.Direction != Local {
			return fmt.Errorf("invalid direction for forward %d", n)
		}
		if _, ok := parseListenAddress(fwd.Listen); !ok {
			return fmt.Errorf("invalid listen address for forward %d", n)
		}
		if !isDialAddress(fwd.Target) {
			return fmt.Errorf("invalid target for forward %d", n)
		}
		for _, earlier := range forwards[:i] {
			if earlier.SameListener(fwd) {
				return fmt.Errorf("duplicate listener for forward %d", n)
			}
		}
	}
	return nil
}
