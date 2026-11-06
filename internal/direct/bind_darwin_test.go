package direct

import (
	"net"
	"syscall"
	"testing"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

func routeTo(dst route.Addr, mask route.Addr, index, flags int) *route.RouteMessage {
	addrs := make([]route.Addr, syscall.RTAX_NETMASK+1)
	addrs[syscall.RTAX_DST], addrs[syscall.RTAX_NETMASK] = dst, mask
	return &route.RouteMessage{Index: index, Flags: syscall.RTF_UP | flags, Addrs: addrs}
}

func TestChooseInterfaces(t *testing.T) {
	const vpn, wifi, ethernet = 24, 6, 7
	any4, any6 := &route.Inet4Addr{}, &route.Inet6Addr{}
	tunnel := func(index int) bool { return index == vpn }
	gateway, scoped := syscall.RTF_GATEWAY, syscall.RTF_GATEWAY|syscall.RTF_IFSCOPE
	for _, c := range []struct {
		name   string
		routes []route.Message
		v4, v6 int
	}{
		{"none", nil, 0, 0},
		{"plain", []route.Message{routeTo(any4, nil, wifi, gateway), routeTo(any6, nil, wifi, gateway)}, wifi, wifi},
		{"a VPN took the default", []route.Message{
			routeTo(any4, nil, vpn, gateway),
			routeTo(any4, nil, wifi, scoped),
			routeTo(any4, nil, ethernet, scoped),
			routeTo(any6, nil, vpn, gateway),
			routeTo(any6, nil, ethernet, scoped),
		}, wifi, ethernet},
		{"the system's own choice first", []route.Message{
			routeTo(any4, nil, wifi, scoped),
			routeTo(any4, nil, ethernet, gateway),
		}, ethernet, 0},
		{"no gateway", []route.Message{routeTo(any4, nil, wifi, syscall.RTF_IFSCOPE)}, 0, 0},
		{"only a VPN", []route.Message{routeTo(any4, nil, vpn, gateway)}, 0, 0},
		{"not a default", []route.Message{
			routeTo(any4, &route.Inet4Addr{IP: [4]byte{128}}, wifi, gateway),
			routeTo(&route.Inet4Addr{IP: [4]byte{10}}, nil, wifi, gateway),
		}, 0, 0},
	} {
		if v4, v6 := chooseInterfaces(c.routes, tunnel); v4 != c.v4 || v6 != c.v6 {
			t.Errorf("%s: got %d and %d, want %d and %d", c.name, v4, v6, c.v4, c.v6)
		}
	}
}

// A bound socket follows the machine to another interface without closing,
// which keeps its port and with it the holes that NATs made for it.
func TestBindAnOpenSocket(t *testing.T) {
	for _, c := range []struct {
		network     string
		level, name int
	}{
		{"udp4", unix.IPPROTO_IP, unix.IP_BOUND_IF},
		{"udp6", unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF},
	} {
		udp, err := net.ListenUDP(c.network, &net.UDPAddr{})
		if err != nil {
			t.Skip(err)
		}
		defer udp.Close()
		raw, err := udp.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		lo, err := net.InterfaceByName("lo0")
		if err != nil {
			t.Fatal(err)
		}
		for _, index := range []int{lo.Index, 0} {
			if err := bindSocket(raw, c.network, index); err != nil {
				t.Fatal(err)
			}
			var got int
			raw.Control(func(fd uintptr) { got, err = unix.GetsockoptInt(int(fd), c.level, c.name) })
			if err != nil || got != index {
				t.Fatalf("%s: bound to %d, %v; want %d", c.network, got, err, index)
			}
		}
	}
}
