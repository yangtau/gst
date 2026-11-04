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

// IsSocket reports whether an address is a Unix socket path rather than
// a host and port.
func IsSocket(address string) bool {
	return strings.HasPrefix(address, "/")
}

// Network returns the network of a target or listen address: "unix" for
// a socket path, else "tcp".
func Network(address string) string {
	if IsSocket(address) {
		return "unix"
	}
	return "tcp"
}

// ValidTarget reports whether address is a target that gst can dial on
// network: host and port for "tcp" or a socket path for "unix".
func ValidTarget(network, address string) bool {
	switch network {
	case "tcp":
		return isDialAddress(address)
	case "unix":
		return isSocketPath(address)
	}
	return false
}

// ParseListen parses where a map listens: a port, which means 127.0.0.1,
// "bind_address:port", where bind_address is an IP, "localhost", or "*"
// for all interfaces, or a Unix socket path, which is anything with a "/"
// and may be relative or start with "~". IPv6 addresses use brackets.
func ParseListen(spec string) (string, error) {
	if strings.Contains(spec, "/") {
		p, err := AbsPath(spec)
		if err != nil {
			return "", err
		}
		if !isSocketPath(p) {
			return "", fmt.Errorf("invalid socket path %q", spec)
		}
		return p, nil
	}
	bind, port := "localhost", spec
	if i := strings.LastIndex(spec, ":"); i >= 0 {
		bind, port = spec[:i], spec[i+1:]
	}
	var addr netip.Addr
	switch bind {
	case "localhost":
		addr = netip.MustParseAddr("127.0.0.1")
	case "", "*":
		addr = netip.IPv4Unspecified()
	default:
		var ok bool
		if addr, ok = parseIP(strings.TrimSuffix(strings.TrimPrefix(bind, "["), "]")); !ok {
			return "", fmt.Errorf("invalid listen address %q: the bind address is not an IP address, localhost, or *", spec)
		}
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return "", fmt.Errorf("invalid listen address %q: want a port, bind_address:port, or a socket path", spec)
	}
	return netip.AddrPortFrom(addr, uint16(n)).String(), nil
}

// ParseTarget parses an address on a machine: a port, which means
// localhost there, host:port, or an absolute Unix socket path.
func ParseTarget(spec string) (string, error) {
	address := spec
	if !IsSocket(spec) && !strings.Contains(spec, ":") {
		address = "localhost:" + spec
	}
	if IsSocket(address) {
		address = path.Clean(address)
	}
	if !ValidTarget(Network(address), address) {
		return "", fmt.Errorf("invalid target %q: want a port, host:port, or an absolute socket path", spec)
	}
	return address, nil
}

// splitHostPort splits "host:port", requiring a non-empty host and a
// numeric port in 1..65535. Service names such as "ssh" are rejected.
func splitHostPort(address string) (string, uint16, bool) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return "", 0, false
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return "", 0, false
	}
	return host, uint16(port), true
}

// parseListenAddress accepts only an IP literal and port.
func parseListenAddress(address string) (netip.AddrPort, bool) {
	host, port, ok := splitHostPort(address)
	if !ok {
		return netip.AddrPort{}, false
	}
	addr, ok := parseIP(host)
	if !ok {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(addr, port), true
}

// isSocketPath reports whether p is a clean absolute path without control
// characters.
func isSocketPath(p string) bool {
	return IsSocket(p) && path.Clean(p) == p && !strings.ContainsFunc(p, unicode.IsControl)
}

// isDialAddress reports whether address is an IP literal or DNS hostname
// followed by a port.
func isDialAddress(address string) bool {
	host, _, ok := splitHostPort(address)
	if !ok {
		return false
	}
	if _, ok := parseIP(host); ok {
		return true
	}
	return isHostname(host)
}

func parseIP(host string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(host)
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, false
	}
	return addr, true
}

func isHostname(host string) bool {
	if len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(strings.TrimSuffix(host, "."), ".") {
		if !isHostnameLabel(label) {
			return false
		}
	}
	return true
}

func isHostnameLabel(label string) bool {
	if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, r := range label {
		isAlphanumeric := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !isAlphanumeric && r != '-' {
			return false
		}
	}
	return true
}
