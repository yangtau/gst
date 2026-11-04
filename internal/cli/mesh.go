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
	"time"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/service"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func (a *app) meshCommand() *cobra.Command {
	cmd := asGroup(&cobra.Command{
		Use:   "mesh",
		Short: "Link gst on several machines",
		Long: `Link gst on several machines, without any setup: a machine with a tunnel
joins the mesh, every tunnel links this machine's gst with the gst on its
server over the tunnel's SSH connection, and the server's gst joins when
it is first linked to. Each then sees the other in gst mesh peers. Linked machines pass on what they know, so gst mesh nodes
on any of them shows every machine in the mesh, and a tunnel to
mesh:<machine> reaches any of them. A server without gst still takes part
as a machine that its tunnels reach. The server's gst must be 0.2 or later
in the PATH of its login shell, or set with --remote-gst; --no-mesh keeps
a tunnel out, and gst mesh disable takes this machine out. In the mesh,
the gst daemon keeps running even without started tunnels.`,
	})
	cmd.AddCommand(a.meshEnableCommand(), a.meshDisableCommand(), a.meshPeersCommand(), a.meshNodesCommand())
	return cmd
}

func (a *app) meshEnableCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Join the mesh again, or rename this machine in it",
		Long: `Join the mesh: accept peer links from other machines' gst and link over
every tunnel without --no-mesh. A machine joins by itself once it has a
tunnel or a peer links to it, so this is needed only after gst mesh
disable or to change the node name, shown to peers, which defaults to
this machine's host name.`,
		Example: "  gst mesh enable\n  gst mesh enable --name laptop",
		Args:    a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("name") {
				if err := config.ValidateNodeName(name); err != nil {
					return &usageError{err}
				}
			}
			var m config.Mesh
			err := a.update(func(f *config.File) error {
				f.NoMesh = false
				if f.Mesh == nil {
					f.Mesh = &config.Mesh{ID: config.NewMeshID(), Name: config.DefaultNodeName()}
				}
				if cmd.Flags().Changed("name") {
					f.Mesh.Name = name
				}
				m = *f.Mesh
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return withHint(fmt.Errorf("the mesh is enabled, but the gst daemon did not start: %w", err), "retry with `gst mesh enable`")
			}
			fmt.Fprintf(a.stdout, "Mesh enabled; this machine is %s (%s).\n", m.Name, m.ID[:8])
			return nil
		}),
	}
	cmd.Flags().StringVar(&name, "name", "", "node name shown to peers (default: the host name)")
	return cmd
}

// meshBackend returns the backend for a command that needs the mesh, or
// an error when the mesh is disabled. A machine that has not joined the
// mesh yet joins it now.
func (a *app) meshBackend(ctx context.Context) (backend, error) {
	f, err := a.load()
	if err != nil {
		return nil, err
	}
	if f.NoMesh {
		return nil, withHint(errors.New("the mesh is disabled on this machine"), "enable it with `gst mesh enable`")
	}
	if f.Mesh == nil {
		if err := a.joinMesh(ctx); err != nil {
			return nil, err
		}
	}
	return a.backend()
}

// joinMesh gives this machine its place in the mesh, unless the mesh is
// disabled, and starts the daemon.
func (a *app) joinMesh(ctx context.Context) error {
	if err := a.update(func(f *config.File) error { f.JoinMesh(); return nil }); err != nil {
		return err
	}
	if err := a.sync(ctx); err != nil {
		return withHint(fmt.Errorf("start the gst daemon: %w", err), "retry with `gst mesh enable`")
	}
	return nil
}

func (a *app) meshDisableCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Leave the mesh",
		Long: `Leave the mesh: close every peer link, refuse new ones, and stop joining
by itself. Tunnels keep running and link again after gst mesh enable.
The node keeps its id only while enabled; enabling again creates a new one.`,
		Args: a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			wasEnabled := false
			err := a.update(func(f *config.File) error {
				wasEnabled = !f.NoMesh
				f.NoMesh = true
				return nil
			})
			if err != nil {
				return err
			}
			if !wasEnabled {
				fmt.Fprintln(a.stdout, "The mesh is already disabled.")
				return nil
			}
			if err := a.sync(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, "Mesh disabled.")
			return nil
		}),
	}
}

func (a *app) meshPeersCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "peers",
		Short: "List peer links",
		Long: `List this machine's peer links: out over its tunnels with --mesh, and in
from other machines' tunnels. A link is connecting, up, or down (failed
and retrying, with the reason).`,
		Args: a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			b, err := a.meshBackend(cmd.Context())
			if err != nil {
				return err
			}
			peers, err := b.Peers(cmd.Context())
			if err != nil {
				return withHint(fmt.Errorf("the gst daemon does not answer: %w", err), "start it with `gst mesh enable`")
			}
			if asJSON {
				return writeJSON(a.stdout, peers)
			}
			fmt.Fprintf(a.stdout, "This machine: %s (%s)\n", peers.Node.Name, shortID(peers.Node.ID))
			if len(peers.Links) == 0 {
				fmt.Fprintln(a.stdout, "No peer links. Each tunnel without --no-mesh links once it runs: gst add <name>")
				return nil
			}
			w := table(a.stdout)
			fmt.Fprintln(w, "DIRECTION\tTUNNEL\tSTATE\tPEER\tSINCE")
			for _, l := range peers.Links {
				peer := "-"
				if l.Peer.ID != "" {
					peer = l.Peer.Name + " (" + shortID(l.Peer.ID) + ")"
				}
				tunnel := l.Tunnel
				if tunnel == "" {
					tunnel = "-"
				}
				state := l.State
				if l.Error != "" {
					state += ": " + l.Error
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", l.Direction, tunnel, state, peer, l.Since.Local().Format(time.DateTime))
			}
			return w.Flush()
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func (a *app) meshNodesCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "nodes",
		Short: "Show every machine in the mesh",
		Long: `Show every machine in the mesh as this machine knows it, with the tunnels
between them. Each gst node announces its tunnels to its peers, which pass the announcement on, so the view includes machines
several links away and SSH servers without gst. A machine is reachable
when this machine can get to it through running tunnels, going on past a
machine only over a peer link.`,
		Args: a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			b, err := a.meshBackend(cmd.Context())
			if err != nil {
				return err
			}
			topo, err := b.Nodes(cmd.Context())
			if err != nil {
				return withHint(fmt.Errorf("the gst daemon does not answer: %w", err), "start it with `gst mesh enable`")
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

// writeTopology prints the machines and then the tunnels between them.
// gst nodes that share a name are told apart by their ids.
func writeTopology(out io.Writer, topo wire.Topology) error {
	count := map[string]int{}
	for _, n := range topo.Nodes {
		count[n.Name]++
	}
	names := map[string]string{}
	w := table(out)
	fmt.Fprintln(w, "NODE\tID\tSOFTWARE\tREACHABLE")
	for _, n := range topo.Nodes {
		name, id, software := n.Name, "-", "-"
		if n.Gst {
			id, software = shortID(n.ID), n.Software
			if count[n.Name] > 1 {
				name += " (" + id + ")"
			}
		}
		names[n.ID] = name
		reach := "no"
		switch {
		case n.ID == topo.Self:
			reach = "this machine"
		case n.Reachable:
			reach = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, id, software, reach)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(topo.Edges) == 0 {
		fmt.Fprintln(out, "\nNo tunnels in the mesh. Add one with: gst add <name>")
		return nil
	}
	edges := slices.Clone(topo.Edges)
	slices.SortFunc(edges, func(a, b wire.TopoEdge) int {
		return cmp.Or(cmp.Compare(names[a.From], names[b.From]), cmp.Compare(a.Tunnel, b.Tunnel))
	})
	fmt.Fprintln(out)
	w = table(out)
	fmt.Fprintln(w, "FROM\tTO\tTUNNEL\tSTATE")
	for _, e := range edges {
		state := e.State
		if e.Linked {
			state += ", peer link up"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", names[e.From], names[e.To], e.Tunnel, state)
	}
	return w.Flush()
}

func (a *app) connectCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "connect <machine> <port | host:port | socket>",
		Short: "Connect standard input and output to a machine of the mesh",
		Long: `Connect standard input and output to a port of a machine in the mesh,
through whichever gst links lead there, for example as an ssh
ProxyCommand. <machine> is a name or id from gst mesh nodes. A bare port
means localhost on that machine, a host:port is resolved there, and an
absolute path is a Unix socket there. The machine needs no gst when a
gst node has a tunnel to it.`,
		Example: `  gst connect devcn 5432
  ssh -o ProxyCommand='gst connect %n 22' devcn`,
		Args: a.args(2, 2),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			target, err := parseTarget(args[1])
			if err != nil {
				return err
			}
			b, err := a.meshBackend(cmd.Context())
			if err != nil {
				return err
			}
			conn, err := b.Connect(cmd.Context(), args[0], target)
			if err != nil {
				return withHint(err, "see the machines and links with `gst mesh nodes`")
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

// parseTarget reads a port, host:port, or socket path of gst connect.
func parseTarget(s string) (wire.Target, error) {
	t := wire.Target{Network: "tcp", Address: s}
	switch {
	case config.IsSocket(s):
		t.Network = "unix"
	case !strings.Contains(s, ":"):
		t.Address = "localhost:" + s
	}
	if !config.ValidTarget(t.Network, t.Address) {
		return wire.Target{}, usage("invalid target %q: use a port, host:port, or an absolute socket path", s)
	}
	return t, nil
}

func shortID(id string) string { return id[:min(8, len(id))] }

// peerBridgeCommand connects its standard input and output to the gst
// daemon on this machine. A peer's daemon runs it over SSH.
func (a *app) peerBridgeCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "peer-bridge",
		Short:  "Connect standard input and output to the gst daemon",
		Long:   "Connect standard input and output to the gst daemon on this machine, for a peer's gst, which runs this command over SSH. A machine that has not joined the mesh joins it now, unless the mesh is disabled there.",
		Hidden: true,
		Args:   a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			f, err := a.load()
			if err != nil {
				return err
			}
			if f.NoMesh {
				return errors.New("gst mesh is disabled on this machine; run `gst mesh enable` there")
			}
			paths, err := service.DefaultPaths()
			if err != nil {
				return err
			}
			var d net.Dialer
			conn, err := d.DialContext(cmd.Context(), "unix", paths.Socket)
			if err != nil || f.Mesh == nil {
				// Not in the mesh yet, or the daemon is not running.
				if err == nil {
					conn.Close()
				}
				if err := a.joinMesh(cmd.Context()); err != nil {
					return err
				}
				if conn, err = d.DialContext(cmd.Context(), "unix", paths.Socket); err != nil {
					return errors.New("the gst daemon does not answer on this machine")
				}
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
