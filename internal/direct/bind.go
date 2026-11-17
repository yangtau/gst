//go:build darwin || (linux && !android)

package direct

// defaultInterfaces returns the interfaces for the sockets to send through,
// for IPv4 and for IPv6. When the routes lead packets for the internet into
// a tunnel, as when a VPN or a proxy took them, that is the interface below
// it that reaches the internet (see physicalInterfaces); otherwise 0, which
// leaves it to the routes, as they know which interface reaches an address
// on another network of the machine, behind a second network card or a
// bridge to containers. A bound socket sends to such an address through the
// one interface, where it goes nowhere.
func (m interfaces) defaultInterfaces() (v4, v6 int) {
	into4, into6 := m.intoTunnel("udp4", internet4), m.intoTunnel("udp6", internet6)
	if !into4 && !into6 {
		return 0, 0
	}
	v4, v6 = physicalInterfaces(m)
	if !into4 {
		v4 = 0
	}
	if !into6 {
		v6 = 0
	}
	return v4, v6
}
