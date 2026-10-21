package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

// settingsFlags are the tunnel settings that override the ssh configuration.
type settingsFlags struct {
	identity          string
	knownHosts        string
	connectTimeout    time.Duration
	keepaliveInterval time.Duration
	keepaliveCount    int64
	cleanup           bool
}

func (s *settingsFlags) register(fs *pflag.FlagSet, clearable bool) {
	var emptyClears, zeroClears, offSuffix string
	if clearable {
		emptyClears = "; '' goes back to the default"
		zeroClears = "; 0 goes back to the default"
		offSuffix = "; turn off with --cleanup-listeners=false"
	}
	fs.StringVar(&s.identity, "identity", "", "private key file without a passphrase (default: IdentityFile from ssh config)"+emptyClears)
	fs.StringVar(&s.knownHosts, "known-hosts", "", "known_hosts file (default: UserKnownHostsFile and GlobalKnownHostsFile from ssh config)"+emptyClears)
	fs.DurationVar(&s.connectTimeout, "connect-timeout", 0, "timeout for connecting and logging in, in whole seconds (default: ConnectTimeout from ssh config, else 15s)"+zeroClears)
	fs.DurationVar(&s.keepaliveInterval, "keepalive-interval", 0, "time between keepalive probes, in whole seconds (default: ServerAliveInterval from ssh config, else 15s)"+zeroClears)
	fs.Int64Var(&s.keepaliveCount, "keepalive-count", 0, "give up after a probe stays unanswered for this many intervals (default: ServerAliveCountMax from ssh config, else 3)"+zeroClears)
	fs.BoolVar(&s.cleanup, "cleanup-listeners", false, "before listening, kill the login user's processes that listen on the server ports and sockets of -R forwards, and remove the login user's socket files there that nothing listens on; only for listeners dedicated to this tunnel"+offSuffix)
}

// apply sets the flags given on the command line and reports whether any were.
func (s *settingsFlags) apply(cmd *cobra.Command, t *config.Tunnel) (bool, error) {
	changed := false
	flags := cmd.Flags()
	path := func(name, value string, field *string) error {
		if !flags.Changed(name) {
			return nil
		}
		changed = true
		if value == "" {
			*field = ""
			return nil
		}
		abs, err := config.AbsPath(value)
		if err != nil {
			return usage("--%s: %v", name, err)
		}
		*field = abs
		return nil
	}
	duration := func(name string, value time.Duration, field *int64) error {
		if !flags.Changed(name) {
			return nil
		}
		changed = true
		if value < 0 || value%time.Second != 0 || int64(value/time.Second) > config.MaxSeconds {
			return usage("--%s must be a whole number of seconds, such as 15s", name)
		}
		*field = int64(value / time.Second)
		return nil
	}
	if err := path("identity", s.identity, &t.IdentityFile); err != nil {
		return false, err
	}
	if err := path("known-hosts", s.knownHosts, &t.KnownHostsFile); err != nil {
		return false, err
	}
	if err := duration("connect-timeout", s.connectTimeout, &t.ConnectTimeoutSeconds); err != nil {
		return false, err
	}
	if err := duration("keepalive-interval", s.keepaliveInterval, &t.KeepaliveIntervalSeconds); err != nil {
		return false, err
	}
	if flags.Changed("keepalive-count") {
		changed = true
		if s.keepaliveCount < 0 || s.keepaliveCount > config.MaxKeepaliveCount {
			return false, usage("--keepalive-count must be between 1 and %d, or 0 for the default", config.MaxKeepaliveCount)
		}
		t.KeepaliveCountMax = s.keepaliveCount
	}
	if flags.Changed("cleanup-listeners") {
		changed = true
		t.CleanupListeners = s.cleanup
	}
	return changed, nil
}

// forwardFlags collects -R, -L, and -D values in command-line order.
type forwardFlags struct {
	specs []forwardSpec
}

type forwardSpec struct {
	direction, value string
}

func (f *forwardFlags) register(fs *pflag.FlagSet, listenerOnly bool) {
	remote, local := "[bind_address:]port:host:hostport", "[bind_address:]port:host:hostport"
	if listenerOnly {
		remote, local = "[bind_address:]port | socket", "[bind_address:]port | socket"
	}
	fs.VarP(&specValue{f, config.Remote}, "remote", "R", remote+"  listen on the server, connect from this machine (like ssh -R); repeatable")
	fs.VarP(&specValue{f, config.Local}, "local", "L", local+"  listen on this machine, connect from the server (like ssh -L); repeatable")
	fs.VarP(&specValue{f, config.Dynamic}, "dynamic", "D", "[bind_address:]port  listen on this machine as a SOCKS5 proxy, connect from the server to each requested address (like ssh -D); repeatable")
}

// parse returns the forwards; listenerOnly accepts "[bind_address:]port".
func (f *forwardFlags) parse(listenerOnly bool) ([]config.Forward, error) {
	forwards := make([]config.Forward, 0, len(f.specs))
	for _, spec := range f.specs {
		parse := config.ParseForward
		if listenerOnly {
			parse = config.ParseListener
		}
		fwd, err := parse(spec.direction, spec.value)
		if err != nil {
			return nil, &usageError{err}
		}
		forwards = append(forwards, fwd)
	}
	return forwards, nil
}

type specValue struct {
	flags     *forwardFlags
	direction string
}

func (v *specValue) String() string { return "" }
func (v *specValue) Type() string   { return "spec" }
func (v *specValue) Set(value string) error {
	v.flags.specs = append(v.flags.specs, forwardSpec{v.direction, value})
	return nil
}

// addForward adds fwd to t unless t already has it. A different forward on
// the same listener is a conflict.
func addForward(name string, t *config.Tunnel, fwd config.Forward) (bool, error) {
	for _, existing := range t.Forwards {
		if !existing.SameListener(fwd) {
			continue
		}
		if existing.Direction == fwd.Direction && existing.Target == fwd.Target {
			return false, nil
		}
		return false, withHint(fmt.Errorf("tunnel %s already has %s on that listener", name, existing.Spec()),
			fmt.Sprintf("remove it first with `gst forward remove %s %s %s`", name, existing.Flag(), existing.Listen))
	}
	t.Forwards = append(t.Forwards, fwd)
	return true, nil
}
