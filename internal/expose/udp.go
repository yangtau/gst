package expose

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

const (
	udpIdle        = 2 * time.Minute
	maxUDPSessions = 256
	udpQueue       = 16
)

// Each source has its own connected socket, so replies cannot cross clients.
// Dialing and a slow destination never hold up the shared receive loop.
func serveUDP(ctx context.Context, socket *net.UDPConn, rule config.Expose) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	var mu sync.Mutex
	sessions := map[netip.AddrPort]chan []byte{}
	buf := make([]byte, 65535)
	for {
		n, from, err := socket.ReadFromUDPAddrPort(buf)
		if err != nil {
			return err
		}
		mu.Lock()
		queue := sessions[from]
		if queue == nil && len(sessions) < maxUDPSessions {
			queue = make(chan []byte, udpQueue)
			sessions[from] = queue
			wg.Go(func() {
				defer func() { mu.Lock(); delete(sessions, from); mu.Unlock() }()
				udpSession(ctx, socket, from, rule, queue, udpIdle)
			})
		}
		if queue != nil {
			select {
			case queue <- append([]byte{}, buf[:n]...):
			default:
			}
		}
		mu.Unlock()
	}
}

func udpSession(ctx context.Context, socket *net.UDPConn, client netip.AddrPort, rule config.Expose, queue <-chan []byte, idle time.Duration) {
	network, target := rule.Destination()
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, network, target)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Read deadlines are refreshed by traffic in either direction.
	conn.SetReadDeadline(time.Now().Add(idle))
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		buf := make([]byte, 65535)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			conn.SetReadDeadline(time.Now().Add(idle))
			if _, err := socket.WriteToUDPAddrPort(buf[:n], client); err != nil {
				return
			}
		}
	}()
	defer func() { conn.Close(); <-done }()
	for {
		select {
		case <-ctx.Done():
			return
		case packet := <-queue:
			conn.SetReadDeadline(time.Now().Add(idle))
			conn.SetWriteDeadline(time.Now().Add(dialTimeout))
			if _, err := conn.Write(packet); err != nil {
				return
			}
		}
	}
}
