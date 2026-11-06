package tun

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const utunControlName = "com.apple.net.utun_control"

// utun puts the address family in front of each packet.
const packetHeader = 4

func setPacketHeader(header []byte, first byte) {
	header[0], header[1], header[2] = 0, 0, 0
	header[3] = unix.AF_INET
	if first>>4 == 6 {
		header[3] = unix.AF_INET6
	}
}

// openDevice creates the next free utun interface.
func openDevice() (*os.File, string, error) {
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, 2) // SYSPROTO_CONTROL
	if err != nil {
		return nil, "", fmt.Errorf("open a control socket: %w", err)
	}
	unix.CloseOnExec(fd)
	info := &unix.CtlInfo{}
	copy(info.Name[:], utunControlName)
	if err := unix.IoctlCtlInfo(fd, info); err != nil {
		unix.Close(fd)
		return nil, "", fmt.Errorf("find the utun control: %w", err)
	}
	if err := unix.Connect(fd, &unix.SockaddrCtl{ID: info.Id, Unit: 0}); err != nil {
		unix.Close(fd)
		return nil, "", fmt.Errorf("create the interface: %w", err)
	}
	name, err := unix.GetsockoptString(fd, 2, 2) // SYSPROTO_CONTROL, UTUN_OPT_IFNAME
	if err != nil {
		unix.Close(fd)
		return nil, "", fmt.Errorf("name the interface: %w", err)
	}
	return os.NewFile(uintptr(fd), "tun"), name, nil
}

func newSystem() system { return newDarwinSystem() }
