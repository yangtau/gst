package mobile

import "github.com/yangtau/ssh-tunnel/internal/tun"

// systemDevice returns the interface that the system made for this
// process.
func systemDevice() (Device, error) { return tun.Inherited() }
