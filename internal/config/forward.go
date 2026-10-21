package config

import (
	"fmt"
	"net"
	"net/netip"
	"path"
	"strconv"
	"strings"
	"unicode"
)

// Forward relays connections accepted on Listen to Target, each of which
// is an IP and port or an absolute Unix socket path. A Remote
// forward listens on the server and dials Target from this machine, like
// "ssh -R"; a Local forward does the reverse, like "ssh -L". A Dynamic
// forward listens on this machine as a SOCKS5 proxy and has the server
// dial the address each client requests, like "ssh -D"; it has no Target.
type Forward struct {
	Direction string `json:"direction"` // Remote, Local, or Dynamic
	Listen    string `json:"listen"`    // IP:port or socket path bound on the listening side
	Target    string `json:"target"`    // host:port or socket path dialed from the other side
}

// Forward directions.
const (
	Remote  = "remote"
	Local   = "local"
	Dynamic = "dynamic"
)

// ListenAddr returns the parsed Listen address; it is valid once the
// forward has passed validation and does not listen on a socket.
func (f Forward) ListenAddr() netip.AddrPort {
	addr, _ := parseListenAddress(f.Listen)
	return addr
}

// Flag returns the ssh option for f's direction: "-R", "-L", or "-D".
func (f Forward) Flag() string {
	switch f.Direction {
	case Remote:
		return "-R"
	case Dynamic:
		return "-D"
	}
	return "-L"
}

// Spec formats f the way ssh takes it, such as "-R 0.0.0.0:2222:127.0.0.1:22"
// or "-D 127.0.0.1:1080".
func (f Forward) Spec() string {
	if f.Direction == Dynamic {
		return f.Flag() + " " + f.Listen
	}
	return f.Flag() + " " + f.Listen + ":" + f.Target
}

// SameListener reports whether f and g listen on the same address on the
// same side, treating ::ffff:a.b.c.d and a.b.c.d as equal. Local and
// Dynamic forwards both listen on this machine.
func (f Forward) SameListener(g Forward) bool {
	if (f.Direction == Remote) != (g.Direction == Remote) {
		return false
	}
	if IsSocket(f.Listen) || IsSocket(g.Listen) {
		return f.Listen == g.Listen
	}
	return listenerKey(f.ListenAddr()) == listenerKey(g.ListenAddr())
}

func listenerKey(addr netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
}

const (
	specSyntax        = "[bind_address:]port:host:hostport, where a field with \"/\" is a Unix socket path"
	dynamicSpecSyntax = "[bind_address:]port"
)

// ParseForward parses an ssh forward specification for direction:
// "[bind_address:]port:host:hostport", or "[bind_address:]port" for
// Dynamic. As in ssh, an omitted bind address means loopback, while an
// empty one or "*" means all interfaces. IPv6 addresses use brackets.
// Either "[bind_address:]port" or "host:hostport" can be a Unix socket
// path instead, which as in ssh is any field containing "/". Paths on this
// machine may be relative or start with "~" and are made absolute; paths
// on the server must be absolute.
func ParseForward(direction, spec string) (Forward, error) {
	fields := splitSpec(spec)
	if direction == Dynamic {
		if len(fields) > 2 || isPath(fields[0]) {
			return Forward{}, fmt.Errorf("invalid forward %q: want %s", spec, dynamicSpecSyntax)
		}
		return ParseListener(direction, spec)
	}
	targetFields := 2
	if isPath(fields[len(fields)-1]) {
		targetFields = 1
	}
	listenFields := len(fields) - targetFields
	if listenFields < 1 || listenFields > 2 {
		return Forward{}, fmt.Errorf("invalid forward %q: want %s", spec, specSyntax)
	}
	listen, err := parseListenFields(direction, fields[:listenFields])
	if err != nil {
		return Forward{}, fmt.Errorf("invalid forward %q: %w", spec, err)
	}
	target, err := parseTargetFields(direction, fields[listenFields:])
	if err != nil {
		return Forward{}, fmt.Errorf("invalid forward %q: %w", spec, err)
	}
	return Forward{Direction: direction, Listen: listen, Target: target}, nil
}

// ParseListener parses the listener of a forward, "[bind_address:]port"
// or a socket path, with the same rules as ParseForward. A full
// specification is accepted too; its target is then returned in the Forward.
func ParseListener(direction, spec string) (Forward, error) {
	fields := splitSpec(spec)
	if len(fields) > 2 || len(fields) == 2 && (isPath(fields[0]) || isPath(fields[1])) {
		return ParseForward(direction, spec)
	}
	listen, err := parseListenFields(direction, fields)
	if err != nil {
		return Forward{}, fmt.Errorf("invalid listener %q: %w", spec, err)
	}
	return Forward{Direction: direction, Listen: listen}, nil
}

// IsSocket reports whether a forward's listen or target address is a Unix
// socket path rather than an IP or host and port.
func IsSocket(address string) bool {
	return strings.HasPrefix(address, "/")
}

func isPath(field string) bool {
	return strings.Contains(field, "/")
}

// parseListenFields parses "[bind_address:]port" given as one or two fields,
// or a socket path as one field.
func parseListenFields(direction string, fields []string) (string, error) {
	if len(fields) == 1 && isPath(fields[0]) {
		return socketPath(fields[0], direction == Remote)
	}
	bind := "localhost"
	if len(fields) == 2 {
		bind = fields[0]
	}
	listen, err := parseListener(bind, fields[len(fields)-1])
	if err != nil {
		return "", err
	}
	return listen.String(), nil
}

// parseTargetFields parses "host:hostport" given as two fields, or a socket
// path as one field.
func parseTargetFields(direction string, fields []string) (string, error) {
	if len(fields) == 1 {
		return socketPath(fields[0], direction != Remote)
	}
	host := strings.TrimSuffix(strings.TrimPrefix(fields[0], "["), "]")
	target := net.JoinHostPort(host, fields[1])
	if !isDialAddress(target) {
		return "", fmt.Errorf("invalid target %s", target)
	}
	return target, nil
}

// socketPath returns the absolute form of a socket path on the server or
// on this machine.
func socketPath(field string, onServer bool) (string, error) {
	var p string
	if onServer {
		if !path.IsAbs(field) {
			return "", fmt.Errorf("socket path %q on the server must be absolute", field)
		}
		p = path.Clean(field)
	} else {
		var err error
		if p, err = AbsPath(field); err != nil {
			return "", err
		}
	}
	if !isSocketPath(p) {
		return "", fmt.Errorf("invalid socket path %q", field)
	}
	return p, nil
}

// isSocketPath reports whether p is a clean absolute path without control
// characters, which remote cleanup passes to a shell.
func isSocketPath(p string) bool {
	return IsSocket(p) && path.Clean(p) == p && !strings.ContainsFunc(p, unicode.IsControl)
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
		if fwd.Direction != Remote && fwd.Direction != Local && fwd.Direction != Dynamic {
			return fmt.Errorf("invalid direction for forward %d", n)
		}
		if _, ok := parseListenAddress(fwd.Listen); !ok && (!isSocketPath(fwd.Listen) || fwd.Direction == Dynamic) {
			return fmt.Errorf("invalid listen address for forward %d", n)
		}
		if fwd.Direction == Dynamic {
			if fwd.Target != "" {
				return fmt.Errorf("dynamic forward %d must not have a target", n)
			}
		} else if !isDialAddress(fwd.Target) && !isSocketPath(fwd.Target) {
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
