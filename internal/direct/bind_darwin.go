package direct

import (
	"cmp"
	"net"
	"strings"
	"syscall"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// A proxy on macOS often takes the routes from the interface that really
// reaches the internet, which keeps a default route of its own, scoped to it.
// Such a proxy sends each flow again from a socket of its own, even a flow
// that its rules call direct, so the port changes, and what the other side's
// firewall let through for the old port drops the packets from the new one.
// So the sockets send through that interface, as if the proxy were not there.
// A VPN that routes packets would have kept the port, but nothing here can
// tell the two apart.

// defaultInterfaces returns the interfaces that reach the internet, for IPv4
// and for IPv6; 0 when there is none, which leaves it to the routes.
func defaultInterfaces() (v4, v6 int) {
	rib, err := route.FetchRIB(syscall.AF_UNSPEC, syscall.NET_RT_DUMP2, 0)
	if err != nil {
		return 0, 0
	}
	messages, err := route.ParseRIB(syscall.NET_RT_DUMP2, rib)
	if err != nil {
		return 0, 0
	}
	return chooseInterfaces(messages, tunnel)
}

// tunnel says whether an interface leads to another machine alone, as a VPN's
// does, rather than to a network.
func tunnel(index int) bool {
	i, err := net.InterfaceByIndex(index)
	return err != nil || i.Flags&(net.FlagPointToPoint|net.FlagLoopback) != 0
}

// chooseInterfaces finds the default routes through a gateway that are not a
// tunnel's. The one that is not scoped to its interface is the one the system
// would use by itself, so it comes before the others.
func chooseInterfaces(messages []route.Message, tunnel func(index int) bool) (v4, v6 int) {
	var global, scoped [2]int // for IPv4 and IPv6
	for _, message := range messages {
		m, ok := message.(*route.RouteMessage)
		if !ok || m.Flags&syscall.RTF_UP == 0 || m.Flags&syscall.RTF_GATEWAY == 0 {
			continue
		}
		if len(m.Addrs) <= syscall.RTAX_NETMASK || !isZero(m.Addrs[syscall.RTAX_NETMASK]) || tunnel(m.Index) {
			continue
		}
		found := &global
		if m.Flags&syscall.RTF_IFSCOPE != 0 {
			found = &scoped
		}
		switch dst := m.Addrs[syscall.RTAX_DST].(type) {
		case *route.Inet4Addr:
			if isZero(dst) && found[0] == 0 {
				found[0] = m.Index
			}
		case *route.Inet6Addr:
			if isZero(dst) && found[1] == 0 {
				found[1] = m.Index
			}
		}
	}
	return cmp.Or(global[0], scoped[0]), cmp.Or(global[1], scoped[1])
}

// isZero says whether an address is all zeros, or missing, as the destination
// and the netmask of a default route are.
func isZero(addr route.Addr) bool {
	switch a := addr.(type) {
	case nil:
		return true
	case *route.Inet4Addr:
		return a.IP == [4]byte{}
	case *route.Inet6Addr:
		return a.IP == [16]byte{}
	}
	return false
}

// bindSocket makes a socket send through an interface; 0 undoes that.
func bindSocket(c syscall.RawConn, network string, index int) error {
	var bindErr error
	err := c.Control(func(fd uintptr) {
		if strings.HasSuffix(network, "6") {
			bindErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, index)
		} else {
			bindErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, index)
		}
	})
	if err != nil {
		return err
	}
	return bindErr
}
