//go:build linux && !android

package direct

import (
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestChooseInterface(t *testing.T) {
	const proxy, wifi, ethernet = 9, 3, 2
	tunnel := func(index int) bool { return index == proxy }
	for _, c := range []struct {
		name   string
		routes []defaultRoute
		want   int
	}{
		{"none", nil, 0},
		{"plain", []defaultRoute{{wifi, 600}}, wifi},
		{"the lowest metric", []defaultRoute{{wifi, 600}, {ethernet, 100}}, ethernet},
		{"a proxy's route in the main table", []defaultRoute{{proxy, 0}, {wifi, 600}}, wifi},
		{"only a proxy", []defaultRoute{{proxy, 0}}, 0},
	} {
		if got := chooseInterface(c.routes, tunnel); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// A socket follows the machine to another interface without closing, which
// keeps its port and with it the holes that NATs made for it.
func TestBindAnOpenSocket(t *testing.T) {
	for _, c := range []struct {
		network     string
		level, name int
	}{
		{"udp4", unix.IPPROTO_IP, unix.IP_UNICAST_IF},
		{"udp6", unix.IPPROTO_IPV6, unix.IPV6_UNICAST_IF},
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
		lo, err := net.InterfaceByName("lo")
		if err != nil {
			t.Fatal(err)
		}
		for _, index := range []int{lo.Index, 0, lo.Index} {
			if err := bindSocket(raw, c.network, index); err != nil {
				t.Fatal(err)
			}
			var got int
			raw.Control(func(fd uintptr) { got, err = unix.GetsockoptInt(int(fd), c.level, c.name) })
			if err != nil || got != networkOrder(index) {
				t.Fatalf("%s: bound to %d, %v; want %d", c.network, networkOrder(got), err, index)
			}
		}
	}
}

// A socket that sends through one interface still receives from all: here
// from the loopback interface while it sends through the default one.
func TestBoundSocketReceivesFromEveryInterface(t *testing.T) {
	index, _ := physicalInterfaces()
	if index == 0 {
		t.Skip("no default route")
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	raw, err := udp.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if err := bindSocket(raw, "udp4", index); err != nil {
		t.Fatal(err)
	}
	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	to := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: udp.LocalAddr().(*net.UDPAddr).Port}
	if _, err := sender.WriteTo([]byte("hello"), to); err != nil {
		t.Fatal(err)
	}
	udp.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	if n, _, err := udp.ReadFrom(buf); err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("read %q: %v", buf[:n], err)
	}
}
