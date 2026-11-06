package direct

import (
	"net"
	"sync/atomic"

	"golang.org/x/net/ipv4"
)

// demux is a socket as quic-go reads it, which takes out the packets that
// are not QUIC and hands them over on the spot. quic-go can do this too,
// through a queue of 32 packets that drops the rest: too few for a frame
// of video, which arrives as a burst of hundreds. Here the socket's own
// buffer does the queueing.
//
// quic-go reads with ReadBatch when a socket has it, and the embedded
// socket gives it all else that it wants, such as ECN and GSO.
type demux struct {
	*net.UDPConn
	batch  *ipv4.PacketConn
	handle atomic.Pointer[func(packet []byte)]
}

func newDemux(udp *net.UDPConn) *demux {
	return &demux{UDPConn: udp, batch: ipv4.NewPacketConn(udp)}
}

// nonQUIC reports what quic-go takes for no QUIC packet: the first two
// bits are clear. WireGuard's message types are 1 to 4.
func nonQUIC(packet []byte) bool {
	return len(packet) > 0 && packet[0]&0xc0 == 0
}

// ReadBatch reads packets for quic-go, which pairs each message with a
// buffer of its own, so the packets that stay move between the messages.
func (d *demux) ReadBatch(messages []ipv4.Message, flags int) (int, error) {
	for {
		n, err := d.batch.ReadBatch(messages, flags)
		handle := d.handle.Load()
		if err != nil || handle == nil {
			return n, err
		}
		kept := 0
		for i := range messages[:n] {
			m := &messages[i]
			if packet := m.Buffers[0][:m.N]; nonQUIC(packet) {
				(*handle)(packet)
				continue
			}
			if kept != i {
				to := &messages[kept]
				copy(to.Buffers[0], m.Buffers[0][:m.N])
				copy(to.OOB, m.OOB[:m.NN])
				to.N, to.NN, to.Flags, to.Addr = m.N, m.NN, m.Flags, m.Addr
			}
			kept++
		}
		if kept > 0 {
			return kept, nil
		}
	}
}
