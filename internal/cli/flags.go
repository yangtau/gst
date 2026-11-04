package cli

import (
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

// settingsFlags are the link settings that override the ssh configuration.
type settingsFlags struct {
	identity          string
	knownHosts        string
	connectTimeout    time.Duration
	keepaliveInterval time.Duration
	keepaliveCount    int64
	remoteGst         string
}

func (s *settingsFlags) register(fs *pflag.FlagSet, clearable bool) {
	var emptyClears, zeroClears string
	if clearable {
		emptyClears = "; '' goes back to the default"
		zeroClears = "; 0 goes back to the default"
	}
	fs.StringVar(&s.identity, "identity", "", "private key file without a passphrase (default: IdentityFile from ssh config)"+emptyClears)
	fs.StringVar(&s.knownHosts, "known-hosts", "", "known_hosts file (default: UserKnownHostsFile and GlobalKnownHostsFile from ssh config)"+emptyClears)
	fs.DurationVar(&s.connectTimeout, "connect-timeout", 0, "timeout for connecting and logging in, in whole seconds (default: ConnectTimeout from ssh config, else 15s)"+zeroClears)
	fs.DurationVar(&s.keepaliveInterval, "keepalive-interval", 0, "time between keepalive probes, in whole seconds (default: ServerAliveInterval from ssh config, else 15s)"+zeroClears)
	fs.Int64Var(&s.keepaliveCount, "keepalive-count", 0, "give up after a probe stays unanswered for this many intervals (default: ServerAliveCountMax from ssh config, else 3)"+zeroClears)
	fs.StringVar(&s.remoteGst, "remote-gst", "", "gst command on the server, when gst is not in the PATH of its login shell (default: gst)"+emptyClears)
}

// apply sets the flags given on the command line and reports whether any were.
func (s *settingsFlags) apply(cmd *cobra.Command, t *config.Link) (bool, error) {
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
	if flags.Changed("remote-gst") {
		changed = true
		t.RemoteGst = s.remoteGst
	}
	return changed, nil
}
