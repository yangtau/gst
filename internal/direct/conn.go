package direct

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

const (
	beatInterval = time.Second     // how often a path says it works; also keeps it open through NATs
	silenceLimit = 3 * time.Second // a path this quiet has ended
)

// Conn is a UDP path to the other side: one socket and one address, which
// carry packets until the path falls silent or a side closes it.
type Conn struct {
	socket *socket
	to     netip.AddrPort
	token  [tokenLen]byte
	handle atomic.Pointer[func(packet []byte)]
	heard  atomic.Int64 // when anything last arrived, in Unix nanoseconds

	once sync.Once
	done chan struct{}
}

func newConn(s *socket, to netip.AddrPort, token [tokenLen]byte) *Conn {
	c := &Conn{socket: s, to: to, token: token, done: make(chan struct{})}
	c.heard.Store(time.Now().UnixNano())
	receive := c.receive
	s.receive.Store(&receive)
	go c.beat()
	return c
}

// Local returns the IP:port of this end.
func (c *Conn) Local() string { return c.socket.addr.String() }

// Remote returns the IP:port of the other end, as this one sees it.
func (c *Conn) Remote() string { return c.to.String() }

// Done is closed when the path ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// WritePacket sends a WireGuard packet to the other side, over the socket
// that has a way through the firewalls and NATs between the two.
func (c *Conn) WritePacket(p []byte) error {
	_, err := c.socket.udp.WriteToUDPAddrPort(p, c.to)
	return err
}

// HandlePackets makes handle receive the WireGuard packets that arrive,
// on the goroutine that reads the socket: it must not block or keep the
// packet. Packets are not authenticated; WireGuard does that.
func (c *Conn) HandlePackets(handle func(packet []byte)) {
	c.handle.Store(&handle)
}

// Close ends the path, telling the other side, and frees its socket.
func (c *Conn) Close() error {
	c.socket.send(kindBye, c.token, c.to)
	c.end()
	return nil
}

func (c *Conn) end() {
	c.once.Do(func() {
		close(c.done)
		c.socket.udp.Close()
	})
}

// wireGuard reports whether packet is one of WireGuard's, by its message
// type.
func wireGuard(packet []byte) bool {
	return len(packet) > 0 && packet[0] >= 1 && packet[0] <= 4
}

func (c *Conn) receive(packet []byte, from netip.AddrPort) {
	if from != c.to {
		return
	}
	c.heard.Store(time.Now().UnixNano())
	if wireGuard(packet) {
		if handle := c.handle.Load(); handle != nil {
			(*handle)(packet)
		}
		return
	}
	if !signed(packet, c.token) {
		return
	}
	switch packet[0] {
	case kindUse:
		// The confirmation got lost.
		c.socket.send(kindUsed, c.token, c.to)
	case kindBye:
		c.end()
	}
}

// beat says that this end is there, and ends the path when the other end
// stopped saying so.
func (c *Conn) beat() {
	tick := time.NewTicker(beatInterval)
	defer tick.Stop()
	for {
		c.socket.send(kindBeat, c.token, c.to)
		select {
		case <-c.done:
			return
		case <-tick.C:
			if time.Since(time.Unix(0, c.heard.Load())) > silenceLimit {
				c.end()
				return
			}
		}
	}
}
