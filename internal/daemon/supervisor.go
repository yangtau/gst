package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/logfile"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// supervisor runs one tunnel and reconnects it after every failure until
// it is stopped.
type supervisor struct {
	d       *Daemon
	name    string
	spec    config.Tunnel // the configuration it was started with
	logOut  *logfile.File // nil when the daemon keeps no tunnel logs
	log     *log.Logger
	cancel  context.CancelFunc
	ctx     context.Context
	stopped chan struct{}

	mu         sync.Mutex
	current    string
	since      time.Time
	lastErr    string
	reconnects int
}

func newSupervisor(d *Daemon, name string, spec config.Tunnel, logOut *logfile.File) *supervisor {
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

func (s *supervisor) set(state, lastErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != state {
		s.current, s.since = state, time.Now()
	}
	if lastErr != "" {
		s.lastErr = lastErr
	}
}

func (s *supervisor) state() wire.TunnelState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return wire.TunnelState{Name: s.name, State: s.current, Since: s.since, Error: s.lastErr, Reconnects: s.reconnects}
}

// run connects until stop. Errors are logged; like the tunnel package's,
// they omit addresses, users, and paths.
func (s *supervisor) run() {
	defer close(s.stopped)
	for {
		s.set(wire.StateConnecting, "")
		err := s.connect()
		if s.ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("tunnel ended unexpectedly")
		}
		s.log.Print(err)
		s.set(wire.StateDown, err.Error())
		timer := time.NewTimer(s.d.cfg.RetryDelay)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.mu.Lock()
		s.reconnects++
		s.mu.Unlock()
	}
}

func (s *supervisor) connect() error {
	opts, err := s.d.cfg.Resolve(s.ctx, &s.spec)
	if err != nil {
		// Resolution errors quote the ssh configuration.
		return errors.New("ssh config resolution failed")
	}
	opts.Logger = s.log
	opts.OnReady = func() { s.set(wire.StateRunning, "") }
	if s.spec.Mesh {
		opts.Attach = func(ctx context.Context, client *ssh.Client) {
			s.d.mesh.Link(ctx, s.name, client, s.spec.RemoteGst, s.log)
		}
	}
	return s.d.cfg.Run(s.ctx, opts)
}

// stop ends the tunnel and waits until it has shut down.
func (s *supervisor) stop() {
	s.cancel()
	<-s.stopped
	s.log.Print("tunnel stopped")
	s.d.cfg.Log.Printf("tunnel %s stopped", s.name)
	if s.logOut != nil {
		s.logOut.Close()
	}
}
