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
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

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
