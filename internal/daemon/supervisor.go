package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/mesh"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// maxRetry bounds the wait before a link that keeps failing retries.
const maxRetry = 5 * time.Minute

// supervisor runs one link and reconnects it after every failure until it
// is stopped, waiting longer after each failure in a row.
type supervisor struct {
	d       *Daemon
	id      string      // the link's id, which the log names it by
	spec    config.Link // the configuration it was started with
	ctx     context.Context
	cancel  context.CancelFunc
	stopped chan struct{}

	mu         sync.Mutex
	current    string
	since      time.Time
	lastErr    string
	reconnects int
	peer       *wire.NodeInfo
}

func newSupervisor(d *Daemon, spec config.Link) *supervisor {
	ctx, cancel := context.WithCancel(context.Background())
	return &supervisor{
		d:       d,
		id:      config.LinkID(spec.Destination),
		spec:    spec,
		ctx:     ctx,
		cancel:  cancel,
		stopped: make(chan struct{}),
		current: wire.StateConnecting,
		since:   time.Now(),
	}
}

// logf writes to the daemon's log about this link.
func (s *supervisor) logf(format string, args ...any) {
	s.d.cfg.Log.Printf("link %s: "+format, append([]any{s.id}, args...)...)
}

func (s *supervisor) set(state, lastErr string, peer *wire.NodeInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != state {
		s.current, s.since = state, time.Now()
	}
	if lastErr != "" {
		s.lastErr = lastErr
	}
	s.peer = peer
}

func (s *supervisor) state() wire.LinkState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return wire.LinkState{Destination: s.spec.Destination, State: s.current, Since: s.since, Error: s.lastErr, Reconnects: s.reconnects, Peer: s.peer}
}

// run connects until stop. The log leaves out what the other machine
// reported, which may quote it; Status shows it.
func (s *supervisor) run() {
	defer close(s.stopped)
	wait := s.d.cfg.RetryDelay
	for {
		s.set(wire.StateConnecting, "", nil)
		err := s.connect(func(peer wire.NodeInfo) {
			s.set(wire.StateRunning, "", &peer)
			s.logf("up")
			wait = s.d.cfg.RetryDelay
		})
		if s.ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("the link ended")
		}
		var remote *mesh.RemoteError
		if errors.As(err, &remote) {
			s.logf("the remote gst failed; see gst status")
		} else {
			s.logf("%v", err)
		}
		s.set(wire.StateDown, err.Error(), nil)
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, maxRetry)
		s.mu.Lock()
		s.reconnects++
		s.mu.Unlock()
	}
}

func (s *supervisor) connect(onUp func(wire.NodeInfo)) error {
	conn, err := s.d.cfg.Connect(s.ctx, &s.spec)
	if err != nil {
		return err
	}
	return s.d.mesh.Link(s.ctx, s.id, conn, onUp)
}

// stop ends the link and waits until it has shut down.
func (s *supervisor) stop() {
	s.cancel()
	<-s.stopped
	s.logf("stopped")
}
