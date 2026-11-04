package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/logfile"
	"github.com/yangtau/ssh-tunnel/internal/mesh"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// maxRetry bounds the wait before a link that keeps failing retries.
const maxRetry = 5 * time.Minute

// supervisor runs one link and reconnects it after every failure until it
// is stopped, waiting longer after each failure in a row.
type supervisor struct {
	d       *Daemon
	name    string
	spec    config.Link   // the configuration it was started with
	logOut  *logfile.File // nil when the daemon keeps no link logs
	log     *log.Logger
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

func newSupervisor(d *Daemon, name string, spec config.Link, logOut *logfile.File) *supervisor {
	ctx, cancel := context.WithCancel(context.Background())
	var w io.Writer = io.Discard
	if logOut != nil {
		w = logOut
	}
	return &supervisor{
		d:       d,
		name:    name,
		spec:    spec,
		logOut:  logOut,
		log:     log.New(w, "", log.LstdFlags),
		ctx:     ctx,
		cancel:  cancel,
		stopped: make(chan struct{}),
		current: wire.StateConnecting,
		since:   time.Now(),
	}
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
	return wire.LinkState{Name: s.name, Destination: s.spec.Destination, State: s.current, Since: s.since, Error: s.lastErr, Reconnects: s.reconnects, Peer: s.peer}
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
			s.log.Print("link up")
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
			s.log.Print("the remote gst failed; see gst link list")
		} else {
			s.log.Print(err)
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
	conn, err := s.d.cfg.Connect(s.ctx, s.name, &s.spec)
	if err != nil {
		return err
	}
	return s.d.mesh.Link(s.ctx, s.name, conn, onUp)
}

// stop ends the link and waits until it has shut down.
func (s *supervisor) stop() {
	s.cancel()
	<-s.stopped
	s.log.Print("link stopped")
	s.d.cfg.Log.Printf("link %s stopped", s.name)
	if s.logOut != nil {
		s.logOut.Close()
	}
}
