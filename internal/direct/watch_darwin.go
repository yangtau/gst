package direct

import (
	"os"

	"golang.org/x/sys/unix"
)

// watchSystem calls changed for each message of the routing socket that
// says that an interface, an address, or a route through a gateway
// changed, until done is closed. Messages for hosts, which the system
// adds and removes all the time for its neighbors, say nothing of the
// network. It does nothing when it cannot open the socket.
func watchSystem(changed func(), done <-chan struct{}) {
	fd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		return
	}
	unix.CloseOnExec(fd)
	if err := unix.SetNonblock(fd, true); err != nil { // so that closing it ends a read
		unix.Close(fd)
		return
	}
	f := os.NewFile(uintptr(fd), "route")
	defer f.Close()
	go func() {
		<-done
		f.Close()
	}()
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		if err != nil {
			return
		}
		if networkMessage(buf[:n]) {
			changed()
		}
	}
}

// networkMessage reports whether a message of the routing socket says that
// the network changed: an interface, its addresses, or a route through a
// gateway that is not for one host.
func networkMessage(m []byte) bool {
	if len(m) < 12 { // up to rtm_flags, which every message that counts has
		return false
	}
	switch m[3] { // rtm_type
	case unix.RTM_NEWADDR, unix.RTM_DELADDR, unix.RTM_IFINFO:
		return true
	case unix.RTM_ADD, unix.RTM_DELETE, unix.RTM_CHANGE:
		flags := int32(m[8]) | int32(m[9])<<8 | int32(m[10])<<16 | int32(m[11])<<24 // little-endian, as on every Apple machine
		return flags&unix.RTF_GATEWAY != 0 && flags&unix.RTF_HOST == 0
	}
	return false
}
