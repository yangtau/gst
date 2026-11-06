package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/service"
)

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
	var yes bool
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Run gst on this machine and put it on the virtual network",
		Long: `Run the gst daemon in the background, now and at login: it runs the
links, accepts links from other machines, and puts this machine on the
virtual network. Other machines can link to this one only after this
command. Run it on each machine, and again after upgrading gst or gst
down. On a machine that is up it changes nothing.

The virtual network needs a small system service, its helper, which takes
root to install. When it is missing or out of date, gst says what sudo
would run and what that does, and asks first; sudo may then ask for your
password. Without a terminal gst does not ask, and only says so.`,
		Example: `  gst up
  gst up --yes   # do not ask; gst still says what it runs`,
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
			return a.joinNetwork(yes)
		}),
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask before running sudo; gst still says what it runs")
	return cmd
}

func (a *app) downCommand() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop gst on this machine and take it off the virtual network",
		Long: `Undo gst up: stop the gst daemon and remove its service, so links stop
and other machines cannot link to this one, and remove the helper of the
virtual network, with its interface, routes, and names. This takes no
password. The configuration stays, with the machine's address and links;
gst up runs gst again, and asks to install the helper again.`,
		Args: a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			if err := a.update(func(f *config.File) error { f.Down = true; return nil }); err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, "gst is down; run gst up to start it again.")
			return a.leaveNetwork(yes)
		}),
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask if removing the helper takes sudo; gst still says what it runs")
	return cmd
}

// notUp is how a machine refuses a link until someone runs gst up there.
const notUp = "gst is not up on this machine"

// peerBridgeCommand connects its standard input and output to the gst
// daemon on this machine. Another machine's daemon runs it over SSH to
// link with this one.
func (a *app) peerBridgeCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "peer-bridge",
		Short:  "Connect standard input and output to the gst daemon",
		Long:   "Connect standard input and output to the gst daemon on this machine, for another machine's gst, which runs this command over SSH to link with this one. It refuses unless someone ran gst up here. The daemon restarts first if it stopped, or runs another gst.",
		Hidden: true,
		Args:   a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			f, err := a.load()
			if err != nil {
				return err
			}
			// Another machine never sets up gst here: that is for a person
			// on this one.
			if f.Down || f.Node == nil {
				return errors.New(notUp + "; run `gst up` there")
			}
			if err := a.sync(cmd.Context()); err != nil {
				return err
			}
			socket, err := service.Socket()
			if err != nil {
				return err
			}
			var d net.Dialer
			conn, err := d.DialContext(cmd.Context(), "unix", socket)
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
