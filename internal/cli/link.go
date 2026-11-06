package cli

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func (a *app) addCommand() *cobra.Command {
	var remoteGst string
	cmd := &cobra.Command{
		Use:        "add <name> [destination]",
		SuggestFor: []string{"link"},
		Short:      "Link this machine with another machine over SSH",
		Long: `Add a link: an SSH connection from this machine to another machine that
also runs gst. Over it, the gst daemons of the two machines join each
other's mesh: each learns every machine that the other knows, and the
traffic of the virtual network can pass through it in both directions. A
link runs in the background, starts at login, and reconnects after
failures; gst status shows its state.

The other machine needs gst 0.4 or later in the PATH of its login shell,
or set with --remote-gst, and someone must have run gst up there: gst
never sets up another machine. If it has not, this command says so and
keeps the link, which connects by itself after gst up there.

<name> uses 1-32 lowercase letters, digits, and '-', and also names the
machine at the other end until it reports its own name. <destination> is
an ssh destination such as host, user@host, or ssh://user@host:port, and
defaults to <name>. It is resolved with "ssh -G" every time the link
connects, so the link follows your ssh configuration: HostName, Port,
User, IdentityFile, known_hosts files, ConnectTimeout, ServerAliveInterval,
and ServerAliveCountMax. ProxyJump, ProxyCommand, and HostKeyAlias are
not supported.

Before saving, gst checks that an identity without a passphrase exists
and that known_hosts records the server; it never accepts unknown host
keys. To change a link, remove it and add it again.`,
		Example: `  gst add devbox    # the "devbox" entry of ~/.ssh/config
  gst add db admin@db.example.com
  gst add web ssh://deploy@203.0.113.7:2200 --remote-gst ~/bin/gst`,
		Args: a.args(1, 2),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := config.ValidateName(name); err != nil {
				return &usageError{err}
			}
			l := &config.Link{Destination: name, RemoteGst: remoteGst}
			if len(args) == 2 {
				l.Destination = args[1]
			}
			if err := l.Validate(); err != nil {
				return &usageError{err}
			}
			if _, err := a.daemonCommand(); err != nil {
				return err
			}
			err := a.update(func(f *config.File) error {
				if f.Links[name] != nil {
					return withHint(fmt.Errorf("link %s already exists", name),
						fmt.Sprintf("to change it, remove it with `gst remove %s` and add it again", name))
				}
				if err := a.preflight(cmd.Context(), l); err != nil {
					return err
				}
				f.Links[name] = l
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return withHint(fmt.Errorf("link %s was added but did not start: %w", name, err), "retry with `gst up`")
			}
			if err := a.watchLink(cmd.Context(), name, l); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Added link %s; it connects in the background. See it with: gst status\n", name)
			return nil
		}),
	}
	cmd.Flags().StringVar(&remoteGst, "remote-gst", "", "gst command on the server, when gst is not in the PATH of its login shell (default: gst)")
	return cmd
}

// linkWatch bounds how long gst add watches a new link.
const linkWatch = 10 * time.Second

// watchLink watches a new link until it shows how it goes, and fails if
// the other machine needs gst up. It only tells the person so: gst never
// sets up another machine.
func (a *app) watchLink(ctx context.Context, name string, l *config.Link) error {
	ctx, cancel := context.WithTimeout(ctx, linkWatch)
	defer cancel()
	for {
		st := a.status(ctx)
		if st == nil {
			return nil
		}
		for _, state := range st.Links {
			switch {
			case state.Name != name:
			case strings.Contains(state.Error, notUp):
				return withHint(fmt.Errorf("link %s was added, but gst is not up on that machine", name),
					fmt.Sprintf("run `gst up` there, for example with `ssh -t %s %s up`; the link then connects by itself", l.Destination, cmp.Or(l.RemoteGst, "gst")))
			case state.State == wire.StateRunning, state.Error != "":
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (a *app) removeCommand() *cobra.Command {
	return &cobra.Command{
		Use:               "remove <name>...",
		Aliases:           []string{"rm"},
		Short:             "Remove links to other machines",
		Example:           "  gst remove devbox\n  gst remove devbox db",
		Args:              a.args(1, -1),
		ValidArgsFunction: a.completeLinks,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			err := a.update(func(f *config.File) error {
				for _, name := range args {
					if f.Links[name] == nil {
						return namesHint(f, fmt.Errorf("no link named %s", name))
					}
				}
				for _, name := range args {
					delete(f.Links, name)
				}
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return withHint(fmt.Errorf("the links were removed from the configuration, but the daemon may still run them: %w", err), "retry with `gst up`")
			}
			for _, name := range args {
				fmt.Fprintf(a.stdout, "Removed link %s.\n", name)
			}
			return nil
		}),
	}
}
