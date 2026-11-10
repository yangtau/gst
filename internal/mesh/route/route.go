// Package route sends messages to any node of the mesh, along the links.
// A message names the node it is for and the node it is from; each node
// that it reaches hands it to the next by its own routing table, which the
// node works out from the links in the table of the network
// (internal/mesh/state). A message is a request, the response to one, or a
// packet of the virtual network. A call is a request and its response;
// when a node on the way has no next node, it answers a request itself
// with unreachable, so that the caller learns at once.
package route

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Kind is the kind of a message.
type Kind byte

// Kinds of messages.
const (
	Request  Kind = 1 // payload: wire.Request
	Response Kind = 2 // payload: wire.Response
	Packet   Kind = 3 // payload: a packet of the virtual network
)

const (
	// HeaderSize is the size of a message's header: to (16 bytes), from
	// (16), ttl (1), kind (1), and id (4, big-endian).
	HeaderSize = 38
	// InitialTTL is how many nodes a message may pass.
	InitialTTL = 16
	// DefaultTimeout bounds a call whose context has no deadline.
	DefaultTimeout = 10 * time.Second
	handlerTimeout = 30 * time.Second
)

// Sender sends the body of a msg frame over a link, urgently or not, and
// reports whether it did.
type Sender func(body []byte, urgent bool) bool

type link struct{ send Sender }

type handler func(ctx context.Context, from string, params json.RawMessage) (any, error)

// Router is a node's part in routing.
type Router struct {
	mu      sync.RWMutex
	self    string
	next    map[string]string  // the neighbour on the way to a node, by node id
	links   map[string][]*link // by neighbour
	pending map[uint32]chan wire.Response
	methods map[string]handler

	ids      atomic.Uint32
	onPacket atomic.Pointer[func(from string, packet []byte)]
}

// New returns a router without an identity, which sends nothing until
// SetSelf gives it one.
func New() *Router {
	return &Router{next: map[string]string{}, links: map[string][]*link{}, pending: map[uint32]chan wire.Response{}, methods: map[string]handler{}}
}

// SetSelf sets the id of this node.
func (r *Router) SetSelf(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.self = id
}

// SetRoutes replaces the routing table: next gives, for each node, the
// neighbour on the way to it.
func (r *Router) SetRoutes(next map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next = next
}

// AddLink adds a link to the neighbour peer; it returns the function that
// removes it.
func (r *Router) AddLink(peer string, send Sender) (remove func()) {
	l := &link{send}
	r.mu.Lock()
	r.links[peer] = append(r.links[peer], l)
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		list := r.links[peer]
		for i, o := range list {
			if o == l {
				list = append(list[:i:i], list[i+1:]...)
				break
			}
		}
		if len(list) == 0 {
			delete(r.links, peer)
		} else {
			r.links[peer] = list
		}
	}
}

// Reaches reports whether the routing table leads to node id.
func (r *Router) Reaches(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.next[id]
	return ok || r.links[id] != nil
}

// HandlePackets makes handle receive the packets for this node, with the
// node each came from; nil drops them. handle must not keep the packet or
// block.
func (r *Router) HandlePackets(handle func(from string, packet []byte)) {
	if handle == nil {
		r.onPacket.Store(nil)
		return
	}
	r.onPacket.Store(&handle)
}

// Handle makes fn answer the calls of method. Missing params leave p zero.
func Handle[P any](r *Router, method string, fn func(ctx context.Context, from string, p P) (any, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.methods[method] = func(ctx context.Context, from string, params json.RawMessage) (any, error) {
		var p P
		if len(params) > 0 && json.Unmarshal(params, &p) != nil {
			return nil, wire.Errorf(wire.CodeInvalidParams, "invalid params")
		}
		return fn(ctx, from, p)
	}
}

func encode(to, from string, ttl byte, kind Kind, id uint32, payload []byte) ([]byte, bool) {
	msg := make([]byte, HeaderSize+len(payload))
	if n, err := hex.Decode(msg[0:16], []byte(to)); err != nil || n != 16 {
		return nil, false
	}
	if n, err := hex.Decode(msg[16:32], []byte(from)); err != nil || n != 16 {
		return nil, false
	}
	msg[32], msg[33] = ttl, byte(kind)
	binary.BigEndian.PutUint32(msg[34:38], id)
	copy(msg[HeaderSize:], payload)
	return msg, true
}

// send hands msg to the neighbour on the way to node to, and reports
// whether one took it.
func (r *Router) send(to string, msg []byte, urgent bool) bool {
	r.mu.RLock()
	if to == r.self {
		r.mu.RUnlock()
		go r.Receive(msg) // Such as unreachable, from this node to itself.
		return true
	}
	hop, ok := r.next[to]
	if !ok {
		hop = to // A neighbour, before the table says so.
	}
	list := r.links[hop]
	r.mu.RUnlock()
	// The newest link first: an older one to the same node may be one
	// that is gone, and that its end has not noticed yet.
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].send(msg, urgent) {
			return true
		}
	}
	return false
}

// SendPacket sends a packet of the virtual network to node to, and reports
// whether a link took it.
func (r *Router) SendPacket(to string, packet []byte) bool {
	r.mu.RLock()
	self := r.self
	r.mu.RUnlock()
	msg, ok := encode(to, self, InitialTTL, Packet, 0, packet)
	return ok && r.send(to, msg, false)
}

// Receive handles the body of a msg frame from a link, which it may keep.
func (r *Router) Receive(msg []byte) {
	if len(msg) < HeaderSize {
		return
	}
	to, from := hex.EncodeToString(msg[0:16]), hex.EncodeToString(msg[16:32])
	ttl, kind, id := msg[32], Kind(msg[33]), binary.BigEndian.Uint32(msg[34:38])
	payload := msg[HeaderSize:]
	r.mu.RLock()
	self := r.self
	r.mu.RUnlock()
	if self == "" {
		return
	}
	if to == self {
		r.deliver(from, kind, id, payload)
		return
	}
	if ttl <= 1 {
		if kind == Request {
			r.reply(from, id, nil, wire.Errorf(wire.CodeUnreachable, "the message took too many machines"))
		}
		return
	}
	msg[32] = ttl - 1
	if !r.send(to, msg, kind != Packet) && kind == Request {
		r.reply(from, id, nil, wire.Errorf(wire.CodeUnreachable, "no way to the machine from here"))
	}
}

func (r *Router) deliver(from string, kind Kind, id uint32, payload []byte) {
	switch kind {
	case Packet:
		if handle := r.onPacket.Load(); handle != nil {
			(*handle)(from, payload)
		}
	case Response:
		var resp wire.Response
		if json.Unmarshal(payload, &resp) != nil {
			return
		}
		r.mu.Lock()
		ch := r.pending[id]
		delete(r.pending, id)
		r.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	case Request:
		var req wire.Request
		if json.Unmarshal(payload, &req) != nil {
			r.reply(from, id, nil, wire.Errorf(wire.CodeInvalidParams, "invalid request"))
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
			defer cancel()
			result, err := r.serve(ctx, from, req)
			r.reply(from, id, result, err)
		}()
	}
}

func (r *Router) serve(ctx context.Context, from string, req wire.Request) (any, error) {
	r.mu.RLock()
	h := r.methods[req.Method]
	r.mu.RUnlock()
	if h == nil {
		return nil, wire.Errorf(wire.CodeUnsupportedMethod, "method %q is not supported", req.Method)
	}
	return h(ctx, from, req.Params)
}

// reply sends the response to request id from node to.
func (r *Router) reply(to string, id uint32, result any, err error) {
	resp := wire.Response{}
	if err != nil {
		resp.Error = wire.AsError(err)
	} else if result != nil {
		raw, encErr := json.Marshal(result)
		if encErr != nil {
			resp.Error = wire.Errorf(wire.CodeFailed, "result encoding failed")
		} else {
			resp.Result = raw
		}
	}
	payload, _ := json.Marshal(resp)
	r.mu.RLock()
	self := r.self
	r.mu.RUnlock()
	if msg, ok := encode(to, self, InitialTTL, Response, id, payload); ok {
		r.send(to, msg, true)
	}
}

// Call calls method on node to and decodes the result into result, which
// may be nil. ctx bounds the call, DefaultTimeout when it has no
// deadline. Errors are *wire.Error: the callee's, unreachable when no node
// on the way could pass the request on, or failed when no answer came in
// time. Calls are not retried.
func (r *Router) Call(ctx context.Context, to, method string, params, result any) error {
	req := wire.Request{Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = raw
	}
	r.mu.RLock()
	self := r.self
	r.mu.RUnlock()
	if self == "" {
		return wire.Errorf(wire.CodeFailed, "gst on this machine has no identity yet")
	}
	var resp wire.Response
	if to == self {
		out, err := r.serve(ctx, self, req)
		if err != nil {
			return wire.AsError(err)
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return err
		}
		resp.Result = raw
	} else {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
			defer cancel()
		}
		payload, _ := json.Marshal(req)
		id := r.ids.Add(1)
		msg, ok := encode(to, self, InitialTTL, Request, id, payload)
		if !ok {
			return wire.Errorf(wire.CodeInvalidParams, "invalid node")
		}
		ch := make(chan wire.Response, 1)
		r.mu.Lock()
		r.pending[id] = ch
		r.mu.Unlock()
		defer func() {
			r.mu.Lock()
			delete(r.pending, id)
			r.mu.Unlock()
		}()
		if !r.send(to, msg, true) {
			return wire.Errorf(wire.CodeUnreachable, "no way to the machine: no link leads there now")
		}
		select {
		case resp = <-ch:
		case <-ctx.Done():
			return wire.Errorf(wire.CodeFailed, "the machine did not answer in time")
		}
	}
	if resp.Error != nil {
		return resp.Error
	}
	if result == nil || resp.Result == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		return errors.New("invalid result of " + method)
	}
	return nil
}
