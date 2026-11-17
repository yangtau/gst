//go:build linux && !android

package direct

import (
	"encoding/binary"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// A proxy on Linux takes UDP with routing rules and a table of its own, or
// with routes to its interface in the main table, and sends each flow again
// from a socket of its own, which changes its port as on macOS (see
// bind_darwin.go). So, while the routes lead into a tunnel (see bind.go),
// the sockets send through the interface of the main table's default route
// that is not a tunnel's. IP_UNICAST_IF does that:
// unlike SO_BINDTODEVICE it takes no privilege, changes on an open socket,
// and leaves alone what arrives; the routes and rules then count only for
// that interface. Android keeps the routes from apps, and its VPN is gst's
// own, so bind_other.go serves there.

// physicalInterfaces returns the interfaces that reach the internet below
// any tunnel, for IPv4 and for IPv6; 0 when there is none, which leaves it
// to the routes.
func physicalInterfaces(m interfaces) (v4, v6 int) {
	return defaultInterface(syscall.AF_INET, m.tunnelAt), defaultInterface(syscall.AF_INET6, m.tunnelAt)
}

// defaultRoute is a default route of the main table: its interface, and its
// metric, of which the lowest wins.
type defaultRoute struct{ index, metric int }

func defaultInterface(family int, tunnelAt func(index int) bool) int {
	rib, err := syscall.NetlinkRIB(syscall.RTM_GETROUTE, family)
	if err != nil {
		return 0
	}
	messages, err := syscall.ParseNetlinkMessage(rib)
	if err != nil {
		return 0
	}
	var routes []defaultRoute
	for _, m := range messages {
		if m.Header.Type != syscall.RTM_NEWROUTE || len(m.Data) < syscall.SizeofRtMsg {
			continue
		}
		rt := (*syscall.RtMsg)(unsafe.Pointer(&m.Data[0]))
		// Not a default route only for some sources (IPv6).
		if rt.Dst_len != 0 || rt.Src_len != 0 || rt.Type != syscall.RTN_UNICAST {
			continue
		}
		attrs, err := syscall.ParseNetlinkRouteAttr(&m)
		if err != nil {
			continue
		}
		table, r := int(rt.Table), defaultRoute{}
		for _, a := range attrs {
			if len(a.Value) < 4 {
				continue
			}
			value := int(binary.NativeEndian.Uint32(a.Value))
			switch a.Attr.Type {
			case unix.RTA_TABLE:
				table = value
			case syscall.RTA_OIF:
				r.index = value
			case syscall.RTA_PRIORITY:
				r.metric = value
			case unix.RTA_MULTIPATH:
				// The first of several next hops (struct rtnexthop:
				// length, flags, hops, then the interface).
				if len(a.Value) >= 8 && r.index == 0 {
					r.index = int(binary.NativeEndian.Uint32(a.Value[4:8]))
				}
			}
		}
		if table == syscall.RT_TABLE_MAIN && r.index != 0 {
			routes = append(routes, r)
		}
	}
	return chooseInterface(routes, tunnelAt)
}

// chooseInterface picks the default route with the lowest metric that is not
// a tunnel's, as a proxy may put one of its own in the main table.
func chooseInterface(routes []defaultRoute, tunnel func(index int) bool) int {
	var best *defaultRoute
	for i, r := range routes {
		if !tunnel(r.index) && (best == nil || r.metric < best.metric) {
			best = &routes[i]
		}
	}
	if best == nil {
		return 0
	}
	return best.index
}

// bindSocket makes a socket send through an interface; 0 undoes that.
func bindSocket(c syscall.RawConn, network string, index int) error {
	level, name := unix.IPPROTO_IP, unix.IP_UNICAST_IF
	if network[len(network)-1] == '6' {
		level, name = unix.IPPROTO_IPV6, unix.IPV6_UNICAST_IF
	}
	var bindErr error
	err := c.Control(func(fd uintptr) {
		bindErr = unix.SetsockoptInt(int(fd), level, name, networkOrder(index))
	})
	if err != nil {
		return err
	}
	return bindErr
}

// networkOrder returns index as both options take it, in network byte order.
func networkOrder(index int) int {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(index))
	return int(int32(binary.NativeEndian.Uint32(b[:])))
}
