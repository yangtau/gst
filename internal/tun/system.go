package tun

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// system does what the helper needs of this machine.
type system interface {
	// open creates the interface. Closing the file removes it.
	open() (*os.File, string, error)
	// number gives the interface an address and routes subnet to it.
	number(name string, address netip.Addr, subnet netip.Prefix) error
	// unnumber undoes number.
	unnumber(name string, address netip.Addr, subnet netip.Prefix)
	// resolve makes the names under .gst resolve, by asking the daemon or
	// from hosts.
	resolve(hosts map[string]netip.Addr) error
	// unresolve undoes resolve.
	unresolve()
	// changes says what the helper does to this system, for people to read.
	changes() []string
}

// Changes says what the helper does to this machine, for the person who
// decides to install it.
func Changes() []string { return newSystem().changes() }

func runCommand(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// writeRoot writes a system file; it reports whether that changed it.
func writeRoot(path string, data []byte) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, data, 0o644)
}

// darwin configures macOS. /etc/resolver sends .gst, and only .gst, to
// the daemon.
type darwin struct {
	run      func(name string, args ...string) error
	resolver string // the resolver file of .gst
}

func newDarwinSystem() *darwin {
	return &darwin{run: runCommand, resolver: "/etc/resolver/gst"}
}

func (d *darwin) open() (*os.File, string, error) { return openDevice() }

func (d *darwin) number(name string, address netip.Addr, subnet netip.Prefix) error {
	a := address.String()
	if err := d.run("/sbin/ifconfig", name, "inet", a, a, "mtu", strconv.Itoa(MTU), "up"); err != nil {
		return err
	}
	return d.run("/sbin/route", "-q", "-n", "add", "-inet", subnet.String(), "-interface", name)
}

func (d *darwin) unnumber(name string, address netip.Addr, subnet netip.Prefix) {
	d.run("/sbin/route", "-q", "-n", "delete", "-inet", subnet.String(), "-interface", name)
	d.run("/sbin/ifconfig", name, "inet", address.String(), "delete")
}

func (d *darwin) resolve(map[string]netip.Addr) error {
	_, err := writeRoot(d.resolver, []byte("# Written by gst.\nnameserver "+dnsHost+"\nport "+dnsPort+"\n"))
	return err
}

func (d *darwin) unresolve() { os.Remove(d.resolver) }

func (d *darwin) changes() []string {
	return []string{
		"creates a utun network interface and sets its address",
		"routes the subnet of the network to that interface",
		"writes " + d.resolver + ", which sends names under .gst, and only those, to the gst daemon",
	}
}

// interfaceName is the interface on Linux.
const interfaceName = "gst0"

// linux configures Linux. systemd-resolved sends .gst to the daemon; on a
// machine without it, nothing can do that for one domain alone, so the
// machines' names go in /etc/hosts.
type linux struct {
	run      func(name string, args ...string) error
	resolved string // the directory that shows systemd-resolved runs
	dropIn   string // the configuration of .gst for systemd-resolved
	hosts    string // /etc/hosts
}

func newLinuxSystem() *linux {
	return &linux{
		run:      runCommand,
		resolved: "/run/systemd/resolve",
		dropIn:   "/etc/systemd/resolved.conf.d/gst.conf",
		hosts:    "/etc/hosts",
	}
}

func (l *linux) open() (*os.File, string, error) { return openDevice() }

func (l *linux) number(name string, address netip.Addr, subnet netip.Prefix) error {
	if err := l.run("ip", "link", "set", "dev", name, "mtu", strconv.Itoa(MTU), "up"); err != nil {
		return err
	}
	// The address's prefix makes the kernel route the subnet to the
	// interface.
	return l.run("ip", "address", "replace", netip.PrefixFrom(address, subnet.Bits()).String(), "dev", name)
}

func (l *linux) unnumber(name string, address netip.Addr, subnet netip.Prefix) {
	l.run("ip", "address", "delete", netip.PrefixFrom(address, subnet.Bits()).String(), "dev", name)
}

func (l *linux) usesResolved() bool {
	info, err := os.Stat(l.resolved)
	return err == nil && info.IsDir()
}

func (l *linux) resolve(hosts map[string]netip.Addr) error {
	if !l.usesResolved() {
		return writeHosts(l.hosts, hosts)
	}
	changed, err := writeRoot(l.dropIn, []byte("# Written by gst.\n[Resolve]\nDNS="+DNSAddress+"\nDomains=~gst\nDNSSEC=false\n"))
	if err != nil || !changed {
		return err
	}
	return l.run("systemctl", "restart", "systemd-resolved")
}

func (l *linux) unresolve() {
	if l.usesResolved() {
		if os.Remove(l.dropIn) == nil {
			l.run("systemctl", "restart", "systemd-resolved")
		}
		return
	}
	writeHosts(l.hosts, nil)
}

func (l *linux) changes() []string {
	names := "adds the names of the machines to " + l.hosts + ", between lines that mark them as gst's"
	if l.usesResolved() {
		names = "writes " + l.dropIn + " and restarts systemd-resolved, which sends names under .gst, and only those, to the gst daemon"
	}
	return []string{
		"creates the network interface " + interfaceName + " and sets its address",
		"routes the subnet of the network to that interface",
		names,
	}
}

const (
	hostsBegin = "# gst begin"
	hostsEnd   = "# gst end"
)

// writeHosts replaces the lines of gst in the hosts file at path.
func writeHosts(path string, hosts map[string]netip.Addr) error {
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var out bytes.Buffer
	inside := false
	for line := range strings.Lines(string(old)) {
		switch strings.TrimSpace(line) {
		case hostsBegin:
			inside = true
			continue
		case hostsEnd:
			inside = false
			continue
		}
		if !inside {
			out.WriteString(line)
		}
	}
	if len(hosts) > 0 {
		if out.Len() > 0 && !bytes.HasSuffix(out.Bytes(), []byte("\n")) {
			out.WriteByte('\n')
		}
		out.WriteString(hostsBegin + "\n")
		for _, name := range slices.Sorted(maps.Keys(hosts)) {
			fmt.Fprintf(&out, "%s %s\n", hosts[name], name)
		}
		out.WriteString(hostsEnd + "\n")
	}
	if bytes.Equal(old, out.Bytes()) {
		return nil
	}
	tmp := path + ".gst"
	if err := os.WriteFile(tmp, out.Bytes(), 0o644); err != nil {
		return err
	}
	if os.Rename(tmp, path) == nil {
		return nil
	}
	// A container mounts the hosts file, which cannot be replaced.
	os.Remove(tmp)
	return os.WriteFile(path, out.Bytes(), 0o644)
}
