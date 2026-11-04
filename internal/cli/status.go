package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func (a *app) statusCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status [machine]",
		Short: "Show a machine's links, proxy, maps, and routes",
		Long: `Show what gst runs on this machine, or on another machine of the mesh:
its links, its proxy, its maps, and its routes, with their state. Another
machine is asked through the mesh, so it must be reachable; see gst nodes.`,
		Example: "  gst status\n  gst status devcn",
		Args:    a.args(0, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			b, err := a.backend()
			if err != nil {
				return err
			}
			var st wire.Status
			if len(args) == 0 {
				var ok bool
				if st, ok = b.Status(cmd.Context()); !ok {
					return withHint(fmt.Errorf("the gst daemon does not answer"), "start it with `gst up`")
				}
			} else {
				machine, err := config.ParseMachine(args[0])
				if err != nil {
					return &usageError{err}
				}
				if st, err = b.NodeStatus(cmd.Context(), machine); err != nil {
					return withHint(err, "see the machines and the way to them with `gst nodes`")
				}
			}
			if asJSON {
				return writeJSON(a.stdout, st)
			}
			return writeStatus(a.stdout, st)
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// writeStatus prints a machine's status: the machine, its proxy, and
// tables of its links, maps, and routes.
func writeStatus(out io.Writer, st wire.Status) error {
	exit := "not an exit"
	if st.Node.Exit {
		exit = "an exit"
	}
	fmt.Fprintf(out, "%s (%s), %s, %s\n", st.Node.Name, shortID(st.Node.ID), st.Software, exit)
	proxy := "off"
	if st.Proxy.Listen != "" {
		proxy = st.Proxy.Listen + " (" + listenerText(st.Proxy) + ")"
	}
	fmt.Fprintf(out, "Proxy: %s\n", proxy)

	fmt.Fprintln(out)
	if len(st.Links) == 0 {
		fmt.Fprintln(out, "No links.")
	} else {
		w := table(out)
		fmt.Fprintln(w, "LINK\tSTATUS\tDESTINATION\tPEER")
		for _, l := range st.Links {
			v := linkView{Status: l.State, Error: l.Error, Peer: l.Peer}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", l.Name, v.statusText(), l.Destination, v.peerText())
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	fmt.Fprintln(out)
	if len(st.Maps) == 0 {
		fmt.Fprintln(out, "No maps.")
	} else {
		w := table(out)
		fmt.Fprintln(w, "MAP\tTARGET\tSTATUS")
		for _, m := range st.Maps {
			fmt.Fprintf(w, "%s\t%s on %s\t%s\n", m.Listen, m.Target, m.Machine, listenerText(m.Listener))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	fmt.Fprintln(out)
	if len(st.Routes) == 0 {
		fmt.Fprintln(out, "No routes.")
		return nil
	}
	w := table(out)
	fmt.Fprintln(w, "ROUTE\tVIA")
	for _, r := range st.Routes {
		fmt.Fprintf(w, "%s\t%s\n", r.Match, r.Via)
	}
	return w.Flush()
}

func listenerText(l wire.Listener) string {
	if l.Error != "" {
		return statusFailed + ": " + l.Error
	}
	return statusListening
}
