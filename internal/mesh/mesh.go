// Package mesh is this machine's node in the mesh of gst daemons, in three
// layers. Links (link) join it to its neighbours over SSH. The table of the
// network (state), which every node holds and floods to its neighbours over
// the links, says which nodes are in the network, the links between them, and
// the subnet of the virtual network. Routing (route) takes a message to
// any node along the links, by a routing table that each node works out
// from the links in its table. On top, this package keeps the node's own
// entry, decides who may link and who is in the network, and carries the
// packets of the virtual network: over a UDP path where the network allows
// one (direct.go), and as routed messages where not (packets.go).
//
// A node stays in the network while it is offline. Only a removal record,
// which any node can write (gst remove), takes a node out, for good: it covers
// the generations of the node up to its own, and only a person who adds
// the node again (gst add) lets it come back with the next generation.
package mesh

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/direct"
	"github.com/yangtau/ssh-tunnel/internal/mesh/link"
	"github.com/yangtau/ssh-tunnel/internal/mesh/route"
	"github.com/yangtau/ssh-tunnel/internal/mesh/state"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// refresh is how often a node publishes its entry when nothing changed, so
// that the others know when it was last online.
const refresh = 10 * time.Minute

// Config configures a node.
type Config struct {
	Software string
	Log      *log.Logger // the daemon's log
	// StatePath is where the table is saved; it stays in memory when
	// empty.
	StatePath string
	// Status reports the daemon to the other nodes; nil answers nothing.
	Status func() wire.Status
	// SaveGeneration keeps this node's generation, which a person's gst
	// add raises after a removal, in the configuration file.
	SaveGeneration func(int64) error
	// Timing is the links'; link.DefaultTiming when zero.
	Timing link.Timing
}

// Node is this machine's node in the mesh.
type Node struct {
	cfg    Config
	log    *log.Logger
	table  *state.Table
	flood  *state.Flood
	router *route.Router
	update chan struct{} // asks Run to look again

	mu         sync.Mutex
	info       wire.NodeInfo // ID is empty until Configure gives the node an identity
	generation int64
	lastSeq    int64
	links      map[*link.Link]struct{}
	net        *wire.NodeNet        // this node's part in the virtual network; see SetNet
	removed    *wire.Removed        // set while this node is removed from the network
	routes     map[string][]string  // shortest routes along the links, from this node, by node id
	lost       map[string]time.Time // when nodes stopped being reachable, by node id
	changed    chan struct{}        // closed and replaced at every change
	moved      chan struct{}        // closed and replaced when the machine's addresses change; see Moved
	seenTable  <-chan struct{}      // the table's Changed at the last change
	seenDirect string               // the nodes with UDP paths at the last change

	// UDP paths; see EnableDirect. directCtx is nil while they are off.
	directCtx   context.Context
	udp         *direct.Transport
	directTries map[string]*directTry         // by node id
	directWaits map[string]directWait         // by node id
	directTrace map[string][]wire.DirectEvent // the latest try with each node
	directPrev  map[string][]wire.DirectEvent // the one before it
	directIdle  time.Duration                 // see runDirect

	subnetPrefix atomic.Pointer[netip.Prefix] // of the virtual network; nil off it
	onPacket     atomic.Pointer[func(node string, packet []byte)]
	packets      sync.Map // node id -> *packetPeer
}

// New returns a node without an identity, which refuses links until
// Configure gives it one.
func New(cfg Config) *Node {
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	table := state.Open(cfg.StatePath, cfg.Log)
	n := &Node{
		cfg: cfg, log: cfg.Log,
		table: table, flood: state.NewFlood(table), router: route.New(),
		update:  make(chan struct{}, 1),
		links:   map[*link.Link]struct{}{},
		routes:  map[string][]string{},
		lost:    map[string]time.Time{},
		changed: make(chan struct{}),
		moved:   make(chan struct{}),

		directTries: map[string]*directTry{},
		directWaits: map[string]directWait{},
		directTrace: map[string][]wire.DirectEvent{},
		directPrev:  map[string][]wire.DirectEvent{},
		directIdle:  directIdle,
	}
	route.Handle(n.router, wire.MethodPeerPing, func(context.Context, string, struct{}) (any, error) { return n.Self(), nil })
	route.Handle(n.router, wire.MethodDirectOffer, n.serveOffer)
	route.Handle(n.router, wire.MethodDirectUpdate, n.serveUpdate)
	route.Handle(n.router, wire.MethodDirectTrace, n.serveTrace)
	if cfg.Status != nil {
		Handle(n, wire.MethodPeerStatus, func(context.Context, struct{}) (any, error) { return cfg.Status(), nil })
	}
	n.router.HandlePackets(n.deliver)
	return n
}

// Handle makes fn answer the calls of method from other nodes.
func Handle[P any](n *Node, method string, fn func(ctx context.Context, p P) (any, error)) {
	route.Handle(n.router, method, func(ctx context.Context, _ string, p P) (any, error) { return fn(ctx, p) })
}

// Configure sets this node's identity. A new identity is a new node: it
// forgets the table and closes the links and UDP paths, which the other
// ends know by the old identity.
func (n *Node) Configure(c *config.Node) {
	defer n.Update()
	var info wire.NodeInfo
	var gen int64
	if c != nil {
		info = wire.NodeInfo{ID: c.ID, Name: c.Name, Software: n.cfg.Software}
		gen = c.Generation
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if info.ID != n.info.ID {
		for l := range n.links {
			l.Close(nil)
		}
		n.closeDirect(func(string) bool { return true })
		n.generation, n.lastSeq, n.net, n.removed = gen, 0, nil, nil
		n.subnetPrefix.Store(nil)
		if info.ID != "" && n.table.Owner() != info.ID {
			n.table.Reset(info.ID)
		}
		n.router.SetSelf(info.ID)
	}
	n.generation = max(n.generation, gen)
	n.info = info
	n.notify()
}

// Self returns this node's identity; its ID is empty until Configure gives it one.
func (n *Node) Self() wire.NodeInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.info
}

// Removed says who removed this node from the network, and when; nil
// while it is in the network.
func (n *Node) Removed() *wire.Removed {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.removed
}

// Update asks Run to look again at what this node publishes.
func (n *Node) Update() {
	select {
	case n.update <- struct{}{}:
	default:
	}
}

// Changed returns a channel that is closed at the next change of what this
// node knows of the network.
func (n *Node) Changed() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.changed
}

// notifyChanges wakes the watchers if what they see changed: the table,
// or the UDP paths; n.mu is held.
func (n *Node) notifyChanges() {
	var direct []string
	n.packets.Range(func(id, peer any) bool {
		if peer.(*packetPeer).direct.Load() != nil {
			direct = append(direct, id.(string))
		}
		return true
	})
	slices.Sort(direct)
	table, udp := n.table.Changed(), strings.Join(direct, " ")
	if table != n.seenTable || udp != n.seenDirect {
		n.seenTable, n.seenDirect = table, udp
		n.notify()
	}
}

// notify wakes the watchers; n.mu is held.
func (n *Node) notify() {
	close(n.changed)
	n.changed = make(chan struct{})
}

// Run keeps the table on disk, publishes this node's entry when it
// changes and every refresh, and acts on what the table says, until ctx is
// done.
func (n *Node) Run(ctx context.Context) {
	saved := make(chan struct{})
	go func() {
		defer close(saved)
		n.table.Run(ctx)
	}()
	defer func() { <-saved }()
	go n.watchSleep(ctx)
	tick := time.NewTicker(refresh)
	defer tick.Stop()
	force := false
	for {
		changed := n.table.Changed()
		n.reconcile(force)
		force = false
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-n.update:
		case <-tick.C:
			force = true
		}
	}
}

// reconcile publishes this node's entry if it changed, or always with
// force, and acts on the table: removals, and the routes.
func (n *Node) reconcile(force bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	defer n.notifyChanges()
	self := n.info
	if self.ID == "" {
		return
	}
	if r, ok := n.table.Get(state.RemovedKey(self.ID)); ok && n.standsRemoved(r, false) {
		if n.removed == nil {
			n.becomeRemoved(r)
		}
		n.setRoutes(nil)
		return
	}
	n.removed = nil
	cur, ok := n.table.Get(state.NodeKey(self.ID))
	if ok && cur.Version[0] > n.generation {
		// An earlier run of this node published it.
		n.raiseGeneration(cur.Version[0])
	}
	value, _ := json.Marshal(wire.NodeValue{Name: self.Name, Software: self.Software, Links: n.nodeLinks(), Net: n.net})
	if force || !ok || cur.Version[0] != n.generation || string(cur.Value) != string(value) {
		seq := max(n.lastSeq+1, time.Now().UnixNano())
		if ok {
			seq = max(seq, cur.Version[1]+1)
		}
		n.lastSeq = seq
		n.flood.Put(wire.Entry{Key: state.NodeKey(self.ID), Version: []int64{n.generation, seq}, Value: value})
	}
	for l := range n.links {
		if r, ok := n.removal(l.Peer.ID, l.Membership.Generation); ok {
			l.Close(&wire.Bye{Error: wire.Errorf(wire.CodeRemoved, "%s was removed from the network", l.Peer.Name), Membership: &wire.Membership{Removed: &r}})
		}
	}
	entries := n.table.Entries()
	n.promoteSubnet(entries)
	n.closeDirect(n.isRemoved)
	n.setRoutes(entries)
}

// promoteSubnet makes gst's choice of a subnet, made while this node was
// alone, the choice of a network once the table knows another node. A
// node alone since long ago thus does not move the network that adds it:
// its choice loses to every network's. Sync sends the subnet entry first,
// so a node that joins a network learns its choice before it sees the
// other nodes. n.mu is held.
func (n *Node) promoteSubnet(entries []wire.Entry) {
	e, ok := n.table.Get(state.SubnetKey)
	if !ok || e.Version[0] >= 0 {
		return
	}
	for _, o := range entries {
		if kind, id, _ := state.ParseKey(o.Key); kind == state.KindNode && id != n.info.ID && !n.isRemoved(id) {
			n.flood.Put(wire.Entry{Key: e.Key, Version: []int64{0, e.Version[1]}, Value: e.Value})
			return
		}
	}
}

// removal returns the removal record that covers node id, whose
// generation is at least gen, if there is one.
func (n *Node) removal(id string, gen int64) (wire.Entry, bool) {
	if e, ok := n.table.Get(state.NodeKey(id)); ok {
		gen = max(gen, e.Version[0])
	}
	r, ok := n.table.Get(state.RemovedKey(id))
	return r, ok && r.Version[0] >= gen
}

// IsRemoved reports whether node id is removed from the network.
func (n *Node) IsRemoved(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.isRemoved(id)
}

// isRemoved reports whether node id is removed from the network.
func (n *Node) isRemoved(id string) bool {
	_, ok := n.removal(id, 0)
	return ok
}

// raiseGeneration makes this node's generation gen; n.mu is held.
func (n *Node) raiseGeneration(gen int64) {
	if gen <= n.generation {
		return
	}
	n.generation = gen
	if save := n.cfg.SaveGeneration; save != nil {
		// Not under n.mu: saving may wait for the configuration file.
		go func() {
			if err := save(gen); err != nil {
				n.log.Printf("save the generation: %v", err)
			}
		}()
	}
	n.Update()
}

// becomeRemoved takes this node out of the network after removal record
// r: it ends its links and forgets the other nodes. n.mu is held.
func (n *Node) becomeRemoved(r wire.Entry) {
	var v wire.RemovedValue
	json.Unmarshal(r.Value, &v)
	n.removed = &wire.Removed{By: wire.NodeInfo{ID: v.By, Name: v.ByName}, At: v.At}
	if v.By == n.info.ID {
		n.log.Print("this machine left the network")
	} else {
		n.log.Printf("this machine was removed from the network by %s", cmp.Or(v.ByName, v.By[:8]))
	}
	for l := range n.links {
		l.Close(&wire.Bye{Error: wire.Errorf(wire.CodeRemoved, "%s left the network", n.info.Name), Membership: &wire.Membership{Removed: &r}})
	}
	n.closeDirect(func(string) bool { return true })
	n.table.Reset(n.info.ID, r)
}

// nodeLinks describes the running links; n.mu is held.
func (n *Node) nodeLinks() []wire.NodeLink {
	list := []wire.NodeLink{}
	for l := range n.links {
		list = append(list, wire.NodeLink{Peer: l.Peer.ID, Opened: l.Opened})
	}
	slices.SortFunc(list, func(a, b wire.NodeLink) int {
		return cmp.Or(cmp.Compare(a.Peer, b.Peer), boolOrder(a.Opened, b.Opened))
	})
	return list // run keeps one link each way
}

// SetNet sets this node's part in the virtual network; nil takes it out.
func (n *Node) SetNet(info *wire.NodeNet) {
	n.mu.Lock()
	same := info == nil && n.net == nil || info != nil && n.net != nil && *info == *n.net
	n.net = info
	n.mu.Unlock()
	// The virtual network sets it after every change it hears of: a
	// change of nothing must not make another.
	if !same {
		n.Update()
	}
}

// Network returns the subnet of the virtual network, as the table has it.
func (n *Node) Subnet() (wire.SubnetValue, bool) {
	e, ok := n.table.Get(state.SubnetKey)
	if !ok {
		return wire.SubnetValue{}, false
	}
	var v wire.SubnetValue
	return v, json.Unmarshal(e.Value, &v) == nil
}

// SetSubnet makes subnet the subnet of the whole network: as a person's
// choice, which beats every earlier one and gst's, or as gst's, which
// counts only until a network with an earlier one joins.
func (n *Node) SetSubnet(subnet string, manual bool) error {
	if n.Removed() != nil {
		return wire.Errorf(wire.CodeRemoved, "this machine is not in a network")
	}
	value, _ := json.Marshal(wire.SubnetValue{Subnet: subnet})
	now := time.Now().UnixNano()
	version := []int64{-1, -now} // promoteNetwork raises it in a network
	if manual {
		order := now
		if cur, ok := n.table.Get(state.SubnetKey); ok && cur.Version[0] == 1 {
			order = max(order, cur.Version[1]+1)
		}
		version = []int64{1, order}
	}
	e := wire.Entry{Key: state.SubnetKey, Version: version, Value: value}
	if err := state.Valid(e); err != nil {
		return wire.Errorf(wire.CodeInvalidParams, "invalid subnet %s", subnet)
	}
	n.flood.Put(e)
	return nil
}

// handshake is this node's side of a handshake; admit is for a link that a
// person added and that has not run yet.
func (n *Node) handshake(admit bool) link.Handshake {
	return link.Handshake{
		Self: n.Self(),
		Membership: func() *wire.Membership {
			n.mu.Lock()
			defer n.mu.Unlock()
			return &wire.Membership{Generation: n.generation, Admit: admit}
		},
		Check:  n.check,
		Timing: n.cfg.Timing,
	}
}

// check judges a node that links with this one.
func (n *Node) check(peer wire.NodeInfo, m wire.Membership) *link.Refusal {
	n.mu.Lock()
	defer n.mu.Unlock()
	if r, ok := n.table.Get(state.RemovedKey(peer.ID)); ok && r.Version[0] >= m.Generation {
		return &link.Refusal{Err: wire.Errorf(wire.CodeRemoved, "%s was removed from the network", peer.Name), Membership: &wire.Membership{Removed: &r}}
	}
	if r, ok := n.table.Get(state.RemovedKey(n.info.ID)); ok && n.standsRemoved(r, m.Admit) {
		return &link.Refusal{Err: wire.Errorf(wire.CodeRemoved, "%s was removed from the network", n.info.Name), Membership: &wire.Membership{Removed: &r}}
	}
	return nil
}

// standsRemoved judges this node by removal record r of itself: it stands
// removed, unless admit, a link that a person added, brings it back with
// a new generation. Every way back into the network goes through here.
// n.mu is held.
func (n *Node) standsRemoved(r wire.Entry, admit bool) bool {
	if r.Version[0] < n.generation {
		return false
	}
	if admit {
		n.raiseGeneration(r.Version[0] + 1)
		return false
	}
	return true
}

// RemoteError is a failure that the other node reported. Its quote of
// that node stays out of logs, which take Logged instead.
type RemoteError struct{ What, Quote string }

func (e *RemoteError) Error() string { return e.What + ": " + e.Quote }

// Logged is the error without what the other node said.
func (e *RemoteError) Logged() string { return e.What + "; see gst status" }

// RetryError is a link that failed in a way that a new try right away
// fixes, such as after a removal that the link's admit undoes.
type RetryError struct{ Err error }

func (e *RetryError) Error() string { return e.Err.Error() }
func (e *RetryError) Unwrap() error { return e.Err }

// Accept serves a link that another node opened on conn, whose Hello was
// theirs, until the link ends or ctx is done.
func (n *Node) Accept(ctx context.Context, conn io.ReadWriteCloser, theirs wire.Hello) {
	if n.Self().ID == "" {
		wire.Answer(conn, theirs, wire.Hello{Software: n.cfg.Software, Role: wire.RolePeer}, wire.Errorf(wire.CodeFailed, "gst on this machine has no identity yet"))
		conn.Close()
		return
	}
	l, err := n.handshake(false).Accept(conn, theirs)
	if err != nil {
		return
	}
	if err := n.run(ctx, l, nil); err != nil && ctx.Err() == nil {
		// The reason may quote the peer, so the log leaves it out.
		n.log.Print("incoming link ended")
	}
}

// Link runs a link over conn, which leads to the other node's daemon,
// until it ends or ctx is done, and closes conn. admit says that a person
// added the link and it has not run yet. onUp is called with the other
// node once the link runs. Link returns why the link ended, or nil when
// ctx is done. When conn has a Failure method, as an sshconn.Session does,
// what it returns explains a remote gst that did not answer.
func (n *Node) Link(ctx context.Context, admit bool, conn io.ReadWriteCloser, onUp func(peer wire.NodeInfo)) error {
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { conn.Close() })()
	if n.Self().ID == "" {
		return errors.New("gst on this machine has no identity yet")
	}
	if r, ok := n.table.Get(state.RemovedKey(n.Self().ID)); ok {
		n.mu.Lock()
		removed := n.standsRemoved(r, admit)
		n.mu.Unlock()
		if removed {
			return errors.New("this machine was removed from the network; `gst add` joins it again")
		}
	}
	l, err := n.handshake(admit).Dial(conn)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return n.refused(err, admit, conn)
	}
	return n.run(ctx, l, onUp)
}

// refused explains a handshake that failed, and acts on a removal record
// that came with it.
func (n *Node) refused(err error, admit bool, conn io.ReadWriteCloser) error {
	var r *link.RefusedError
	if !errors.As(err, &r) {
		if failed, ok := conn.(interface{ Failure() string }); ok && failed.Failure() != "" {
			return &RemoteError{"the remote gst failed", failed.Failure()}
		}
		return errors.New("the remote gst did not answer; is gst installed there, and the same version as here?")
	}
	if r.ByPeer {
		err = &RemoteError{"the remote gst refused the link", r.Err.Message}
	}
	if r.Membership != nil && r.Membership.Removed != nil {
		return n.learnRemoval(*r.Membership.Removed, admit, err)
	}
	return err
}

// learnRemoval takes in removal record e, which a link that failed with
// err brought, and returns the error to report.
func (n *Node) learnRemoval(e wire.Entry, admit bool, err error) error {
	_, id, _ := state.ParseKey(e.Key)
	if id == n.Self().ID && admit && state.Valid(e) == nil {
		// A person added this link after the removal: come back.
		n.mu.Lock()
		n.standsRemoved(e, true)
		n.mu.Unlock()
		n.flood.Put(e)
		n.Update()
		return &RetryError{errors.New("this machine was removed from the network; joining it again")}
	}
	n.flood.Put(e)
	n.Update()
	if id == n.Self().ID {
		return errors.New("this machine was removed from the network")
	}
	if admit {
		// The other node learns of its removal from this side, and
		// with the admit of the next try comes back.
		return &RetryError{err}
	}
	return err
}

// run carries the frames of l until it ends, or ctx is done.
func (n *Node) run(ctx context.Context, l *link.Link, onUp func(wire.NodeInfo)) error {
	peer := l.Peer.ID
	n.mu.Lock()
	// One link each way between two nodes: a new one replaces the
	// one before, which is likely dead without either side knowing yet.
	for old := range n.links {
		if old.Peer.ID == peer && old.Opened == l.Opened {
			old.Close(&wire.Bye{Error: wire.Errorf(wire.CodeFailed, "a newer link between %s and %s replaced it", n.info.Name, l.Peer.Name)})
			delete(n.links, old)
		}
	}
	n.links[l] = struct{}{}
	n.mu.Unlock()
	removeFlood := n.flood.Add(peer, func(body []byte) bool { return l.Send(link.Sync, body, true) })
	removeRoute := n.router.AddLink(peer, func(body []byte, urgent bool) bool { return l.Send(link.Msg, body, urgent) })
	n.Update()
	if onUp != nil {
		onUp(l.Peer)
	}
	stop := context.AfterFunc(ctx, func() {
		l.Close(&wire.Bye{Error: wire.Errorf(wire.CodeFailed, "gst stopped on %s", n.Self().Name)})
	})
	err := l.Run(func(t link.Type, body []byte) {
		switch t {
		case link.Sync:
			n.flood.Receive(peer, body)
		case link.Msg:
			n.router.Receive(body)
		}
	})
	stop()
	removeRoute()
	removeFlood()
	n.mu.Lock()
	delete(n.links, l)
	n.mu.Unlock()
	n.Update()
	if ctx.Err() != nil {
		return nil
	}
	var bye *link.ByeError
	if !errors.As(err, &bye) {
		return err
	}
	if bye.Bye.Error != nil {
		err = &RemoteError{"the other machine ended the link", bye.Bye.Error.Message}
	}
	if bye.Bye.Membership != nil && bye.Bye.Membership.Removed != nil {
		// The handshake took any admit: a removal now is a new one.
		return n.learnRemoval(*bye.Bye.Membership.Removed, false, err)
	}
	return err
}

// boolOrder sorts true before false.
func boolOrder(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1
	}
	return 1
}
