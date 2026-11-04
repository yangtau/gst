package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// meshStatus is what gst status prints without a machine. Its JSON form
// is part of the command line interface.
type meshStatus struct {
	Self wire.Status   `json:"self"` // this machine's daemon
	Mesh wire.Topology `json:"mesh"`
}

func (a *app) statusCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status [machine]",
		Short: "Show the machines of the mesh, or what one machine runs",
		Long: `Without a machine, show every machine of the mesh as this machine knows
it, one per line: its id, name, and gst, and whether it is online, the
machines that connections to it pass through, and whether it is an exit.
Linked machines pass on what they know, so the list includes machines
several links away. Problems on this machine, such as a link that is
down, follow the list.

With a machine, show its links, proxy, maps, and routes, asked through
the mesh. A machine is named by its name, its id or an id prefix of at
least 4 characters, or the name of a link to it, optionally followed by
.gst.`,
		Example: "  gst status\n  gst status devcn",
		Args:    a.args(0, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			b, err := a.backend()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				machine, err := config.ParseMachine(args[0])
				if err != nil {
					return &usageError{err}
				}
				st, err := b.NodeStatus(cmd.Context(), machine)
				if err != nil {
					return withHint(err, "see the machines of the mesh with `gst status`")
				}
				if asJSON {
					return writeJSON(a.stdout, st)
				}
				return writeStatus(a.stdout, st)
			}
			st, ok := b.Status(cmd.Context())
			if !ok {
				return withHint(errors.New("the gst daemon does not answer"), "start it with `gst up`")
			}
			topo, err := b.Nodes(cmd.Context())
			if err != nil {
				return noDaemon(err)
			}
			if asJSON {
				return writeJSON(a.stdout, meshStatus{st, topo})
			}
			return writeMesh(a.stdout, st, topo)
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// noDaemon explains err from a daemon that does not answer.
func noDaemon(err error) error {
	return withHint(fmt.Errorf("the gst daemon does not answer: %w", err), "start it with `gst up`")
}

// writeMesh prints a line for each machine, this one first, and then the
// problems of this machine's links and listeners.
func writeMesh(out io.Writer, st wire.Status, topo wire.Topology) error {
	names := displayNames(topo)
	w := table(out)
	for _, n := range topo.Nodes {
		var state []string
		switch {
		case n.ID == topo.Self:
			state = append(state, "-")
		case !n.Reachable:
			state = append(state, "offline")
		case len(n.Path) > 2:
			via := make([]string, len(n.Path)-2)
			for i, id := range n.Path[1 : len(n.Path)-1] {
				via[i] = names[id]
			}
			state = append(state, "online", "via "+strings.Join(via, ", "))
		default:
			state = append(state, "online")
		}
		if n.Exit {
			state = append(state, "exit")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", shortID(n.ID), names[n.ID], cmp.Or(n.Software, "-"), strings.Join(state, "; "))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(topo.Nodes) == 1 && len(st.Links) == 0 {
		fmt.Fprintln(out, "\nNo links yet. Link to another machine with: gst link add <name> [destination]")
	}
	var problems []string
	for _, l := range st.Links {
		if l.State != wire.StateRunning {
			problems = append(problems, "link "+l.Name+" is "+linkStateText(l))
		}
	}
	if st.Proxy.Error != "" {
		problems = append(problems, "proxy "+st.Proxy.Listen+" failed: "+st.Proxy.Error)
	}
	for _, m := range st.Maps {
		if m.Error != "" {
			problems = append(problems, "map "+m.Listen+" failed: "+m.Error)
		}
	}
	if len(problems) > 0 {
		fmt.Fprintln(out, "\n# Problems:")
		for _, p := range problems {
			fmt.Fprintln(out, "#   - "+p)
		}
	}
	return nil
}

// displayNames names the machines, telling apart the ones that share a
// name by their ids.
func displayNames(topo wire.Topology) map[string]string {
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
	return names
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
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", l.Name, linkStateText(l), l.Destination, peerText(l.Peer))
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

func (a *app) pingCommand() *cobra.Command {
	var count int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ping <machine>",
		Short: "Check that a machine answers through the mesh",
		Long: `Ask a machine of the mesh to answer through the links that lead to it,
once a second, and print each answer with the machines it passed and
the time it took, which includes setting up the relay at each machine on
the way. Exit status is 1 if no answer came.`,
		Example: "  gst ping devcn\n  gst ping devsg -c 10",
		Args:    a.args(1, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			if count < 1 {
				return usage("-c must be at least 1")
			}
			machine, err := config.ParseMachine(args[0])
			if err != nil {
				return &usageError{err}
			}
			b, err := a.backend()
			if err != nil {
				return err
			}
			var pings []wire.Ping
			var last error
			for i := range count {
				if i > 0 {
					select {
					case <-cmd.Context().Done():
						return cmd.Context().Err()
					case <-time.After(time.Second):
					}
				}
				p, err := b.Ping(cmd.Context(), machine)
				if err != nil {
					last = err
					if !asJSON {
						fmt.Fprintf(a.stdout, "no answer: %v\n", err)
					}
					continue
				}
				pings = append(pings, p)
				if !asJSON {
					writePing(a.stdout, p)
				}
			}
			if asJSON {
				if pings == nil {
					pings = []wire.Ping{}
				}
				if err := writeJSON(a.stdout, pings); err != nil {
					return err
				}
			}
			if len(pings) == 0 {
				return withHint(fmt.Errorf("%s did not answer: %w", args[0], last), "see the machines of the mesh with `gst status`")
			}
			return nil
		}),
	}
	cmd.Flags().IntVarP(&count, "count", "c", 3, "number of answers to ask for")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func writePing(out io.Writer, p wire.Ping) {
	path := "directly"
	if len(p.Via) > 0 {
		via := make([]string, len(p.Via))
		for i, n := range p.Via {
			via[i] = n.Name
		}
		path = "via " + strings.Join(via, ", ")
	}
	fmt.Fprintf(out, "pong from %s (%s) %s in %.1f ms\n", p.Node.Name, shortID(p.Node.ID), path, p.TimeMS)
}
