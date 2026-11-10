// Package expose forwards ports bound to this machine's gst address.
package expose

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

const dialTimeout = 5 * time.Second

// Manager owns the listeners. Apply follows the network lifecycle; an empty
// address stops all traffic while retaining the configured rules in Status.
// Its zero value is ready to use.
type Manager struct {
	mu      sync.Mutex
	address string
	rules   []config.Expose
	running map[string]*listener // by rule key
	states  []wire.ExposeState   // of rules, in their order
}

type listener struct {
	rule config.Expose
	stop context.CancelFunc
	done chan struct{}
	err  error // written before done closes
}

func (m *Manager) Apply(ctx context.Context, address string, rules []config.Expose) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.apply(ctx, address, rules)
}

// Close stops all traffic, as Apply does without an address.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.apply(context.Background(), "", m.rules)
}

func (m *Manager) apply(ctx context.Context, address string, rules []config.Expose) {
	if m.running == nil {
		m.running = map[string]*listener{}
	}
	for key, l := range m.running {
		if address != m.address || !slices.Contains(rules, l.rule) {
			l.stop()
			<-l.done
			delete(m.running, key)
		}
	}
	m.address, m.rules = address, rules
	m.states = []wire.ExposeState{}
	for _, rule := range rules {
		state := wire.ExposeState{Protocol: rule.Protocol, Port: rule.Port, Target: rule.Target, State: wire.StateWaiting}
		if address != "" {
			state.Address = net.JoinHostPort(address, strconv.Itoa(rule.Port))
			l := m.running[rule.Key()]
			if l != nil {
				select {
				case <-l.done:
					delete(m.running, rule.Key())
					l = nil
				default:
				}
			}
			var err error
			if l == nil {
				l, err = start(ctx, address, rule)
			}
			if err != nil {
				state.State, state.Error = wire.StateFailed, err.Error()
			} else {
				m.running[rule.Key()] = l
				state.State = wire.StateRunning
			}
		}
		m.states = append(m.states, state)
	}
}

func (m *Manager) Status() []wire.ExposeState {
	m.mu.Lock()
	defer m.mu.Unlock()
	states := slices.Clone(m.states)
	for i, rule := range m.rules {
		if l := m.running[rule.Key()]; l != nil {
			select {
			case <-l.done:
				s := &states[i]
				s.State = wire.StateFailed
				if l.err != nil {
					s.Error = l.err.Error()
				}
			default:
			}
		}
	}
	return states
}

func start(parent context.Context, address string, rule config.Expose) (*listener, error) {
	ip, err := netip.ParseAddr(address)
	if err != nil || !ip.Is4() || ip.IsUnspecified() {
		return nil, fmt.Errorf("no IPv4 gst address")
	}
	ctx, stop := context.WithCancel(parent)
	l := &listener{rule: rule, stop: stop, done: make(chan struct{})}
	addr := netip.AddrPortFrom(ip, uint16(rule.Port))
	var serve func() error
	var closeSocket func() error
	if rule.Protocol == "udp" {
		c, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(addr))
		if err != nil {
			stop()
			return nil, err
		}
		closeSocket = c.Close
		serve = func() error { return serveUDP(ctx, c, rule) }
	} else {
		ln, err := net.Listen("tcp4", addr.String())
		if err != nil {
			stop()
			return nil, err
		}
		closeSocket = ln.Close
		serve = func() error { return serveTCP(ctx, ln, rule) }
	}
	go func() {
		defer close(l.done)
		defer closeSocket()
		defer stop()
		context.AfterFunc(ctx, func() { closeSocket() }) // to end serve
		l.err = serve()
	}()
	return l, nil
}

func serveTCP(ctx context.Context, ln net.Listener, rule config.Expose) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	network, target := rule.Destination()
	for {
		client, err := ln.Accept()
		if err != nil {
			return err
		}
		wg.Go(func() {
			defer client.Close()
			defer context.AfterFunc(ctx, func() { client.Close() })()
			d := net.Dialer{Timeout: dialTimeout}
			upstream, err := d.DialContext(ctx, network, target)
			if err != nil {
				return
			}
			defer context.AfterFunc(ctx, func() { upstream.Close() })()
			pipe(client, upstream)
		})
	}
}
