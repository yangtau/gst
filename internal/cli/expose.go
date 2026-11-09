package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func (a *app) exposeCommand() *cobra.Command {
	var port int
	var udp bool
	cmd := &cobra.Command{
		Use:   "expose [target]",
		Short: "Expose a port of this machine on its gst IP, or list them",
		Long: `Without a target, list the ports exposed on this machine's gst IP.

With a target, forward a port on this machine's gst IP to it, so that every
machine of the network can reach it. A bare port means that port on
127.0.0.1; other targets are tcp://host:port, udp://host:port, or
unix:///absolute/path to a Unix stream socket. --port sets the port on the
gst IP; it defaults to the target's port, and Unix sockets need it.

The rule stays until gst unexpose, across restarts. It listens only on the
gst IP. The target sees a connection from gst, not from the client; bytes
pass unchanged, with no TLS or HTTP processing. Repeating a rule changes
nothing; to change its target, unexpose it first.`,
		Example: "  gst expose\n  gst expose 8080 --port 5999\n  gst expose 5353 --udp\n  gst expose unix:///tmp/service.sock --port 5999",
		Args:    a.args(0, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				if cmd.Flags().Changed("port") || udp {
					return usage("--port and --udp need a target")
				}
				states, _, err := a.exposeStates(cmd.Context())
				if err != nil {
					return err
				}
				if len(states) == 0 {
					fmt.Fprintln(a.stdout, "No exposed ports. Expose one with `gst expose <target>`.")
					return nil
				}
				return writeExposes(a.stdout, states)
			}
			if cmd.Flags().Changed("port") && port == 0 { // ParseExpose takes 0 for the target port
				return usage("port must be between 1 and 65535")
			}
			rule, err := config.ParseExpose(args[0], port, udp)
			if err != nil {
				return &usageError{err}
			}
			err = a.update(func(f *config.File) error {
				for _, old := range f.Exposes {
					if old.Key() == rule.Key() {
						if old == rule {
							return nil
						}
						return withHint(fmt.Errorf("%s is already exposed, with another target", rule.Key()), "unexpose it first with `gst unexpose "+unexposeArgs(rule)+"`")
					}
				}
				// In order of port, which is how everything lists them.
				i, _ := slices.BinarySearchFunc(f.Exposes, rule, func(a, b config.Expose) int {
					return cmp.Or(cmp.Compare(a.Port, b.Port), strings.Compare(a.Protocol, b.Protocol))
				})
				f.Exposes = slices.Insert(f.Exposes, i, rule)
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return withHint(fmt.Errorf("the rule is saved but not applied: %w", err), "retry with `gst up`")
			}
			states, down, err := a.exposeStates(cmd.Context())
			if err != nil {
				return err
			}
			i := slices.IndexFunc(states, func(s wire.ExposeState) bool { return s.Protocol == rule.Protocol && s.Port == rule.Port })
			if i < 0 { // removed in the meantime
				return errors.New("the rule is saved but was removed since; see `gst expose`")
			}
			state := states[i]
			switch {
			case state.State == wire.StateFailed:
				return withHint(fmt.Errorf("the rule is saved but does not listen: %s", state.Error), "see it with `gst expose`")
			case state.State == wire.StateStopped && !down:
				return withHint(errors.New("the rule is saved but gst has not reported it"), "see it with `gst expose`")
			}
			return writeExposes(a.stdout, []wire.ExposeState{state})
		}),
	}
	cmd.Flags().IntVar(&port, "port", 0, "port on the gst IP (default: the target's port)")
	cmd.Flags().BoolVar(&udp, "udp", false, "expose UDP instead of TCP")
	return cmd
}

// unexposeArgs is what gst unexpose takes to remove rule.
func unexposeArgs(rule config.Expose) string {
	if rule.Protocol == "udp" {
		return strconv.Itoa(rule.Port) + " --udp"
	}
	return strconv.Itoa(rule.Port)
}

func (a *app) unexposeCommand() *cobra.Command {
	var udp bool
	cmd := &cobra.Command{
		Use:     "unexpose <port>",
		Short:   "Stop exposing a port and close its connections",
		Example: "  gst unexpose 5999\n  gst unexpose 5353 --udp",
		Args:    a.args(1, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			port, err := config.ParsePort(args[0])
			if err != nil {
				return &usageError{err}
			}
			protocol := "tcp"
			if udp {
				protocol = "udp"
			}
			err = a.update(func(f *config.File) error {
				i := slices.IndexFunc(f.Exposes, func(e config.Expose) bool { return e.Protocol == protocol && e.Port == port })
				if i < 0 {
					return withHint(fmt.Errorf("%s is not exposed", config.ExposeKey(protocol, port)), "see the exposed ports with `gst expose`")
				}
				f.Exposes = slices.Delete(f.Exposes, i, i+1)
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return withHint(fmt.Errorf("the rule is removed from the configuration, but may still run: %w", err), "retry with `gst up`")
			}
			fmt.Fprintf(a.stdout, "Unexposed %s.\n", config.ExposeKey(protocol, port))
			return nil
		}),
	}
	cmd.Flags().BoolVar(&udp, "udp", false, "the UDP port instead of the TCP one")
	return cmd
}

// exposeStates lists the rules of the configuration in its order, as the
// daemon reports them, and whether gst is down here.
func (a *app) exposeStates(ctx context.Context) ([]wire.ExposeState, bool, error) {
	f, err := a.load()
	if err != nil {
		return nil, false, err
	}
	// The rules can be seen while gst is not running.
	states := []wire.ExposeState{}
	var live []wire.ExposeState
	if !f.Down {
		if st := a.status(ctx); st != nil {
			live = st.Exposes
		}
	}
	for _, e := range f.Exposes {
		s := wire.ExposeState{Protocol: e.Protocol, Port: e.Port, Target: e.Target, State: wire.StateStopped}
		for _, v := range live {
			if v.Protocol == e.Protocol && v.Port == e.Port && v.Target == e.Target {
				s = v
				break
			}
		}
		states = append(states, s)
	}
	return states, f.Down, nil
}

func writeExposes(out io.Writer, states []wire.ExposeState) error {
	w := table(out)
	fmt.Fprintln(w, "PROTOCOL\tPORT\tADDRESS\tTARGET\tSTATE")
	for _, s := range states {
		address := s.Address
		if address == "" {
			address = "-"
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n", s.Protocol, s.Port, address, s.Target, exposeStateText(s))
	}
	return w.Flush()
}
