package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
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
		Long: `Link gst on several machines. A tunnel with --mesh links this machine's
gst with the gst on its server, over the tunnel's SSH connection, so that
each sees the other in gst mesh peers. Both machines need gst mesh enable;
the server needs gst 0.2 or later in the PATH of its login shell, or
--remote-gst. With the mesh enabled, the gst daemon keeps running even
without started tunnels.`,
	})
	cmd.AddCommand(a.meshEnableCommand(), a.meshDisableCommand(), a.meshPeersCommand())
	return cmd
}

func (a *app) meshEnableCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Take part in the mesh",
		Long: `Take part in the mesh: accept peer links from other machines' gst and
link over tunnels with --mesh. The node name, shown to peers, defaults to
this machine's host name; --name changes it.`,
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
				if f.Mesh == nil {
					f.Mesh = &config.Mesh{ID: config.NewMeshID(), Name: defaultNodeName()}
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

// defaultNodeName derives a valid node name from the host name.
func defaultNodeName() string {
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(strings.ToLower(host), ".")
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 32 {
		name = strings.Trim(name[:32], "-")
	}
	if config.ValidateNodeName(name) != nil {
		return "node"
	}
	return name
}

func (a *app) meshDisableCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Leave the mesh",
		Long: `Leave the mesh: close every peer link and refuse new ones. Tunnels keep
running; their --mesh setting takes effect again after gst mesh enable.
The node keeps its id only while enabled; enabling again creates a new one.`,
		Args: a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			wasEnabled := false
			err := a.update(func(f *config.File) error {
				wasEnabled = f.Mesh != nil
				f.Mesh = nil
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
			f, err := a.load()
			if err != nil {
				return err
			}
			if f.Mesh == nil {
				return withHint(errors.New("the mesh is not enabled on this machine"), "enable it with `gst mesh enable`")
			}
			b, err := a.backend()
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
				fmt.Fprintln(a.stdout, "No peer links. Link over a tunnel with: gst set <tunnel> --mesh")
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

func shortID(id string) string { return id[:min(8, len(id))] }

// peerBridgeCommand connects its standard input and output to the gst
// daemon on this machine. A peer's daemon runs it over SSH.
func (a *app) peerBridgeCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "peer-bridge",
		Short:  "Connect standard input and output to the gst daemon",
		Long:   "Connect standard input and output to the gst daemon on this machine, for a peer's gst, which runs this command over SSH.",
		Hidden: true,
		Args:   a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			paths, err := service.DefaultPaths()
			if err != nil {
				return err
			}
			var d net.Dialer
			conn, err := d.DialContext(cmd.Context(), "unix", paths.Socket)
			if err != nil {
				return errors.New("the gst daemon is not running on this machine; run `gst mesh enable` there")
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

// peers asks the daemon for its peer links.
func (b *daemonBackend) Peers(ctx context.Context) (wire.Peers, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := b.connectOnce(ctx)
	if err != nil {
		return wire.Peers{}, err
	}
	defer c.Close()
	return c.Peers(ctx)
}
