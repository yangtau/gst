package tun

import (
	"io"
	"os"
	"sync"

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
