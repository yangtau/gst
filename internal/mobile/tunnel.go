package mobile

import (
	"context"
	"errors"
	"log"
	"net"
	"net/netip"
	"os"
	"runtime"
	"runtime/debug"
	"sync"

	wgtun "golang.zx2c4.com/wireguard/tun"

	"github.com/yangtau/ssh-tunnel/internal/daemon"
	"github.com/yangtau/ssh-tunnel/internal/direct"
	"github.com/yangtau/ssh-tunnel/internal/logfile"
	"github.com/yangtau/ssh-tunnel/internal/tun"
	"github.com/yangtau/ssh-tunnel/internal/version"
	"github.com/yangtau/ssh-tunnel/internal/vnet"
)

const (
	// dnsAddress is the address in the interface that answers for .gst.
	// The system's resolver asks only an address on port 53, and the
	// interface must route it; 198.18.0.0/15 is set aside for tests of
	// network equipment (RFC 2544), so no network that a phone meets has
	// the address, and no subnet of gst, which is always private, holds it.
	dnsAddress = "198.18.0.53"
	// dnsDomain is the domain that the system asks that address for.
	dnsDomain = "gst"
	// memoryLimit keeps the daemon well under what iOS gives the process
	// of a VPN, which it ends at 50 MiB.
	memoryLimit = 32 << 20
)

// Settings are what the system's interface should be.
type Settings struct {
	Address string  `json:"address"` // of this machine
	Routes  []Route `json:"routes"`  // what goes to the interface: the subnet, and DNS
	DNS     string  `json:"dns"`     // the resolver of Domain
	Domain  string  `json:"domain"`  // the names that the system asks DNS for
	MTU     int     `json:"mtu"`
}

// Route is a destination of the interface, as a system takes it: its
// first address and its mask.
type Route struct {
	Address string `json:"address"`
	Mask    string `json:"mask"`
}

func route(p netip.Prefix) Route {
	return Route{Address: p.Masked().Addr().String(), Mask: net.IP(net.CIDRMask(p.Bits(), 32)).String()}
}

// Device is the system's interface, once it is set up.
type Device interface {
	wgtun.Device
}

// System is what the system does for gst's interface, which it makes and
// sets up there; gst only says how.
type System struct {
	// Apply sets the system's interface up as the settings say, and
	// returns once that is done. A system that hands the interface over
	// as a descriptor, as Android does, returns it, and gst owns it from
	// then on; one that keeps it, as iOS does, returns -1, and gst finds
	// the interface. gst calls it when the network gives this machine an
	// address, and again whenever the settings change.
	Apply func(Settings) (fd int, err error)
	// Forward asks the system's own resolver a DNS query for a name
	// outside .gst, and returns its answer, for a system that sends every
	// query to the DNS of the interface, as Android does. It is nil where
	// the system sends only the names under Domain there, as iOS does.
	Forward func(query []byte) ([]byte, error)
}

// Start runs the daemon in this process until Stop, with sys setting
// the interface up.
func (a *App) Start(sys System) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running != nil {
		return errors.New("gst runs already")
	}
	var redirect []int
	if runtime.GOOS == "ios" || runtime.GOOS == "android" {
		// What the Go runtime writes as it dies is the only trace that a
		// process without a console leaves.
		redirect = []int{2}
	}
	if runtime.GOOS == "ios" {
		debug.SetMemoryLimit(memoryLimit)
	}
	logFile, err := logfile.Open(a.logPath(), redirect)
	if err != nil {
		return errors.New("the log file cannot be opened")
	}
	d := daemon.New(daemon.Config{
		ConfigPath: a.configPath(),
		StateDir:   a.dir,
		Software:   version.Software,
		Connect:    a.connect,
		Addresses:  direct.Addresses,
		OpenNet: func(spec tun.Spec) (vnet.Interface, error) {
			i := &systemInterface{apply: sys.Apply, device: a.device}
			if _, err := i.Set(spec); err != nil {
				i.Close()
				return nil, err
			}
			return i, nil
		},
		NetDNS:     dnsAddress + ":53",
		NetEcho:    tun.EchoPort,
		NetInside:  true,
		NetForward: sys.Forward,
		Log:        log.New(logFile, "", log.LstdFlags),
	})
	ctx, stop := context.WithCancel(context.Background())
	r := &running{daemon: d, stop: stop, done: make(chan struct{}), log: logFile}
	go func() {
		defer close(r.done)
		d.Run(ctx)
	}()
	a.running = r
	return nil
}

// Stop stops the daemon and waits until it has.
func (a *App) Stop() {
	a.mu.Lock()
	r := a.running
	a.running = nil
	a.mu.Unlock()
	if r == nil {
		return
	}
	r.stop()
	<-r.done
	r.log.Close()
}

// systemInterface is the interface that the system makes for this
// process and sets up as apply tells it.
type systemInterface struct {
	apply   func(Settings) (int, error)
	device  func(fd int) (Device, error) // the device of what apply returned
	current switching
	applied tun.Spec // what the system is set to; its names are left out
}

func (i *systemInterface) Device() wgtun.Device { return &i.current }

// Set brings the interface to spec. The names of spec are left to the
// DNS, which the system asks for them.
func (i *systemInterface) Set(spec tun.Spec) (string, error) {
	want := tun.Spec{Subnet: spec.Subnet, Address: spec.Address}
	if want.Subnet == i.applied.Subnet && want.Address == i.applied.Address {
		return i.current.Name()
	}
	subnet, err := netip.ParsePrefix(spec.Subnet)
	if err != nil {
		return "", err
	}
	fd, err := i.apply(Settings{
		Address: spec.Address,
		Routes:  []Route{route(subnet), route(netip.MustParsePrefix(dnsAddress + "/32"))},
		DNS:     dnsAddress,
		Domain:  dnsDomain,
		MTU:     tun.MTU,
	})
	if err != nil {
		return "", err
	}
	// A system that hands over a descriptor makes a new interface each
	// time, and ends the one before.
	if fd >= 0 || i.current.device() == nil {
		device, err := i.device(fd)
		if err != nil {
			return "", err
		}
		i.current.set(device)
	}
	i.applied = want
	return i.current.Name()
}

func (i *systemInterface) Close() error { return i.current.Close() }

// switching is the interface as WireGuard sees it: the system's latest,
// which takes over from the one before, on a system that makes a new one
// whenever the settings change.
type switching struct {
	mu      sync.Mutex
	current Device
	changed chan struct{} // closed when current is replaced or closed
	closed  bool
	events  chan wgtun.Event
}

func (s *switching) device() Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// get returns the current device, and a channel that is closed when
// that changes.
func (s *switching) get() (Device, <-chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.current == nil {
		return nil, nil, os.ErrClosed
	}
	return s.current, s.changed, nil
}

// set puts device in place of the one before, which it closes.
func (s *switching) set(device Device) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		device.Close()
		return
	}
	old, changed := s.current, s.changed
	s.current, s.changed = device, make(chan struct{})
	if s.events == nil {
		s.events = make(chan wgtun.Event)
	}
	s.mu.Unlock()
	if old != nil {
		close(changed)
		old.Close()
	}
}

func (s *switching) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	for {
		d, changed, err := s.get()
		if err != nil {
			return 0, err
		}
		n, err := d.Read(bufs, sizes, offset)
		if err == nil || n > 0 {
			return n, err
		}
		select {
		case <-changed:
			// It ended because another took over.
		default:
			return n, err
		}
	}
}

func (s *switching) Write(bufs [][]byte, offset int) (int, error) {
	d, _, err := s.get()
	if err != nil {
		return 0, err
	}
	return d.Write(bufs, offset)
}

func (s *switching) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	current, changed, events := s.current, s.changed, s.events
	s.mu.Unlock()
	if events != nil {
		close(events)
	}
	if current == nil {
		return nil
	}
	close(changed)
	return current.Close()
}

func (s *switching) Name() (string, error) {
	d, _, err := s.get()
	if err != nil {
		return "", err
	}
	return d.Name()
}

func (s *switching) File() *os.File    { return nil }
func (s *switching) MTU() (int, error) { return tun.MTU, nil }
func (s *switching) BatchSize() int    { return 1 }

func (s *switching) Events() <-chan wgtun.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events
}
