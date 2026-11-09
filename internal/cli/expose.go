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
		Use: "expose <target>", Short: "Expose a TCP port, UDP port, or Unix stream socket on this machine's gst IP",
		Long: `Forward a port on this machine's gst IP to a target reachable by its daemon.
A bare port means 127.0.0.1 on that port. Explicit targets use tcp://host:port,
udp://host:port, or unix:///absolute/path. --port selects the exposed port;
it defaults to the target port and is required for Unix sockets.

Rules persist across restarts. Only the gst IPv4 address is bound; there is
no wildcard fallback. All machines of the mesh can connect. The target sees
a connection from this daemon, not the original client. No TLS or HTTP
processing is performed. --udp selects UDP for a bare port; udp:// also
selects UDP. Unix datagram sockets are not supported.

The same protocol and port cannot have different targets. Remove the old
rule first. Repeating the same rule is harmless.`,
		Example: "  gst expose 8080 --port 5999\n  gst expose 5353 --udp\n  gst expose unix:///tmp/service.sock --port 5999",
		Args:    a.args(1, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
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
						flag := ""
						if rule.Protocol == "udp" {
							flag = " --udp"
						}
						return withHint(fmt.Errorf("expose %s already has a different target", rule.Key()), "remove it with `gst expose remove "+strconv.Itoa(rule.Port)+flag+"`")
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
				return withHint(fmt.Errorf("rule saved but not applied: %w", err), "retry with `gst up`")
			}
			states, down, err := a.exposeStates(cmd.Context())
			if err != nil {
				return err
			}
			i := slices.IndexFunc(states, func(s wire.ExposeState) bool { return s.Protocol == rule.Protocol && s.Port == rule.Port })
			if i < 0 { // removed in the meantime
				return errors.New("rule saved but removed since; run `gst expose list`")
			}
			state := states[i]
			switch {
			case state.State == wire.StateFailed:
				return withHint(fmt.Errorf("rule saved but not listening: %s", state.Error), "inspect it with `gst expose list`")
			case state.State == wire.StateStopped && !down:
				return withHint(errors.New("rule saved but daemon has not reported it"), "inspect it with `gst expose list`")
			}
			return writeExposes(a.stdout, []wire.ExposeState{state})
		}),
	}
	cmd.Flags().IntVar(&port, "port", 0, "port to expose on the gst IP (default: target port)")
	cmd.Flags().BoolVar(&udp, "udp", false, "expose UDP instead of TCP")
	var json bool
	list := &cobra.Command{Use: "list", Short: "List this machine's exposed ports", Args: a.args(0, 0), RunE: run(func(cmd *cobra.Command, _ []string) error {
		states, _, err := a.exposeStates(cmd.Context())
		if err != nil {
			return err
		}
		if json {
			return writeJSON(a.stdout, states)
		}
		return writeExposes(a.stdout, states)
	})}
	list.Flags().BoolVar(&json, "json", false, "print JSON")
	var removeUDP bool
	remove := &cobra.Command{Use: "remove <port>", Short: "Remove an exposed port and close its connections", Args: a.args(1, 1), RunE: run(func(cmd *cobra.Command, args []string) error {
		port, err := config.ParsePort(args[0])
		if err != nil {
			return &usageError{err}
		}
		protocol := "tcp"
		if removeUDP {
			protocol = "udp"
		}
		err = a.update(func(f *config.File) error {
			i := slices.IndexFunc(f.Exposes, func(e config.Expose) bool { return e.Protocol == protocol && e.Port == port })
			if i < 0 {
				return fmt.Errorf("no expose rule for %s", config.ExposeKey(protocol, port))
			}
			f.Exposes = slices.Delete(f.Exposes, i, i+1)
			return nil
		})
		if err != nil {
			return err
		}
		if err := a.sync(cmd.Context()); err != nil {
			return fmt.Errorf("rule removed from config but daemon did not apply it: %w", err)
		}
		fmt.Fprintf(a.stdout, "Removed expose %s.\n", config.ExposeKey(protocol, port))
		return nil
	})}
	remove.Flags().BoolVar(&removeUDP, "udp", false, "remove the UDP rule instead of TCP")
	cmd.AddCommand(list, remove)
	// Keep Cobra's usage for a command that accepts both a target and subcommands.
	cmd.SetUsageTemplate(cmd.UsageTemplate())
	return cmd
}

// exposeStates lists the rules of the configuration in its order, as the
// daemon reports them, and whether gst is down here.
func (a *app) exposeStates(ctx context.Context) ([]wire.ExposeState, bool, error) {
	f, err := a.load()
	if err != nil {
		return nil, false, err
	}
	// Configuration remains inspectable while the daemon is stopped.
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
