//go:build !darwin

package mobile

import (
	"errors"

	"github.com/yangtau/ssh-tunnel/internal/tun"
)

// systemDevice returns the interface of descriptor fd, which the system
// made for this process, as Android does.
func systemDevice(fd int) (Device, error) {
	if fd < 0 {
		return nil, errors.New("the system gave this process no interface")
	}
	return tun.Adopt(fd)
}
