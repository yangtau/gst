package mesh

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"slices"
	"sync/atomic"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/direct"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

const (
	packetQueue    = 1024             // packets waiting for a stream; more are dropped, as a congested network does
	packetIdle     = time.Minute      // a stream that carries nothing for this long ends
	packetRetry    = time.Second      // after a failure to open a stream
	packetTimeout  = 10 * time.Second // to write a packet to a stream
	directInterval = 5 * time.Second  // how often traffic may ask for a direct link
	heartbeat      = time.Second      // how often a direct link says it works
	directSilence  = 3 * time.Second  // a direct link this quiet carries no packets
	maxPacketSize  = 1<<16 - 1
)

// The virtual network sends packets between nodes that may have no link
// between them. Packets take the direct link with the node when there is
// one, on the same UDP socket, and otherwise a stream along the links of
// the mesh, which works wherever connections do. They need no protection
// from the mesh: WireGuard encrypts them end to end, so the machines on
// the way see only ciphertext, and packets lost here count as lost on a
// network. Either side may open a stream, and either way it carries
// packets both ways; traffic also asks for a direct link, as a connection
// does, so that a path through the mesh moves to UDP as soon as it can.
//
// Heartbeats on a direct link tell within seconds that UDP stopped working.
// The connections on the link can wait for it to recover, but packets and
// new connections cannot: while it is silent, this node treats it as gone
// and uses its other links.

// packetPeer is what this node keeps for sending packets to one node.
type packetPeer struct {
	id      string
	direct  atomic.Pointer[direct.Conn]
	heard   atomic.Int64 // when the direct link last brought anything, in Unix nanoseconds; 0 for not yet
	queue   chan []byte  // for the streams
	streams atomic.Int32 // running
	dialing atomic.Bool
	asked   atomic.Int64 // when traffic last asked for a direct link, in Unix nanoseconds
}

func (n *Node) packetPeer(id string) *packetPeer {
	if p, ok := n.packets.Load(id); ok {
		return p.(*packetPeer)
	}
	p, _ := n.packets.LoadOrStore(id, &packetPeer{id: id, queue: make(chan []byte, packetQueue)})
	return p.(*packetPeer)
}

// HandlePackets makes handle receive the packets that other nodes send,
// with the id of the node that sent each; nil stops that, and streams are
// refused. handle must not keep the packet or block.
func (n *Node) HandlePackets(handle func(node string, packet []byte)) {
	if handle == nil {
		n.onPacket.Store(nil)
		return
	}
	n.onPacket.Store(&handle)
}

func (n *Node) deliver(node string, packet []byte) {
	if handle := n.onPacket.Load(); handle != nil {
		(*handle)(node, packet)
	}
}

// SendPacket sends packet to node id, or drops it. It does not block or
// keep packet.
func (n *Node) SendPacket(id string, packet []byte) {
	p := n.packetPeer(id)
	if c := p.directConn(); c != nil {
		// When this fails for long, heartbeats fail too. A stream opened
		// now would run over this link, and stall with it.
		c.WritePacket(packet)
		return
	}
	select {
	case p.queue <- slices.Clone(packet):
	default:
	}
	if p.streams.Load() == 0 && p.dialing.CompareAndSwap(false, true) {
		go n.dialPackets(p)
	}
	now := time.Now().UnixNano()
	if last := p.asked.Load(); now-last >= int64(directInterval) && p.asked.CompareAndSwap(last, now) {
		n.tryDirect(id)
	}
}

// PacketPath tells how packets go to node id now.
func (n *Node) PacketPath(id string) string {
	if n.packetPeer(id).directConn() != nil {
		return wire.PathDirect
	}
	return wire.PathMesh
}

// directConn returns the direct link, unless it is silent.
func (p *packetPeer) directConn() *direct.Conn {
	if p.silent() {
		return nil
	}
	return p.direct.Load()
}

// silent reports whether the direct link stopped bringing anything. A
// link that never did has a peer without heartbeats, and tells nothing.
func (p *packetPeer) silent() bool {
	heard := p.heard.Load()
	return heard != 0 && time.Now().UnixNano()-heard > int64(directSilence)
}

// usable reports whether l can carry new connections now.
func (n *Node) usable(l *link) bool {
	return !l.direct || !n.packetPeer(l.peer.ID).silent()
}

// dialPackets opens a stream to p's node along the mesh.
func (n *Node) dialPackets(p *packetPeer) {
	defer p.dialing.Store(false)
	conn, err := n.openPackets(p.id)
	if err != nil {
		// The packets are old by the next try, and the protocols above
		// send again.
		for len(p.queue) > 0 {
			<-p.queue
		}
		select {
		case <-time.After(packetRetry):
		case <-n.life.Done():
		}
		return
	}
	p.streams.Add(1)
	go n.runPackets(p, conn)
}

func (n *Node) openPackets(id string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(n.life, relayTimeout)
	defer cancel()
	conn, err := n.Dial(ctx, id, wire.NetworkGst, "")
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	err = wire.Call(conn, wire.MethodNetPackets, wire.PacketsParams{Node: n.Self().ID}, nil)
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func (n *Node) servePackets(_ context.Context, conn net.Conn, params wire.PacketsParams, accept func(any) error) error {
	if !config.ValidNodeID(params.Node) || params.Node == n.Self().ID {
		return wire.Errorf(wire.CodeInvalidParams, "invalid node")
	}
	if n.onPacket.Load() == nil {
		return wire.Errorf(wire.CodeFailed, "%s is not on the virtual network", n.Self().Name)
	}
	if err := accept(nil); err != nil {
		return err
	}
	p := n.packetPeer(params.Node)
	p.streams.Add(1)
	n.runPackets(p, conn)
	return nil
}

// runPackets carries the packets of conn in both directions until it
// fails or goes idle, and closes it. The caller counted it in p.streams.
func (n *Node) runPackets(p *packetPeer, conn net.Conn) {
	defer p.streams.Add(-1)
	defer conn.Close()
	defer context.AfterFunc(n.life, func() { conn.Close() })()
	var active atomic.Bool
	failed := make(chan struct{})
	go func() {
		defer close(failed)
		r := bufio.NewReaderSize(conn, 64<<10)
		var size [2]byte
		packet := make([]byte, maxPacketSize)
		for {
			if _, err := io.ReadFull(r, size[:]); err != nil {
				return
			}
			length := int(binary.BigEndian.Uint16(size[:]))
			if _, err := io.ReadFull(r, packet[:length]); err != nil {
				return
			}
			active.Store(true)
			n.deliver(p.id, packet[:length])
		}
	}()
	idle := time.NewTicker(packetIdle)
	defer idle.Stop()
	frame := make([]byte, 0, 2048)
	for {
		select {
		case <-failed:
			return
		case <-idle.C:
			if !active.Swap(false) {
				return
			}
		case packet := <-p.queue:
			active.Store(true)
			if len(packet) > maxPacketSize {
				continue
			}
			frame = binary.BigEndian.AppendUint16(frame[:0], uint16(len(packet)))
			frame = append(frame, packet...)
			conn.SetWriteDeadline(time.Now().Add(packetTimeout))
			if _, err := conn.Write(frame); err != nil {
				return
			}
		}
	}
}

// attachPackets makes packets to node id take conn, a direct link, and
// hands on what arrives on it.
func (n *Node) attachPackets(id string, conn *direct.Conn) {
	p := n.packetPeer(id)
	p.heard.Store(0)
	p.direct.Store(conn)
	conn.HandlePackets(func(packet []byte) {
		p.heard.Store(time.Now().UnixNano())
		if len(packet) > len(heartbeatPacket) {
			n.deliver(id, packet)
		}
	})
	go func() {
		tick := time.NewTicker(heartbeat)
		defer tick.Stop()
		silent := false
		for {
			select {
			case <-conn.Done():
				return
			case <-tick.C:
				conn.WritePacket(heartbeatPacket)
				if now := p.silent(); now != silent {
					silent = now
					n.Update()
				}
			}
		}
	}()
}

// heartbeatPacket has a first byte that QUIC and WireGuard leave alone,
// and fewer bytes than any packet of theirs.
var heartbeatPacket = []byte{0}

// detachPackets undoes attachPackets when conn ended.
func (n *Node) detachPackets(id string, conn *direct.Conn) {
	p := n.packetPeer(id)
	if p.direct.CompareAndSwap(conn, nil) {
		p.heard.Store(0)
	}
}
