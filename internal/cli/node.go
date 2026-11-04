package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/service"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// noDaemon explains err from a daemon that does not answer.
func noDaemon(err error) error {
	return withHint(fmt.Errorf("the gst daemon does not answer: %w", err), "start it with `gst up`")
}

func (a *app) nodesCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "nodes",
		Short: "Show every machine in the mesh",
		Long: `Show every machine in the mesh as this machine knows it, and the links
between them. Linked machines pass on what they know, so the list
includes machines several links away. ROUTE is the way connections to a
machine take from here. A machine is named by its name, its id or an id
prefix of at least 4 characters, or the name of a link to it, optionally
followed by .gst. gst status <machine> shows what a machine runs.`,
		Args: a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			b, err := a.backend()
			if err != nil {
				return err
			}
			topo, err := b.Nodes(cmd.Context())
			if err != nil {
				return noDaemon(err)
			}
			if asJSON {
				return writeJSON(a.stdout, topo)
			}
			return writeTopology(a.stdout, topo)
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// writeTopology prints the machines and then the links between them.
// Machines that share a name are told apart by their ids.
func writeTopology(out io.Writer, topo wire.Topology) error {
	count := map[string]int{}
	for _, n := range topo.Nodes {
		count[n.Name]++
	}
	names := map[string]string{}
	for _, n := range topo.Nodes {
		names[n.ID] = n.Name
		if count[n.Name] > 1 {
			names[n.ID] += " (" + shortID(n.ID) + ")"
		}
	}
	w := table(out)
	fmt.Fprintln(w, "NAME\tID\tSOFTWARE\tEXIT\tROUTE")
	for _, n := range topo.Nodes {
		exit := "no"
		if n.Exit {
			exit = "yes"
		}
		route := "unreachable"
		switch {
		case n.ID == topo.Self:
			route = "this machine"
		case n.Reachable && len(n.Path) > 1:
			hops := make([]string, len(n.Path)-1)
			for i, id := range n.Path[1:] {
				hops[i] = names[id]
			}
			route = strings.Join(hops, " → ")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", names[n.ID], shortID(n.ID), cmp.Or(n.Software, "-"), exit, route)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(topo.Edges) == 0 {
		fmt.Fprintln(out, "\nNo links in the mesh. Add one with: gst link add <name> [destination]")
		return nil
	}
	edges := slices.Clone(topo.Edges)
	slices.SortFunc(edges, func(a, b wire.TopoEdge) int {
		return cmp.Or(cmp.Compare(names[a.From], names[b.From]), cmp.Compare(a.Link, b.Link))
	})
	fmt.Fprintln(out)
	w = table(out)
	fmt.Fprintln(w, "FROM\tLINK\tTO")
	for _, e := range edges {
		fmt.Fprintf(w, "%s\t%s\t%s\n", names[e.From], e.Link, names[e.To])
	}
	return w.Flush()
}

func (a *app) connectCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "connect <machine> <port | host:port | socket>",
		Short: "Connect standard input and output to a machine",
		Long: `Connect standard input and output to a port or socket of a machine in the
mesh, for example as an ssh ProxyCommand. A bare port means localhost on
that machine, and an absolute path is a Unix socket there. A host:port
other than that machine's own needs the machine to be an exit.`,
		Example: `  gst connect devcn 5432
  gst connect devcn /run/user/1000/app.sock
  ssh -o ProxyCommand='gst connect %h %p' devcn.gst`,
		Args: a.args(2, 2),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			machine, err := config.ParseMachine(args[0])
			if err != nil {
				return &usageError{err}
			}
			target, err := config.ParseTarget(args[1])
			if err != nil {
				return &usageError{err}
			}
			b, err := a.backend()
			if err != nil {
				return err
			}
			conn, err := b.Connect(cmd.Context(), machine, wire.Target{Network: config.Network(target), Address: target})
			if err != nil {
				return err
			}
			defer conn.Close()
			go func() {
				if a.stdin != nil {
					io.Copy(conn, a.stdin)
				}
				if c, ok := conn.(interface{ CloseWrite() error }); ok {
					c.CloseWrite()
				}
			}()
			_, err = io.Copy(a.stdout, conn)
			return err
		}),
	}
}

func (a *app) nameCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "name [name]",
		Short: "Show or change this machine's name in the mesh",
		Long: `Show this machine's name and id, or change its name. Other machines
use the name to reach this one, as <name>.gst; it defaults to the host
name. Names need not be unique; ids are.`,
		Example: "  gst name\n  gst name laptop",
		Args:    a.args(0, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if err := config.ValidateName(args[0]); err != nil {
					return &usageError{err}
				}
			}
			node, err := a.updateNode(cmd.Context(), func(n *config.Node) {
				if len(args) == 1 {
					n.Name = args[0]
				}
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "This machine is %s (%s).\n", node.Name, node.ID)
			return nil
		}),
	}
}

func (a *app) exitCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "exit [on | off]",
		Short: "Show or change whether this machine is an exit",
		Long: `Show whether this machine is an exit, or turn that on or off. Any machine
of the mesh connects to addresses on its own machine for the others:
localhost, loopback addresses, and Unix sockets. Only an exit also
connects to other addresses, such as its local network or the internet,
so that routes can send connections out through it.`,
		Example: "  gst exit\n  gst exit on",
		Args:    a.args(0, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			var on bool
			if len(args) == 1 {
				switch args[0] {
				case "on":
					on = true
				case "off":
				default:
					return usage("invalid argument %q: use on or off", args[0])
				}
			}
			node, err := a.updateNode(cmd.Context(), func(n *config.Node) {
				if len(args) == 1 {
					n.Exit = on
				}
			})
			if err != nil {
				return err
			}
			if node.Exit {
				fmt.Fprintf(a.stdout, "%s is an exit: other machines can connect through it to any address.\n", node.Name)
			} else {
				fmt.Fprintf(a.stdout, "%s is not an exit: other machines connect through it only to its own ports and sockets.\n", node.Name)
			}
			return nil
		}),
	}
}

// updateNode changes this machine's node with fn, applies a change, and
// returns the node.
func (a *app) updateNode(ctx context.Context, fn func(*config.Node)) (config.Node, error) {
	var node, before config.Node
	err := a.update(func(f *config.File) error {
		if f.Node == nil {
			f.Node = &config.Node{ID: config.NewNodeID(), Name: config.DefaultNodeName()}
		}
		before = *f.Node
		fn(f.Node)
		node = *f.Node
		return nil
	})
	if err != nil || node == before {
		return node, err
	}
	return node, a.sync(ctx)
}

func (a *app) upCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Run gst on this machine",
		Long: `Run the gst daemon in the background, now and at login: it runs the
started links, the proxy, and the maps, and accepts links from other
machines. gst runs from the first change to its configuration, so this is
needed only after gst down, or to check that gst runs.`,
		Args: a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			if err := a.update(func(f *config.File) error { f.Down = false; return nil }); err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return err
			}
			f, err := a.load()
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "gst is up; this machine is %s (%s).\n", f.Node.Name, shortID(f.Node.ID))
			return nil
		}),
	}
}

func (a *app) downCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Stop gst on this machine",
		Long: `Stop the gst daemon and keep it from starting at login: links, the
proxy, and the maps stop, and other machines cannot link to this one.
The configuration stays; gst up runs gst again.`,
		Args: a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			if err := a.update(func(f *config.File) error { f.Down = true; return nil }); err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, "gst is down; run gst up to start it again.")
			return nil
		}),
	}
}

// peerBridgeCommand connects its standard input and output to the gst
// daemon on this machine. Another machine's daemon runs it over SSH to
// link with this one.
func (a *app) peerBridgeCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "peer-bridge",
		Short:  "Connect standard input and output to the gst daemon",
		Long:   "Connect standard input and output to the gst daemon on this machine, for another machine's gst, which runs this command over SSH to link with this one. The daemon starts first if it does not run, or runs another gst.",
		Hidden: true,
		Args:   a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			f, err := a.load()
			if err != nil {
				return err
			}
			if f.Down {
				return errors.New("gst is down on this machine; run `gst up` there")
			}
			if f.Node == nil {
				// The first link gives this machine its identity.
				if err := a.update(func(*config.File) error { return nil }); err != nil {
					return err
				}
			}
			if err := a.sync(cmd.Context()); err != nil {
				return err
			}
			paths, err := service.DefaultPaths()
			if err != nil {
				return err
			}
			var d net.Dialer
			conn, err := d.DialContext(cmd.Context(), "unix", paths.Socket)
			if err != nil {
				return errors.New("the gst daemon does not answer on this machine")
			}
			defer conn.Close()
			done := make(chan struct{}, 2)
			go func() {
				io.Copy(conn, os.Stdin)
				conn.(*net.UnixConn).CloseWrite()
				done <- struct{}{}
			}()
			go func() {
				io.Copy(os.Stdout, conn)
				done <- struct{}{}
			}()
			// The link ends when either side ends.
			<-done
			return nil
		}),
	}
}
