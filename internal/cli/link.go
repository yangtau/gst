package cli

import (
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

func (a *app) addCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "add <destination> [-- <ssh option>...]",
		Short: "Add a machine to the network over SSH",
		Long: `Add the machine at an SSH destination to the network, and link this
machine to it. When that machine is in another network, the two networks
become one. When it is in this network already, this machine links to it
too. The link runs in the background, starts at login, and reconnects
after failures.

<destination> is an ssh destination: a host of your ssh configuration,
user@host, or ssh://user@host:port. ssh options after -- go with it, such
as -p 2222 or -i ~/.ssh/key. gst resolves both with "ssh -G" each time the
link connects, so the link follows HostName, Port, User, IdentityFile,
known_hosts files, and ConnectTimeout. ProxyJump, ProxyCommand, and
HostKeyAlias are not supported.

The other machine needs gst up run there, with a gst of the same protocol
(see gst version). gst never sets up another machine; until then the
link waits, and connects by itself after gst up there. This machine needs
a private key without a passphrase for it, and the server's host key in
known_hosts. gst checks both before it saves the link.

Adding a destination again replaces its ssh options. gst add also brings
back a machine that was removed from the network, from either end.`,
		Example: `  gst add devbox                    # the "devbox" host of ~/.ssh/config
  gst add admin@ssh.example.com
  gst add ssh.example.com -- -p 2222 -l deploy`,
		Args: func(cmd *cobra.Command, args []string) error {
			before := len(args)
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				before = dash
			}
			return a.args(1, 1)(cmd, args[:before])
		},
		RunE: run(func(cmd *cobra.Command, args []string) error {
			l := config.Link{Destination: args[0], Admit: true}
			if cmd.ArgsLenAtDash() >= 0 {
				l.SSHArgs = slices.Clone(args[cmd.ArgsLenAtDash():])
			}
			if err := l.Validate(); err != nil {
				return &usageError{err}
			}
			if _, err := a.daemonCommand(); err != nil {
				return err
			}
			if err := a.preflight(cmd.Context(), &l); err != nil {
				return err
			}
			members := map[string]bool{}
			if b, err := a.backend(); err == nil && a.status(cmd.Context()) != nil {
				if topo, err := call[wire.Topology](cmd.Context(), b, wire.MethodNodes, nil); err == nil {
					for _, n := range topo.Nodes {
						members[n.ID] = true
					}
				}
			}
			changed := false
			err := a.update(func(f *config.File) error {
				if old := f.Link(l.Destination); old != nil {
					changed = true
					*old = l
				} else {
					f.Links = append(f.Links, l)
				}
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return withHint(fmt.Errorf("added the link to %s, but gst did not start: %w", l.Destination, err), "retry with `gst up`")
			}
			peer, err := a.watchLink(cmd.Context(), &l)
			if err != nil {
				return err
			}
			switch {
			case changed:
				fmt.Fprintf(a.stdout, "Changed this machine's link to %s.\n", l.Destination)
			case peer == nil:
				fmt.Fprintf(a.stdout, "Added the link to %s. It connects in the background; see it with `gst status`.\n", l.Destination)
			case members[peer.ID]:
				fmt.Fprintf(a.stdout, "%s is already in the network; this machine now also links to it.\n", peer.Name)
			default:
				if address := a.addressOf(cmd.Context(), peer.ID); address != "" {
					fmt.Fprintf(a.stdout, "Added %s to the network: %s, %s.gst.\n", peer.Name, address, peer.Name)
				} else {
					fmt.Fprintf(a.stdout, "Added %s to the network.\n", peer.Name)
				}
			}
			return nil
		}),
	}
}

// linkWatch bounds how long gst add watches a new link.
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
				return nil, withHint(fmt.Errorf("added the link to %s, but gst is not up there", l.Destination),
					fmt.Sprintf("run `%s gst up`; the link then connects by itself", sshCommand("-t", l)))
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

// addressOf waits a moment for the gst IP of machine id.
func (a *app) addressOf(ctx context.Context, id string) string {
	b, err := a.backend()
	if err != nil {
		return ""
	}
	if st := a.status(ctx); st == nil || st.Net == nil || st.Net.Interface == "" {
		return "" // This machine is not on the gst network.
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for {
		if topo, err := call[wire.Topology](ctx, b, wire.MethodNodes, nil); err == nil {
			for _, n := range topo.Nodes {
				if n.ID == id && n.Net != nil {
					return n.Net.Address
				}
			}
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (a *app) unlinkCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "unlink <destination>...",
		Short: "Remove this machine's links",
		Long: `Remove this machine's links to the destinations given. Only this machine
changes: the machines stay in the network, and the other machines learn
nothing. A machine that no link leads to any more shows as offline. To
take a machine out of the network, use gst remove.`,
		Example:           "  gst unlink devbox\n  gst unlink devbox admin@ssh.example.com",
		Args:              a.args(1, -1),
		ValidArgsFunction: a.completeLinks,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			err := a.update(func(f *config.File) error {
				for _, destination := range args {
					if f.Link(destination) == nil {
						return linksHint(f, fmt.Errorf("this machine has no link to %s", destination))
					}
				}
				f.Links = slices.DeleteFunc(f.Links, func(l config.Link) bool { return slices.Contains(args, l.Destination) })
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return withHint(fmt.Errorf("the links are removed from the configuration, but may still run: %w", err), "retry with `gst up`")
			}
			for _, destination := range slices.Compact(slices.Clone(args)) {
				fmt.Fprintf(a.stdout, "Unlinked %s.\n", destination)
			}
			return nil
		}),
	}
}

func (a *app) removeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <machine>",
		Short: "Remove a machine from the network",
		Long: `Remove a machine from the network, for every machine of it: they drop
their links to it and forget it, and it drops its links and forgets them,
as soon as it hears of it. Any machine of the network can remove any
other, and this one, which leaves the network. A machine is given by its
name, or its id or an id prefix, as gst status shows them.

A removed machine comes back only with gst add, on it or on a machine of
the network. To keep it out for good, also take its key out of the other
machines' authorized_keys.`,
		Example: "  gst remove tablet\n  gst remove 3f2a",
		Args:    a.args(1, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			machine, err := config.ParseMachine(args[0])
			if err != nil {
				return &usageError{err}
			}
			b, err := a.backend()
			if err != nil {
				return err
			}
			if a.status(cmd.Context()) == nil {
				return withHint(errNoDaemon, "start it with `gst up`")
			}
			r, err := call[wire.Removal](cmd.Context(), b, wire.MethodRemove, wire.NodeParams{Node: machine})
			if err != nil {
				return err
			}
			if r.Self {
				fmt.Fprintln(a.stdout, "This machine left the network. Join one again with `gst add <destination>`.")
				return nil
			}
			fmt.Fprintf(a.stdout, "Removed %s from the network.\n", r.Node.Name)
			if len(r.Stranded) > 0 {
				fmt.Fprintf(a.stdout, "%s reached the network only through it, and stays offline until it links to another machine.\n", strings.Join(nodeNames(r.Stranded), ", "))
			}
			return nil
		}),
	}
}

// linksHint adds this machine's links to err.
func linksHint(f *config.File, err error) error {
	if len(f.Links) == 0 {
		return withHint(err, "this machine has no links yet; add one with `gst add <destination>`")
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
