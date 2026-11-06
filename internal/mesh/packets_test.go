package mesh

import (
	"sync"
	"testing"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// inbox collects the packets that a node receives.
type inbox struct {
	mu  sync.Mutex
	got map[string]bool // "node packet"
}

func (i *inbox) handle(node string, packet []byte) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.got == nil {
		i.got = map[string]bool{}
	}
	i.got[node+" "+string(packet)] = true
}

func (i *inbox) has(node, packet string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.got[node+" "+packet]
}

// Without a UDP path, packets take a stream along the links, also
// through a machine in between, and the stream carries them both ways.
func TestPacketsAlongTheMesh(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	linkUp(t, a, b, "tob")
	linkUp(t, c, b, "tob")
	waitFor(t, "a reaches c", func() bool { return reaches(a, "c", "a", "b", "c") })
	var atA, atC inbox
	a.HandlePackets(atA.handle)
	c.HandlePackets(atC.handle)

	a.SendPacket(nid("c"), []byte("hello"))
	waitFor(t, "c gets the packet", func() bool { return atC.has(nid("a"), "hello") })
	if got := a.PacketPath(nid("c")); got != wire.PathSSH {
		t.Fatalf("path = %q", got)
	}
	c.SendPacket(nid("a"), []byte("back"))
	waitFor(t, "a gets the packet", func() bool { return atA.has(nid("c"), "back") })
	if got := a.packetPeer(nid("c")).streams.Load(); got != 1 {
		t.Fatalf("%d streams at a", got)
	}
}

// A machine off the network refuses streams; the sender drops the packets
// and tries again with later ones.
func TestPacketsToAMachineOffTheNetwork(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	linkUp(t, a, b, "tob")
	a.SendPacket(nid("b"), []byte("lost"))
	waitFor(t, "the try ends", func() bool { return !a.packetPeer(nid("b")).dialing.Load() })

	var atB inbox
	b.HandlePackets(atB.handle)
	waitFor(t, "b gets a later packet", func() bool {
		a.SendPacket(nid("b"), []byte("later"))
		return atB.has(nid("a"), "later")
	})
	if atB.has(nid("a"), "lost") {
		t.Fatal("a packet from before the failure arrived")
	}
}

func TestPacketsRefuseBadNodes(t *testing.T) {
	a := newNode(t, "a")
	a.HandlePackets(func(string, []byte) {})
	err := a.servePackets(t.Context(), nil, wire.PacketsParams{Node: "nonsense"}, nil)
	wantCode(t, err, wire.CodeInvalidParams)
	err = a.servePackets(t.Context(), nil, wire.PacketsParams{Node: nid("a")}, nil)
	wantCode(t, err, wire.CodeInvalidParams)
}

func TestValidNet(t *testing.T) {
	key := "0000000000000000000000000000000000000000000000000000000000000000"
	good := wire.NetInfo{Subnet: "10.99.0.0/16", Address: "10.99.1.2", PublicKey: key}
	if !validNet(nil) || !validNet(&good) {
		t.Fatal("a good record is invalid")
	}
	for _, bad := range []wire.NetInfo{
		{Subnet: "8.8.8.0/24", Address: "8.8.8.8", PublicKey: key},
		{Subnet: "10.99.0.0/16", Address: "10.98.1.2", PublicKey: key},
		{Subnet: "10.99.0.0/16", Address: "nonsense", PublicKey: key},
		{Subnet: "10.99.0.0/16", Address: "10.99.1.2", PublicKey: "beef"},
		{Subnet: "10.99.0.0/16", Address: "10.99.1.2", PublicKey: "not hex"},
	} {
		if validNet(&bad) {
			t.Errorf("%+v is valid", bad)
		}
	}
}

// The network's part of a record reaches the other machines, and leaves
// them when the machine takes it back.
func TestNetSpreads(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	linkUp(t, a, b, "tob")
	key := "0000000000000000000000000000000000000000000000000000000000000000"
	info := func(n *Node, id string) *wire.NetInfo {
		v, _ := findTopo(n.Topology(), id)
		return v.Net
	}
	a.SetNet(&wire.NetInfo{Subnet: "10.99.0.0/16", Since: 1, Address: "10.99.1.2", PublicKey: key})
	waitFor(t, "b learns a's address", func() bool {
		v := info(b, nid("a"))
		return v != nil && v.Address == "10.99.1.2"
	})
	a.SetNet(nil)
	waitFor(t, "b forgets a's address", func() bool { return info(b, nid("a")) == nil })
}
