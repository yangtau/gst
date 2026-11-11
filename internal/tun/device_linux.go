package tun

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux hands over packets as they are (IFF_NO_PI).
const packetHeader = 0

func setPacketHeader([]byte, byte) {}

// openDevice creates the interface.
func openDevice() (*os.File, string, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /dev/net/tun: %w", err)
	}
	var req [unix.IFNAMSIZ + 64]byte
	copy(req[:], interfaceName)
	*(*uint16)(unsafe.Pointer(&req[unix.IFNAMSIZ])) = unix.IFF_TUN | unix.IFF_NO_PI
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TUNSETIFF, uintptr(unsafe.Pointer(&req[0]))); errno != 0 {
		unix.Close(fd)
		return nil, "", fmt.Errorf("create the interface: %w", errno)
	}
	return os.NewFile(uintptr(fd), "tun"), interfaceName, nil
}

// deviceName returns the name of the interface that fd is open on.
func deviceName(fd int) (string, error) {
	var req [unix.IFNAMSIZ + 64]byte
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TUNGETIFF, uintptr(unsafe.Pointer(&req[0]))); errno != 0 {
		return "", errno
	}
	return unix.ByteSliceToString(req[:unix.IFNAMSIZ]), nil
}

func newSystem() system { return newLinuxSystem() }
