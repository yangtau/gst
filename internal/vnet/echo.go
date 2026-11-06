package vnet

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"strconv"
	"time"
)

// A ping travels as any program's packets do: a UDP packet to a machine's
// address goes through the routes, the interface, WireGuard, and the path
// to that machine, whose daemon sends it back. So a reply says that all of
// it works, and how long it takes.

const (
	pingTimeout = 2 * time.Second
	pingSize    = 16
)

// Ping sends a packet to the daemon at address in the network and waits
// for it to come back.
func (m *Manager) Ping(ctx context.Context, address string) (time.Duration, error) {
	return ping(ctx, net.JoinHostPort(address, strconv.Itoa(m.cfg.Echo)))
}

func ping(ctx context.Context, address string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", address)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { conn.Close() })()
	sent := make([]byte, pingSize)
	rand.Read(sent)
	start := time.Now()
	if _, err := conn.Write(sent); err != nil {
		return 0, err
	}
	got := make([]byte, pingSize+1)
	for {
		n, err := conn.Read(got)
		if err != nil {
			return 0, fmt.Errorf("timed out after %s", pingTimeout)
		}
		if bytes.Equal(got[:n], sent) {
			return time.Since(start), nil
		}
	}
}

// answerPings answers on this machine's address, once the interface has it.
func (m *Manager) answerPings(ctx context.Context) error {
	if m.cfg.Echo == 0 || m.echoing == m.state.Address {
		return nil
	}
	m.stopAnswering()
	conn, err := net.ListenPacket("udp", net.JoinHostPort(m.state.Address, strconv.Itoa(m.cfg.Echo)))
	if err != nil {
		return fmt.Errorf("answer gst ping: %w", err)
	}
	echoCtx, stop := context.WithCancel(ctx)
	m.stopEcho, m.echoing = stop, m.state.Address
	go serveEcho(echoCtx, conn)
	return nil
}

func (m *Manager) stopAnswering() {
	if m.stopEcho != nil {
		m.stopEcho()
		m.stopEcho, m.echoing = nil, ""
	}
}

// serveEcho sends back what arrives until ctx is done, and then closes conn.
// A reply is as long as the request, and only machines of the network get
// to this address.
func serveEcho(ctx context.Context, conn net.PacketConn) {
	defer context.AfterFunc(ctx, func() { conn.Close() })()
	packet := make([]byte, pingSize+1)
	for {
		n, from, err := conn.ReadFrom(packet)
		if err != nil {
			return
		}
		conn.WriteTo(packet[:n], from)
	}
}
