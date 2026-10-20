package config

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

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

// parseListenAddress accepts only an IP literal and port, so the address
// means the same whichever side binds it.
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

// expandHome resolves a leading "~" or "~/" to the current user's home
// directory. Other users' homes ("~name") are not supported.
func expandHome(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return "", errors.New("invalid credential path")
	}
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return "", errors.New("unsupported home path")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("home directory lookup failed")
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
}
