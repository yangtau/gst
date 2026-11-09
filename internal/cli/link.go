package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func (a *app) linkCommand() *cobra.Command {
	var remoteGst string
	cmd := &cobra.Command{
		Use:   "link <destination>",
		Short: "Link this machine to another machine over SSH",
		Long: `Link this machine to another machine over SSH. The two machines then
share one network: each reaches every machine that the other reaches. The
link runs in the background, starts at login, and reconnects after
failures.

<destination> is an ssh destination: a host of your ssh configuration,
user@host, or ssh://user@host:port. gst resolves it with "ssh -G" each
time the link connects, so the link follows HostName, Port, User,
IdentityFile, known_hosts files, ConnectTimeout, ServerAliveInterval, and
ServerAliveCountMax. ProxyJump, ProxyCommand, and HostKeyAlias are not
supported.

The other machine needs:
  - gst up run there. gst never sets up another machine; until then the
    link waits, and connects by itself after gst up there.
  - a gst of the same protocol (see gst version), in the PATH of its login
    shell, or given with --remote-gst.
And this one needs a private key without a passphrase for it, and the
server's host key in known_hosts. gst checks both before it saves the
link.

To change a link, unlink it and link it again.`,
		Example: `  gst link devbox                    # the "devbox" host of ~/.ssh/config
  gst link admin@db.example.com
  gst link ssh://deploy@203.0.113.7:2200 --remote-gst ~/bin/gst`,
		Args: a.args(1, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			l := config.Link{Destination: args[0], RemoteGst: remoteGst}
			if err := l.Validate(); err != nil {
				return &usageError{err}
			}
			if _, err := a.daemonCommand(); err != nil {
				return err
			}
			err := a.update(func(f *config.File) error {
				if f.Link(l.Destination) != nil {
					return withHint(fmt.Errorf("this machine already links to %s", l.Destination),
						fmt.Sprintf("to change the link, run `gst unlink %s` and link again", l.Destination))
				}
				if err := a.preflight(cmd.Context(), &l); err != nil {
					return err
				}
				f.Links = append(f.Links, l)
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return withHint(fmt.Errorf("linked to %s, but gst did not start: %w", l.Destination, err), "retry with `gst up`")
			}
			peer, err := a.watchLink(cmd.Context(), &l)
			if err != nil {
				return err
			}
			switch {
			case peer != nil && peer.Name == l.Destination:
				fmt.Fprintf(a.stdout, "Linked to %s.\n", l.Destination)
			case peer != nil:
				fmt.Fprintf(a.stdout, "Linked to %s, which is %s.\n", l.Destination, peer.Name)
			default:
				fmt.Fprintf(a.stdout, "Linked to %s. It connects in the background; see it with `gst status`.\n", l.Destination)
			}
			return nil
		}),
	}
	cmd.Flags().StringVar(&remoteGst, "remote-gst", "", "gst command on the other machine, when gst is not in the PATH of its login shell (default: gst)")
	return cmd
}

// linkWatch bounds how long gst link watches a new link.
const linkWatch = 10 * time.Second

// watchLink watches a new link until it shows how it goes, returns the
// machine at its other end once it runs, and fails if that machine needs
// gst up. It only tells the person so: gst never sets up another machine.
func (a *app) watchLink(ctx context.Context, l *config.Link) (*wire.NodeInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, linkWatch)
	defer cancel()
	for {
		st := a.status(ctx)
		if st == nil {
			return nil, nil
		}
		for _, state := range st.Links {
			switch {
			case state.Destination != l.Destination:
			case strings.Contains(state.Error, notUp):
				return nil, withHint(fmt.Errorf("linked to %s, but gst is not up there", l.Destination),
					fmt.Sprintf("run `ssh -t %s %s up`; the link then connects by itself", l.Destination, cmp.Or(l.RemoteGst, "gst")))
			case state.State == wire.StateRunning:
				return state.Peer, nil
			case state.Error != "":
				return nil, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (a *app) unlinkCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "unlink <machine>...",
		Short: "Remove this machine's links to other machines",
		Long: `Remove this machine's links to the machines given. A machine is given by
its name, its id or an id prefix, or the destination of the link.

A link belongs to the machine that made it: to remove a link that another
machine made to this one, run gst unlink there.`,
		Example:           "  gst unlink devbox\n  gst unlink devbox admin@db.example.com",
		Args:              a.args(1, -1),
		ValidArgsFunction: a.completeLinks,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			var running []wire.LinkState
			if st := a.status(cmd.Context()); st != nil {
				running = st.Links
			}
			var removed []string
			err := a.update(func(f *config.File) error {
				var gone []string
				for _, ref := range args {
					found := linksTo(f, running, ref)
					if len(found) == 0 {
						return linksHint(f, fmt.Errorf("this machine has no link to %s", ref))
					}
					for _, destination := range found {
						if !slices.Contains(gone, destination) {
							gone = append(gone, destination)
						}
					}
				}
				f.Links = slices.DeleteFunc(f.Links, func(l config.Link) bool { return slices.Contains(gone, l.Destination) })
				removed = gone
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return withHint(fmt.Errorf("the links are removed from the configuration, but may still run: %w", err), "retry with `gst up`")
			}
			for _, destination := range removed {
				fmt.Fprintf(a.stdout, "Unlinked %s.\n", destination)
			}
			return nil
		}),
	}
}

// linksTo returns the destinations of the links of f that ref names: by
// its destination, or by the name, id, or id prefix of the machine that a
// running link leads to.
func linksTo(f *config.File, running []wire.LinkState, ref string) []string {
	var found []string
	if f.Link(ref) != nil {
		found = append(found, ref)
	}
	machine, err := config.ParseMachine(ref)
	if err != nil {
		return found
	}
	for _, l := range running {
		p := l.Peer
		if p == nil || f.Link(l.Destination) == nil || slices.Contains(found, l.Destination) {
			continue
		}
		if p.ID == machine || p.Name == machine || len(machine) >= 4 && strings.HasPrefix(p.ID, machine) {
			found = append(found, l.Destination)
		}
	}
	return found
}

// linksHint adds this machine's links to err.
func linksHint(f *config.File, err error) error {
	if len(f.Links) == 0 {
		return withHint(err, "this machine has no links yet; add one with `gst link <destination>`")
	}
	return withHint(err, "this machine links to "+strings.Join(destinations(f), ", "))
}

func destinations(f *config.File) []string {
	list := make([]string, len(f.Links))
	for i, l := range f.Links {
		list[i] = l.Destination
	}
	return list
}

var errNoDaemon = errors.New("gst is not running on this machine")
