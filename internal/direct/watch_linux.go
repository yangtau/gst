//go:build linux && !android

package direct

import (
	"encoding/binary"
	"os"

	"golang.org/x/sys/unix"
)

// watchSystem calls changed for each netlink message that says that a
// link, an address, or a default route changed, until done is closed. It
// does nothing when it cannot open the socket.
func watchSystem(changed func(), done <-chan struct{}) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.NETLINK_ROUTE)
	if err != nil {
		return
	}
	groups := uint32(unix.RTMGRP_LINK | unix.RTMGRP_IPV4_IFADDR | unix.RTMGRP_IPV6_IFADDR | unix.RTMGRP_IPV4_ROUTE | unix.RTMGRP_IPV6_ROUTE)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: groups}); err != nil {
		unix.Close(fd)
		return
	}
	f := os.NewFile(uintptr(fd), "netlink")
	defer f.Close()
	go func() {
		<-done
		f.Close()
	}()
	buf := make([]byte, 1<<16)
	for {
		n, err := f.Read(buf)
		if err != nil {
			return
		}
		if networkMessages(buf[:n]) {
			changed()
		}
	}
}

// networkMessages reports whether one of the netlink messages in b says
// that the network changed: a link, an address, or a default route.
// Other routes come and go with the machine's containers and VPNs.
func networkMessages(b []byte) bool {
	for len(b) >= unix.NLMSG_HDRLEN {
		size := int(binary.NativeEndian.Uint32(b))
		if size < unix.NLMSG_HDRLEN || size > len(b) {
			return false
		}
		switch binary.NativeEndian.Uint16(b[4:]) {
		case unix.RTM_NEWLINK, unix.RTM_DELLINK, unix.RTM_NEWADDR, unix.RTM_DELADDR:
			return true
		case unix.RTM_NEWROUTE, unix.RTM_DELROUTE:
			if size > unix.NLMSG_HDRLEN+1 && b[unix.NLMSG_HDRLEN+1] == 0 { // rtm_dst_len: a default route
				return true
			}
		}
		b = b[min(len(b), (size+unix.NLMSG_ALIGNTO-1)&^(unix.NLMSG_ALIGNTO-1)):]
	}
	return false
}
