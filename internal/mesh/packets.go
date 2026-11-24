package mesh

import (
	"encoding/hex"
	"sync/atomic"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/direct"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// directInterval is how often traffic may ask for a UDP path.
const directInterval = 5 * time.Second

// The virtual network sends packets between nodes that may have no link
// between them. A node's own packets take the UDP path to the other node
// when there is one, else the UDP paths of a relay that the two nodes
// agreed on (relay.go), and otherwise go as routed messages along the SSH
// links, which works wherever the mesh does. They need no protection from
// the mesh: WireGuard encrypts them end to end, so the nodes on the way
// see only ciphertext, and packets lost here count as lost on a network.
// Traffic also asks for a UDP path, so that packets move to UDP as soon
// as they can.

// packetPeer is what this node keeps for sending packets to one node.
type packetPeer struct {
	key    [direct.NodeLen]byte        // the node's id, as the packets that a relay carries name it
	direct atomic.Pointer[direct.Path] // the UDP path; nil while there is none
	asked  atomic.Int64                // when traffic last asked for a UDP path, in Unix nanoseconds

	// The relay that packets go through while there is no UDP path; see
	// relay.go. Times are in Unix nanoseconds.
	relay      atomic.Pointer[string] // its id; nil while there is none
	relayHeard atomic.Int64           // when a packet last came back through it, or the nodes agreed on it
	relayAsked atomic.Int64           // when the latest try at one started
	relayFails atomic.Int32           // tries that failed in a row

	writeErrors atomic.Uint64 // packets that UDP could not send
	queueDrops  atomic.Uint64 // packets lost as the links' queue was full
}

func (n *Node) packetPeer(id string) *packetPeer {
	if p, ok := n.packets.Load(id); ok {
		return p.(*packetPeer)
	}
	fresh := &packetPeer{}
	hex.Decode(fresh.key[:], []byte(id))
	p, _ := n.packets.LoadOrStore(id, fresh)
	return p.(*packetPeer)
}

// HandlePackets makes handle receive the packets that other nodes send,
// with the id of the node that sent each; nil stops that. handle must not
// keep the packet or block.
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
	if c := p.direct.Load(); c != nil {
		err := c.WritePacket(packet)
		if err == nil {
			return
		}
		p.writeErrors.Add(1) // Such as a network gone: the links may still lead there.
	}
	if !n.relayPacket(p, packet) && !n.router.SendPacket(id, packet) {
		p.queueDrops.Add(1)
	}
	now := time.Now().UnixNano()
	if last := p.asked.Load(); now-last >= int64(directInterval) && p.asked.CompareAndSwap(last, now) {
		n.tryDirect(id)
		n.tryRelay(id)
	}
}

// PacketWay tells how packets go to node id now: wire.WayDirect,
// wire.WayUDPRelay, wire.WayRelay, or empty when none leads there.
func (n *Node) PacketWay(id string) string {
	switch {
	case n.packetPeer(id).direct.Load() != nil:
		return wire.WayDirect
	case n.PacketRelay(id) != "":
		return wire.WayUDPRelay
	case n.router.Reaches(id):
		return wire.WayRelay
	}
	return ""
}

// PacketEndpoint returns the IP:port that packets to node id go to, when
// they go over UDP. It holds an address, so it stays out of the log.
func (n *Node) PacketEndpoint(id string) string {
	if c := n.packetPeer(id).direct.Load(); c != nil {
		return c.Remote()
	}
	return ""
}

// receiveOn hands on what arrives on path, a try at a UDP path to node id
// or the path that came of it. The path lets packets through once this
// node knows that it works, when the other node may send on it already.
func (n *Node) receiveOn(id string, path *direct.Path) {
	key := n.packetPeer(id).key
	path.HandlePackets(func(packet []byte) { n.deliver(id, packet) })
	path.HandleRelay(func(kind direct.Relay, node [direct.NodeLen]byte, packet []byte) {
		n.carry(id, key, kind, node, packet)
	})
}

// attachPackets makes packets to node id take path.
func (n *Node) attachPackets(id string, path *direct.Path) {
	n.packetPeer(id).direct.Store(path)
}

// detachPackets undoes attachPackets when path ended.
func (n *Node) detachPackets(id string, path *direct.Path) {
	n.packetPeer(id).direct.CompareAndSwap(path, nil)
}
