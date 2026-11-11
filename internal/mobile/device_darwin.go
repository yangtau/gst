package mobile

import "github.com/yangtau/ssh-tunnel/internal/tun"

// systemDevice returns the interface that the system made for this
// process: the one of descriptor fd, or, when fd is -1, the one that it
// keeps, as iOS does.
func systemDevice(fd int) (Device, error) {
	if fd >= 0 {
		return tun.Adopt(fd)
	}
	return tun.Inherited()
}
