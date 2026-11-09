package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

// Expose maps a TCP or UDP port on this node's gst address to a target.
// Unix stream targets use TCP on the gst side.
type Expose struct {
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	Target   string `json:"target"`
}

// Key names the rule by what it listens on, such as tcp/8080.
func (e Expose) Key() string { return ExposeKey(e.Protocol, e.Port) }

func ExposeKey(protocol string, port int) string { return protocol + "/" + strconv.Itoa(port) }

// Destination returns the network and address understood by net.Dialer.
func (e Expose) Destination() (string, string) {
	u, err := url.Parse(e.Target)
	if err != nil {
		return "", ""
	}
	if u.Scheme == "unix" {
		return "unix", u.Path
	}
	return u.Scheme, u.Host
}

func (e Expose) Validate() error {
	parsed, err := ParseExpose(e.Target, e.Port, e.Protocol == "udp")
	if err != nil || parsed != e {
		return fmt.Errorf("invalid expose rule") // Config errors must not quote targets.
	}
	return nil
}

// ParseExpose accepts a local port or an explicit tcp://, udp:// or unix:// target.
func ParseExpose(target string, port int, udp bool) (Expose, error) {
	protocol := "tcp"
	if udp {
		protocol = "udp"
	}
	if !strings.Contains(target, "://") {
		n, err := ParsePort(target)
		if err != nil {
			return Expose{}, err
		}
		target = protocol + "://127.0.0.1:" + strconv.Itoa(n)
	}
	u, err := url.Parse(target)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return Expose{}, fmt.Errorf("target must be a port, tcp://host:port, udp://host:port, or unix:///absolute/path")
	}
	switch u.Scheme {
	case "tcp", "udp":
		if udp && u.Scheme != "udp" {
			return Expose{}, fmt.Errorf("--udp requires a UDP target")
		}
		protocol = u.Scheme
		host, p, err := net.SplitHostPort(u.Host)
		if err != nil || host == "" || u.Path != "" {
			return Expose{}, fmt.Errorf("TCP and UDP targets require a host and port")
		}
		n, err := ParsePort(p)
		if err != nil {
			return Expose{}, err
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
			return Expose{}, fmt.Errorf("target must not be an unspecified address")
		}
		target = (&url.URL{Scheme: protocol, Host: net.JoinHostPort(host, strconv.Itoa(n))}).String()
		if port == 0 {
			port = n
		}
	case "unix":
		if udp {
			return Expose{}, fmt.Errorf("Unix stream sockets cannot use --udp")
		}
		if u.Host != "" || !filepath.IsAbs(u.Path) || strings.ContainsRune(u.Path, 0) {
			return Expose{}, fmt.Errorf("Unix socket target requires an absolute path")
		}
		target = (&url.URL{Scheme: "unix", Path: filepath.Clean(u.Path)}).String()
		if port == 0 {
			return Expose{}, fmt.Errorf("Unix socket target requires --port")
		}
	default:
		return Expose{}, fmt.Errorf("target scheme must be tcp, udp, or unix")
	}
	if port < 1 || port > 65535 {
		return Expose{}, fmt.Errorf("port must be between 1 and 65535")
	}
	return Expose{Protocol: protocol, Port: port, Target: target}, nil
}

func ParsePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 || strings.Trim(s, "0123456789") != "" {
		return 0, fmt.Errorf("port must be between 1 and 65535")
	}
	return n, nil
}
