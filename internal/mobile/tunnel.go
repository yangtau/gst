package mobile

import (
	"context"
	"errors"
	"log"
	"net"
	"net/netip"
	"runtime"
	"runtime/debug"

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

// Settings are what the system's interface should be. The system makes
// and sets up the interface there; gst only says how.
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

// Start runs the daemon in this process until Stop. apply sets the
// system's interface up as the settings say, and returns once that is
// done; gst calls it when the network gives this machine an address, and
// again whenever the settings change.
func (a *App) Start(apply func(Settings) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running != nil {
		return errors.New("gst runs already")
	}
	var redirect []int
	if runtime.GOOS == "ios" {
		// What the Go runtime writes as it dies is the only trace that a
		// process without a console leaves.
		redirect = []int{2}
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
			i := &systemInterface{apply: apply}
			if _, err := i.Set(spec); err != nil {
				return nil, err
			}
			device, err := a.device()
			if err != nil {
				return nil, err
			}
			i.device = device
			i.name, _ = device.Name()
			return i, nil
		},
		NetDNS:    dnsAddress + ":53",
		NetEcho:   tun.EchoPort,
		NetInside: true,
		Log:       log.New(logFile, "", log.LstdFlags),
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
// process and sets up as apply tells it. It goes when the system stops
// the VPN, so closing it does nothing.
type systemInterface struct {
	apply   func(Settings) error
	device  Device
	name    string
	applied tun.Spec // what the system is set to; its names are left out
}

func (i *systemInterface) Device() wgtun.Device { return i.device }

// Set brings the interface to spec. The names of spec are left to the
// DNS, which the system asks for them.
func (i *systemInterface) Set(spec tun.Spec) (string, error) {
	want := tun.Spec{Subnet: spec.Subnet, Address: spec.Address}
	if want.Subnet == i.applied.Subnet && want.Address == i.applied.Address {
		return i.name, nil
	}
	subnet, err := netip.ParsePrefix(spec.Subnet)
	if err != nil {
		return "", err
	}
	err = i.apply(Settings{
		Address: spec.Address,
		Routes:  []Route{route(subnet), route(netip.MustParsePrefix(dnsAddress + "/32"))},
		DNS:     dnsAddress,
		Domain:  dnsDomain,
		MTU:     tun.MTU,
	})
	if err != nil {
		return "", err
	}
	i.applied = want
	return i.name, nil
}

func (i *systemInterface) Close() error { return nil }
