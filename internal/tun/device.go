package tun

import (
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
	wgtun "golang.zx2c4.com/wireguard/tun"
)

// Device is an open interface as wireguard-go uses it. wireguard-go's own
// devices also change the interface and watch it, which takes root; the
// helper does that here.
type Device struct {
	file   *os.File
	name   string
	events chan wgtun.Event
	once   sync.Once
}

func newDevice(file *os.File, name string) *Device {
	return &Device{file: file, name: name, events: make(chan wgtun.Event)}
}

func (d *Device) File() *os.File             { return d.file }
func (d *Device) MTU() (int, error)          { return MTU, nil }
func (d *Device) Name() (string, error)      { return d.name, nil }
func (d *Device) Events() <-chan wgtun.Event { return d.events }
func (d *Device) BatchSize() int             { return 1 }

// Close closes the device. wireguard-go calls it when its device closes.
func (d *Device) Close() error {
	var err error
	d.once.Do(func() {
		err = d.file.Close()
		close(d.events)
	})
	return err
}

// Read reads one packet into bufs[0] at offset.
func (d *Device) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	if offset < packetHeader {
		return 0, io.ErrShortBuffer
	}
	n, err := d.file.Read(bufs[0][offset-packetHeader:])
	if err != nil {
		return 0, err
	}
	if n <= packetHeader {
		return 0, nil
	}
	sizes[0] = n - packetHeader
	return 1, nil
}

// Write writes the packets of bufs, each at offset.
func (d *Device) Write(bufs [][]byte, offset int) (int, error) {
	if offset < packetHeader {
		return 0, io.ErrShortBuffer
	}
	for i, buf := range bufs {
		setPacketHeader(buf[offset-packetHeader:offset], buf[offset])
		if _, err := d.file.Write(buf[offset-packetHeader:]); err != nil {
			return i, err
		}
	}
	return len(bufs), nil
}

// Adopt returns the device of an interface that the system made and
// handed over as the descriptor fd, as Android does for the process that
// runs a VPN. The device owns fd from then on.
func Adopt(fd int) (*Device, error) {
	name, err := deviceName(fd)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("name the interface: %w", err)
	}
	unix.CloseOnExec(fd)
	// In non-blocking mode the runtime polls it, so that closing it ends
	// the read in progress.
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return newDevice(os.NewFile(uintptr(fd), "tun"), name), nil
}
