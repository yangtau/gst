//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

const netInterface = "gst0"

// installHelper installs the helper of the virtual network for the gst
// user, as gst up runs it through sudo.
func (n *node) installHelper() {
	n.t.Helper()
	if out, err := n.exec("root", "env", "HOME=/root", "gst", "install", "--user", nodeUser); err != nil {
		n.t.Fatalf("gst install: %v: %s", err, out)
	}
	eventually(n.t, 10*time.Second, "the helper did not start", func() bool {
		_, err := n.exec("root", "test", "-S", "/var/run/gst-net.sock")
		return err == nil
	})
}

// helperGone waits until nothing of the helper is left.
func (n *node) helperGone() {
	n.t.Helper()
	eventually(n.t, 10*time.Second, "the helper still runs", func() bool {
		out, _ := n.exec("root", "sh", "-c", "pgrep -x gst-net || ls /var/run/gst-net.sock /usr/local/libexec/gst-net /etc/systemd/system/gst-net.service 2>/dev/null; true")
		return strings.TrimSpace(out) == ""
	})
}

// net returns the network as gst status reports it.
func (n *node) net() wire.NetState {
	n.t.Helper()
	if st := n.meshStatus().Self.Net; st != nil {
		return *st
	}
	return wire.NetState{}
}

// waitNet waits until n's network is up with peers others, and returns it.
func (n *node) waitNet(peers int) wire.NetState {
	n.t.Helper()
	var v wire.NetState
	deadline := time.Now().Add(60 * time.Second)
	for {
		v = n.net()
		if v.Interface != "" && v.Error == "" && len(v.Peers) == peers {
			return v
		}
		if time.Now().After(deadline) {
			n.t.Fatalf("the network of %s did not come up: %+v", n.container, v)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitPath waits until packets to peer go along path.
func (n *node) waitPath(peer, path string, within time.Duration) {
	n.t.Helper()
	start := time.Now()
	eventually(n.t, within, fmt.Sprintf("packets from %s to %s do not go along %s", n.container, peer, path), func() bool {
		for _, p := range n.net().Peers {
			if p.Name == peer {
				return p.Path == path
			}
		}
		return false
	})
	n.t.Logf("%s to %s: %s after %s", n.container, peer, path, time.Since(start).Round(time.Millisecond))
}

// pingable reports whether address answers a ping.
func (n *node) pingable(address string) bool {
	_, err := n.exec("root", "ping", "-c", "1", "-W", "1", address)
	return err == nil
}

func (n *node) waitPing(address string) {
	n.t.Helper()
	eventually(n.t, 30*time.Second, n.container+" cannot ping "+address, func() bool { return n.pingable(address) })
}

func (n *node) hasInterface() bool {
	_, err := n.exec("root", "ip", "link", "show", netInterface)
	return err == nil
}

// Machines a and c have no link between them, and packets go along the
// links through b: the network chooses a subnet, gives each machine an
// address and a name, and carries every protocol between them.
func TestNet(t *testing.T) {
	t.Parallel()
	a, b, c, _, _ := start3(t)
	nodes := []*node{a, b, c}
	for _, n := range nodes {
		n.installHelper()
	}
	view := map[*node]wire.NetState{}
	seen := map[string]bool{}
	for _, n := range nodes {
		view[n] = n.waitNet(2)
	}
	eventually(t, 30*time.Second, "the machines did not agree on a subnet", func() bool {
		clear(seen)
		for _, n := range nodes {
			view[n] = n.waitNet(2)
			seen[view[n].Subnet] = true
		}
		return len(seen) == 1
	})
	subnet := netip.MustParsePrefix(view[a].Subnet)
	addresses := map[string]bool{}
	for _, n := range nodes {
		address := netip.MustParseAddr(view[n].Address)
		if !subnet.Contains(address) {
			t.Fatalf("address %s is outside subnet %s", address, subnet)
		}
		addresses[view[n].Address] = true
		if routes, _ := n.exec("root", "ip", "route", "show", subnet.String()); !strings.Contains(routes, netInterface) {
			t.Fatalf("no route of %s to %s: %q", subnet, netInterface, routes)
		}
		ip := netip.MustParseAddr(n.ip)
		if subnet.Contains(ip) {
			t.Fatalf("subnet %s holds the address of container %s", subnet, ip)
		}
	}
	if len(addresses) != 3 {
		t.Fatalf("addresses are not unique: %v", addresses)
	}
	addressOfC := view[c].Address

	t.Run("Protocols", func(t *testing.T) {
		a.waitPing(addressOfC)
		c.startEchoOn("0.0.0.0:7000")
		if err := a.roundtrip("-conns", "4", addressOfC+":7000"); err != nil {
			t.Fatalf("TCP: %v", err)
		}
		c.mustSh(`setsid gst-it udp-echo 0.0.0.0:7001 </dev/null >/dev/null 2>&1 &`)
		out, err := a.helper("udp", addressOfC+":7001", "hello")
		if err != nil || strings.TrimSpace(out) != "reply:hello" {
			t.Fatalf("UDP: %q, %v", out, err)
		}
		for _, p := range a.net().Peers {
			if p.Path != wire.PathSSH {
				t.Fatalf("path to %s is %q without UDP paths", p.Name, p.Path)
			}
		}
	})

	t.Run("Names", func(t *testing.T) {
		eventually(t, 15*time.Second, "c.gst does not resolve", func() bool {
			out, _ := a.exec("root", "getent", "hosts", "c.gst")
			return strings.HasPrefix(out, addressOfC+" ")
		})
		a.waitPing("c.gst")
		// The link's name works too, and the daemon answers as it does for
		// systems that ask it.
		out, err := a.helper("dns", "127.0.0.1:15353", "tob.gst")
		if err != nil || strings.TrimSpace(out) != view[b].Address {
			t.Fatalf("dns tob.gst: %q, %v, want %s", out, err, view[b].Address)
		}
		if out, err := a.helper("dns", "127.0.0.1:15353", "nobody.gst"); err == nil {
			t.Fatalf("dns nobody.gst: %q", out)
		}
	})

	t.Run("Status", func(t *testing.T) {
		if out := a.mustGst("status"); !strings.Contains(out, addressOfC) {
			t.Fatalf("gst status lacks %s: %q", addressOfC, out)
		}
		if out := a.mustGst("status", "c"); !strings.Contains(out, "Address: "+addressOfC) {
			t.Fatalf("gst status c: %q", out)
		}
		if out := a.mustGst("status", "--all"); !strings.Contains(out, "HANDSHAKE") || !strings.Contains(out, " ago") {
			t.Fatalf("gst status --all shows no handshake with c: %q", out)
		}
	})

	t.Run("Subnet", func(t *testing.T) {
		for _, bad := range []string{"8.8.8.0/24", "10.0.0.0/7", "10.1.2.3/30", "nonsense"} {
			if code, _, stderr := a.gst("subnet", bad); code != 2 || !strings.Contains(stderr, "usage:") {
				t.Fatalf("gst subnet %s: exit %d: %q", bad, code, stderr)
			}
		}
		const chosen = "10.99.0.0/16"
		if out := a.mustGst("subnet", chosen); !strings.Contains(out, chosen) {
			t.Fatalf("gst subnet: %q", out)
		}
		next := netip.MustParsePrefix(chosen)
		eventually(t, 30*time.Second, "the others did not follow the subnet", func() bool {
			for _, n := range nodes {
				v := n.net()
				if v.Subnet != chosen || v.Address == "" || !next.Contains(netip.MustParseAddr(v.Address)) || len(v.Peers) != 2 {
					return false
				}
			}
			return true
		})
		addressOfC = c.net().Address
		a.waitPing(addressOfC)
		if routes, _ := a.exec("root", "ip", "route", "show", subnet.String()); strings.Contains(routes, netInterface) {
			t.Fatalf("the route of the old subnet is left: %q", routes)
		}

		// A subnet that a machine uses for something else is an error,
		// which says what to do.
		ip := netip.MustParseAddr(a.ip)
		taken, _ := ip.Prefix(24)
		code, _, stderr := a.gst("subnet", taken.String())
		if code != 1 || !strings.Contains(stderr, "overlaps") || !strings.Contains(stderr, "hint:") {
			t.Fatalf("gst subnet %s: exit %d: %q", taken, code, stderr)
		}
		if out := a.mustGst("status"); !strings.Contains(out, "overlaps") {
			t.Fatalf("gst status does not report the overlap: %q", out)
		}
		a.mustGst("subnet", chosen)
		a.waitNet(2)
		eventually(t, 30*time.Second, "the others did not return to the subnet", func() bool {
			return c.net().Subnet == chosen && b.net().Subnet == chosen
		})
		addressOfC = c.net().Address
		a.waitPing(addressOfC)
	})

	// gst ping goes where programs' packets go, and says which way.
	t.Run("Ping", func(t *testing.T) {
		reply := regexp.MustCompile(`^reply from (\w+) \(` + regexp.QuoteMeta(addressOfC) + `\) over SSH via b in [0-9]+\.[0-9] ms$`)
		for _, machine := range []string{"c", "c.gst", "C"} {
			out := a.mustGst("ping", machine, "-c", "1")
			if m := reply.FindStringSubmatch(strings.TrimSpace(out)); m == nil || m[1] != "c" {
				t.Fatalf("gst ping %s: %q", machine, out)
			}
		}
		var pings []wire.NetPing
		if err := json.Unmarshal([]byte(a.mustGst("ping", "c", "-c", "1", "--json")), &pings); err != nil {
			t.Fatal(err)
		}
		if len(pings) != 1 || pings[0].Node.Name != "c" || pings[0].Address != addressOfC || pings[0].Path != wire.PathSSH ||
			len(pings[0].Via) != 1 || pings[0].Via[0].Name != "b" || pings[0].TimeMS <= 0 {
			t.Fatalf("gst ping c --json: %+v", pings)
		}
		if out := a.mustGst("ping", "a", "-c", "1"); !strings.HasPrefix(out, "reply from a (") || strings.Contains(out, " over ") {
			t.Fatalf("gst ping a: %q", out)
		}
	})

	t.Run("Down", func(t *testing.T) {
		c.mustGst("down")
		eventually(t, 10*time.Second, "the interface outlived gst down", func() bool { return !c.hasInterface() })
		if out, _ := c.exec("root", "cat", "/etc/hosts"); strings.Contains(out, ".gst") {
			t.Fatalf("names are left after gst down: %q", out)
		}
		a.waitUnreachable("c")
		if a.pingable(addressOfC) {
			t.Fatal("c still answers")
		}
		c.helperGone()

		// It comes back with its address.
		c.mustGst("up")
		c.installHelper()
		a.waitReachable("c")
		a.waitPing(addressOfC)
	})
}

// Packets take UDP when a UDP path comes up, fall back to SSH within
// seconds when UDP stops working, and return when it works again.
func TestNetDirect(t *testing.T) {
	t.Parallel()
	client, server := startNode(t), startNode(t)
	client.direct, server.direct = true, true
	p := trust(t, client, server)
	client.mustGst("name", "laptop")
	server.mustGst("name", "devbox")
	client.mustGst("add", "devbox", p.destination())
	client.waitReachable("devbox")
	for _, n := range []*node{client, server} {
		n.installHelper()
	}
	var address string
	eventually(t, 30*time.Second, "the machines did not agree on a subnet", func() bool {
		c, s := client.waitNet(1), server.waitNet(1)
		address = s.Address
		return c.Subnet == s.Subnet
	})

	client.waitPing(address)
	client.waitPath("devbox", wire.PathDirect, 30*time.Second)
	server.waitPath("laptop", wire.PathDirect, 30*time.Second)
	client.waitPing(address)

	server.mustSh(`setsid gst-it sink 0.0.0.0:7002 </dev/null >/dev/null 2>&1 &`)
	rate := func(path string) float64 {
		out, err := client.helper("blast", address+":7002", "5")
		if err != nil {
			t.Fatalf("blast over %s: %v", path, err)
		}
		mbps, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("TCP through the network over %s: %.1f Mbit/s", path, mbps)
		return mbps
	}
	if rate("UDP") < 10 {
		t.Fatal("the UDP path is too slow")
	}
	// Video comes in a burst per frame, which must not overrun the queues
	// on the way.
	server.mustSh(`setsid gst-it udp-count 0.0.0.0:7003 </dev/null >/dev/null 2>&1 &`)
	for _, mbits := range []string{"50", "100", "200"} {
		out, err := client.helper("udp-burst", address+":7003", mbits, "3")
		if err != nil {
			t.Fatalf("udp-burst: %v", err)
		}
		t.Logf("UDP bursts through the network at %s Mbit/s: %s arrived", mbits, strings.TrimSpace(out))
		if share, err := strconv.ParseFloat(strings.TrimSpace(out), 64); err != nil || share < 0.95 {
			t.Fatalf("too many packets of the bursts were lost: %q", out)
		}
	}

	drop := func(flag string) {
		t.Helper()
		// UDP paths may run over either IP version.
		for _, tables := range []string{"iptables", "ip6tables"} {
			for _, chain := range []string{"INPUT", "OUTPUT"} {
				if _, err := server.exec("root", tables, flag, chain, "-p", "udp", "-j", "DROP"); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	drop("-A")
	start := time.Now()
	eventually(t, 15*time.Second, "packets did not fall back to SSH", func() bool { return client.pingable(address) })
	t.Logf("packets back after %s without UDP", time.Since(start).Round(time.Millisecond))
	client.waitPath("devbox", wire.PathSSH, 5*time.Second)
	if rate("SSH") < 1 {
		t.Fatal("packets over SSH are too slow")
	}

	drop("-D")
	start = time.Now()
	eventually(t, 150*time.Second, "packets did not return to UDP", func() bool {
		client.pingable(address)
		for _, peer := range client.net().Peers {
			if peer.Path == wire.PathDirect {
				return true
			}
		}
		return false
	})
	t.Logf("UDP again after %s", time.Since(start).Round(time.Millisecond))
	client.waitPing(address)
}

// The network needs the helper, says so, and starts by itself once the
// helper is there. It goes away with the daemon and returns with it.
func TestNetHelper(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	n := p.client
	n.mustGst("name", "laptop")
	code, _, stderr := n.gst("subnet", "10.99.0.0/16")
	if code != 1 || !strings.Contains(stderr, "hint:") || !strings.Contains(stderr, "gst up") {
		t.Fatalf("gst subnet without the helper: exit %d: %q", code, stderr)
	}
	eventually(t, 10*time.Second, "gst status does not report the missing helper", func() bool {
		out := n.mustGst("status")
		return strings.Contains(out, "virtual network") && strings.Contains(out, "gst up")
	})
	if code, stdout, _ := n.gst("ping", "laptop", "-c", "1"); code != 1 || !strings.Contains(stdout, "this machine is not on the virtual network") {
		t.Fatalf("gst ping without the helper: exit %d: %q", code, stdout)
	}
	if code, _, stderr := n.gst("install"); code != 1 || !strings.Contains(stderr, "gst up") {
		t.Fatalf("gst install as a user: exit %d: %q", code, stderr)
	}

	n.installHelper()
	if v := n.waitNet(0); v.Subnet != "10.99.0.0/16" {
		t.Fatalf("the choice of subnet was lost: %+v", v)
	}
	if !n.hasInterface() {
		t.Fatal("no interface")
	}
	if out, _ := n.exec("root", "stat", "-c", "%U %a", "/usr/local/libexec/gst-net"); strings.TrimSpace(out) != "root 755" {
		t.Fatalf("the helper's gst: %q", out)
	}
	if out, _ := n.exec("root", "stat", "-c", "%U %a", "/var/run/gst-net.sock"); strings.TrimSpace(out) != nodeUser+" 600" {
		t.Fatalf("the helper's socket: %q", out)
	}
	if out, _ := n.exec("root", "stat", "-c", "%a", "/etc/hosts"); strings.TrimSpace(out) != "644" {
		t.Fatalf("/etc/hosts: %q", out)
	}

	address := n.net().Address
	n.killDaemon()
	eventually(t, 10*time.Second, "the interface outlived the daemon", func() bool { return !n.hasInterface() })
	n.mustGst("up")
	n.waitNet(0)
	if got := n.net().Address; got != address {
		t.Fatalf("address %s became %s after a restart", address, got)
	}

	n.mustGst("down")
	eventually(t, 15*time.Second, "the interface outlived the helper", func() bool { return !n.hasInterface() })
	n.helperGone()
}

// gst up runs sudo only when told to, with a command that works as it is,
// and leaves a helper of the current version alone. The image has no sudo,
// so a stand-in records what would run, and the test runs that as root.
func TestUpInstallsTheHelper(t *testing.T) {
	t.Parallel()
	n := startNode(t)
	const record = "/tmp/sudo-args"
	script := "#!/bin/sh\necho \"$@\" >" + record + "\n"
	if out, err := n.exec("root", "sh", "-c", `printf '%s' "$1" >/usr/local/bin/sudo && chmod 755 /usr/local/bin/sudo`, "sh", script); err != nil {
		t.Fatalf("%v: %s", err, out)
	}

	code, _, stderr := n.gst("up")
	if code != 0 || !strings.Contains(stderr, "gst asks first") {
		t.Fatalf("gst up without a terminal: exit %d: %q", code, stderr)
	}
	if _, err := n.exec("root", "test", "-e", record); err == nil {
		t.Fatal("gst up ran sudo without being told to")
	}

	code, _, stderr = n.gst("up", "--yes")
	if code != 0 || !strings.Contains(stderr, "sudo /usr/local/bin/gst install --user "+nodeUser) || !strings.Contains(stderr, "/etc/hosts") {
		t.Fatalf("gst up --yes: exit %d: %q", code, stderr)
	}
	args, err := n.exec("root", "cat", record)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := n.exec("root", append([]string{"env", "HOME=/root"}, strings.Fields(args)...)...); err != nil {
		t.Fatalf("%s: %v: %s", args, err, out)
	}
	eventually(t, 10*time.Second, "the helper did not start", func() bool {
		_, err := n.exec("root", "test", "-S", "/var/run/gst-net.sock")
		return err == nil
	})
	n.waitNet(0)

	if _, err := n.exec("root", "rm", record); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = n.gst("up", "--yes")
	if code != 0 || stderr != "" {
		t.Fatalf("gst up with a current helper: exit %d: %q", code, stderr)
	}
	if _, err := n.exec("root", "test", "-e", record); err == nil {
		t.Fatal("gst up ran sudo for a current helper")
	}
}
