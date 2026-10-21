package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

func (a *app) forwardCommand() *cobra.Command {
	cmd := asGroup(&cobra.Command{Use: "forward", Short: "Add, remove, and list a tunnel's forwards"})
	cmd.AddCommand(a.forwardAddCommand(), a.forwardRemoveCommand(), a.forwardListCommand())
	return cmd
}

func (a *app) forwardAddCommand() *cobra.Command {
	var forwards forwardFlags
	var noRestart bool
	cmd := &cobra.Command{
		Use:   "add <tunnel> (-R spec | -L spec | -D [bind_address:]port)...",
		Short: "Add forwards to a tunnel",
		Long: `Add forwards to a tunnel. A spec is [bind_address:]port:host:hostport,
as in ssh:

  -R  listen on bind_address:port on the server; connect to host:hostport
      from this machine
  -L  listen on bind_address:port on this machine; connect to host:hostport
      from the server
  -D  listen on bind_address:port on this machine as a SOCKS5 proxy;
      connect from the server to each address that clients request.
      Only TCP CONNECT without authentication is supported. Domain names
      that clients send are resolved by the server.

For -R and -L, either [bind_address:]port or host:hostport can be a Unix
socket path instead, which is any field containing "/". Paths on this
machine may be relative or start with "~" and are saved as absolute paths;
paths on the server must be absolute. Sockets that gst creates on this
machine are accessible to their owner only.

Without bind_address a forward listens on loopback only; "*" or an empty
bind_address means all interfaces. For -R the server's sshd GatewayPorts
setting can restrict this further.

Adding a forward that already exists does nothing. A running tunnel
restarts to apply the change unless --no-restart is given.`,
		Example: `  gst forward add myhost -R 2222:localhost:22
  gst forward add myhost -L 15432:127.0.0.1:5432 -L 16379:127.0.0.1:6379
  gst forward add myhost -R '*:8080:localhost:3000'
  gst forward add myhost -D 1080
  gst forward add myhost -L ~/.gst/db.sock:/run/postgresql/.s.PGSQL.5432
  gst forward add myhost -R /home/user/agent.sock:$SSH_AUTH_SOCK`,
		Args:              a.args(1, 1),
		ValidArgsFunction: a.completeTunnels,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			name := args[0]
			fwds, err := forwards.parse(false)
			if err != nil {
				return err
			}
			if len(fwds) == 0 {
				return usage("give at least one -R, -L, or -D forward")
			}
			var messages []string
			added := false
			err = a.update(func(f *config.File) error {
				t, err := lookup(f, name)
				if err != nil {
					return err
				}
				messages, added = nil, false
				for _, fwd := range fwds {
					ok, err := addForward(name, t, fwd)
					if err != nil {
						return err
					}
					if ok {
						added = true
						messages = append(messages, fmt.Sprintf("Added %s to tunnel %s.", fwd.Spec(), name))
					} else {
						messages = append(messages, fmt.Sprintf("Tunnel %s already has %s.", name, fwd.Spec()))
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			for _, m := range messages {
				fmt.Fprintln(a.stdout, m)
			}
			if !added {
				return nil
			}
			return a.applyChange(name, noRestart)
		}),
	}
	forwards.register(cmd.Flags(), false)
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "do not restart a running tunnel")
	return cmd
}

func (a *app) forwardRemoveCommand() *cobra.Command {
	var forwards forwardFlags
	var noRestart bool
	cmd := &cobra.Command{
		Use:     "remove <tunnel> (-R | -L | -D [bind_address:]port | socket)...",
		Aliases: []string{"rm"},
		Short:   "Remove forwards from a tunnel",
		Long: `Remove forwards from a tunnel, identified by direction and listener:
[bind_address:]port or a socket path, with the same rules as gst forward add.
A full spec as printed by gst forward list works too. A running tunnel
restarts to apply the change unless --no-restart is given.`,
		Example: `  gst forward remove myhost -R 2222              # the -R forward listening on 127.0.0.1:2222
  gst forward remove myhost -R 0.0.0.0:2222
  gst forward remove myhost -L 127.0.0.1:15432:127.0.0.1:5432
  gst forward remove myhost -D 1080
  gst forward remove myhost -L ~/.gst/db.sock`,
		Args:              a.args(1, 1),
		ValidArgsFunction: a.completeTunnels,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			name := args[0]
			targets, err := forwards.parse(true)
			if err != nil {
				return err
			}
			if len(targets) == 0 {
				return usage("give at least one -R, -L, or -D listener")
			}
			var removed []config.Forward
			err = a.update(func(f *config.File) error {
				t, err := lookup(f, name)
				if err != nil {
					return err
				}
				removed = nil
				for _, target := range targets {
					i := indexOfListener(t.Forwards, target)
					if i < 0 || t.Forwards[i].Direction != target.Direction || (target.Target != "" && t.Forwards[i].Target != target.Target) {
						return withHint(fmt.Errorf("tunnel %s has no forward %s %s", name, target.Flag(), describeListener(target)),
							"list its forwards with `gst forward list "+name+"`")
					}
					removed = append(removed, t.Forwards[i])
					t.Forwards = append(t.Forwards[:i], t.Forwards[i+1:]...)
				}
				return nil
			})
			if err != nil {
				return err
			}
			for _, fwd := range removed {
				fmt.Fprintf(a.stdout, "Removed %s from tunnel %s.\n", fwd.Spec(), name)
			}
			return a.applyChange(name, noRestart)
		}),
	}
	forwards.register(cmd.Flags(), true)
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "do not restart a running tunnel")
	return cmd
}

func indexOfListener(forwards []config.Forward, target config.Forward) int {
	for i, fwd := range forwards {
		if fwd.SameListener(target) {
			return i
		}
	}
	return -1
}

func describeListener(f config.Forward) string {
	if f.Target == "" {
		return f.Listen
	}
	return f.Listen + ":" + f.Target
}

func (a *app) forwardListCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list <tunnel>",
		Aliases: []string{"ls"},
		Short:   "List a tunnel's forwards",
		Long: `List a tunnel's forwards as specs that gst forward add and remove accept.
The number of a forward is how the tunnel's log refers to it.`,
		Example:           "  gst forward list myhost",
		Args:              a.args(1, 1),
		ValidArgsFunction: a.completeTunnels,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			f, err := a.load()
			if err != nil {
				return err
			}
			t, err := lookup(f, args[0])
			if err != nil {
				return err
			}
			views := forwardViews(t.Forwards)
			if asJSON {
				return writeJSON(a.stdout, views)
			}
			if len(views) == 0 {
				fmt.Fprintf(a.stdout, "Tunnel %s has no forwards. Add one with: gst forward add %s -R spec | -L spec | -D port\n", args[0], args[0])
				return nil
			}
			for _, v := range views {
				fmt.Fprintf(a.stdout, "%d  %s\n", v.Number, v.Spec)
			}
			return nil
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
