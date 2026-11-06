package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// directPoll is how often gst ping -v looks whether a try ended.
const directPoll = 500 * time.Millisecond

// meshStatus is what gst status prints without a machine. Its JSON form
// is part of the command line interface.
type meshStatus struct {
	Self wire.Status   `json:"self"` // this machine's daemon
	Mesh wire.Topology `json:"mesh"`
}

func (a *app) statusCommand() *cobra.Command {
	var asJSON, all bool
	cmd := &cobra.Command{
		Use:   "status [machine]",
		Short: "Show the machines of the mesh, or what one machine runs",
		Long: `Without a machine, show the machines that this machine reaches now, one
per line: its id, name, gst, and address, and how packets go to it:
directly over UDP, or along the SSH links, through the machines that it
names. Linked machines pass on what they know, so the list includes
machines several links away. Problems on this
machine, such as a link that is down, follow the list; with --all, the
running links between all the machines and the traffic with each of them
come first.

With a machine, show its address and links, asked through the mesh. A
machine is named by its name, its id or an id prefix of at least 4
characters, or the name of a link to it, optionally followed by .gst.`,
		Example: "  gst status\n  gst status --all\n  gst status devbox",
		Args:    a.args(0, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			b, err := a.backend()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				if all {
					return usage("--all shows the whole mesh; give no machine")
				}
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
			return writeMesh(a.stdout, st, topo, all)
		}),
	}
	cmd.Flags().BoolVar(&all, "all", false, "also show the running links and the traffic with each machine")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// noDaemon explains err from a daemon that does not answer.
func noDaemon(err error) error {
	return withHint(fmt.Errorf("the gst daemon does not answer: %w", err), "start it with `gst up`")
}

// writeMesh prints a line for each machine, this one first, with all the
// running links between them, and then the problems of this machine.
func writeMesh(out io.Writer, st wire.Status, topo wire.Topology, all bool) error {
	names := displayNames(topo)
	onNet := slices.ContainsFunc(topo.Nodes, func(n wire.TopoNode) bool { return n.Net != nil })
	w := table(out)
	for _, n := range topo.Nodes {
		var state []string
		switch {
		case n.ID == topo.Self:
			state = append(state, "-")
		case n.Direct:
			state = append(state, "online", "direct")
		case len(n.Path) > 2:
			via := make([]string, len(n.Path)-2)
			for i, id := range n.Path[1 : len(n.Path)-1] {
				via[i] = names[id]
			}
			state = append(state, "online", "via "+strings.Join(via, ", "))
		default:
			state = append(state, "online")
		}
		line := []string{shortID(n.ID), names[n.ID], cmp.Or(n.Software, "-")}
		if onNet {
			address := "-"
			if n.Net != nil {
				address = n.Net.Address
			}
			line = append(line, address)
		}
		fmt.Fprintln(w, strings.Join(append(line, strings.Join(state, "; ")), "\t"))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(topo.Nodes) == 1 && len(st.Links) == 0 {
		fmt.Fprintln(out, "\nNo links yet. Link to another machine with: gst add <name> [destination]")
	}
	if all && len(topo.Edges) > 0 {
		edges := slices.Clone(topo.Edges)
		slices.SortFunc(edges, func(a, b wire.TopoEdge) int {
			return cmp.Or(cmp.Compare(names[a.From], names[b.From]), cmp.Compare(a.Link, b.Link))
		})
		fmt.Fprintln(out, "\nLINK")
		for _, e := range edges {
			fmt.Fprintln(out, linkText(names[e.From], names[e.To], e.Link))
		}
	}
	if all && st.Net != nil && len(st.Net.Peers) > 0 {
		fmt.Fprintln(out)
		if err := writePeers(out, st.Net); err != nil {
			return err
		}
	}
	var problems []string
	for _, l := range st.Links {
		if l.State != wire.StateRunning {
			problems = append(problems, "link "+stateLinkText(st.Node.Name, l)+" is "+linkStateText(l))
		}
	}
	if st.Net != nil && st.Net.Error != "" {
		text := "virtual network: " + st.Net.Error
		if st.Net.Hint != "" {
			text += "; " + st.Net.Hint
		}
		problems = append(problems, text)
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

// writeStatus prints a machine's status: the machine, its address, and a
// table of its links.
func writeStatus(out io.Writer, st wire.Status) error {
	fmt.Fprintf(out, "%s (%s), %s\n", st.Node.Name, shortID(st.Node.ID), st.Software)
	if st.Net != nil {
		fmt.Fprintf(out, "Address: %s (subnet %s)\n", cmp.Or(st.Net.Address, "-"), cmp.Or(st.Net.Subnet, "-"))
	}

	fmt.Fprintln(out)
	if len(st.Links) == 0 {
		fmt.Fprintln(out, "No links.")
		return nil
	}
	w := table(out)
	fmt.Fprintln(w, "LINK\tSTATUS\tDESTINATION")
	for _, l := range st.Links {
		fmt.Fprintf(w, "%s\t%s\t%s\n", stateLinkText(st.Node.Name, l), linkStateText(l), l.Destination)
	}
	return w.Flush()
}

func (a *app) pingCommand() *cobra.Command {
	var count int
	var asJSON, verbose bool
	cmd := &cobra.Command{
		Use:   "ping <machine>",
		Short: "Check that a machine answers over the virtual network",
		Long: `Send a packet to a machine and back over the virtual network, once a
second, and print each reply: the machine's address, the way the packet
went, and the time it took. It goes the way of every program's packets,
through the interface and WireGuard, so the time is what programs get.
via an IP:port means that it went directly over UDP, to that address; via
SSH, along the SSH links, through the machines that it names. Exit status
is 1 if no reply came.

Traffic to a machine tries a UDP path to it, so the first replies may come
over SSH and the next over UDP.

With -v, after the replies and any try still running, also check the SSH
links, which carry the control of the network, and show the UDP path, or
why there is none, and what both machines did in their latest try: the
addresses they offered and the probes they heard.`,
		Example: "  gst ping devbox\n  gst ping gateway -c 10\n  gst ping devbox -v",
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
			var pings []wire.NetPing
			var last error
			for i := range count {
				if i > 0 {
					select {
					case <-cmd.Context().Done():
						return cmd.Context().Err()
					case <-time.After(time.Second):
					}
				}
				p, err := b.NetPing(cmd.Context(), machine)
				if err != nil {
					last = err
					if !asJSON {
						fmt.Fprintf(a.stdout, "no reply: %v\n", err)
					}
					continue
				}
				pings = append(pings, p)
				if !asJSON {
					writePing(a.stdout, p)
				}
			}
			if pings == nil {
				pings = []wire.NetPing{}
			}
			switch {
			case verbose:
				links, linksErr := b.Ping(cmd.Context(), machine)
				report, err := directReport(cmd.Context(), b, machine)
				if err != nil {
					fmt.Fprintf(a.stderr, "no report on the UDP path: %v\n", err)
				}
				switch {
				case asJSON && err == nil:
					v := verbosePing{Pings: pings, Direct: report}
					if linksErr == nil {
						v.Links = &links
					}
					if err := writeJSON(a.stdout, v); err != nil {
						return err
					}
				case !asJSON:
					writeLinks(a.stdout, links, linksErr)
					if err == nil {
						writeDirect(a.stdout, report)
					}
				}
			case asJSON:
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
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "also show the UDP path and how the latest try at one went")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// verbosePing is what gst ping -v --json prints. Its JSON form is part of
// the command line interface.
type verbosePing struct {
	Pings  []wire.NetPing    `json:"pings"`
	Links  *wire.Ping        `json:"links,omitempty"` // the answer over the SSH links
	Direct wire.DirectReport `json:"direct"`
}

// directReport waits for a running try at a UDP path to end and reports
// it.
func directReport(ctx context.Context, b backend, machine string) (wire.DirectReport, error) {
	for {
		r, err := b.Direct(ctx, machine)
		if err != nil || !r.Trying {
			return r, err
		}
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-time.After(directPoll):
		}
	}
}

func writeDirect(out io.Writer, r wire.DirectReport) {
	switch {
	case r.Path != nil:
		fmt.Fprintf(out, "UDP path to %s: up, %s -> %s\n", r.Node.Name, r.Path.Local, r.Path.Remote)
	case r.Off:
		fmt.Fprintf(out, "UDP path to %s: off on this machine\n", r.Node.Name)
	case !r.Next.IsZero():
		fmt.Fprintf(out, "UDP path to %s: none, next try in %s\n", r.Node.Name, time.Until(r.Next).Round(time.Second))
	default:
		fmt.Fprintf(out, "UDP path to %s: none\n", r.Node.Name)
	}
	writeTrace(out, "This machine", r.Local, "")
	writeTrace(out, r.Node.Name, r.Peer, r.PeerError)
}

// writeTrace prints a machine's events with the time since its first.
func writeTrace(out io.Writer, machine string, t wire.DirectTrace, failure string) {
	fmt.Fprintf(out, "\n%s:\n", machine)
	if failure != "" {
		fmt.Fprintf(out, "  no answer: %s\n", failure)
		return
	}
	if len(t.Events) == 0 {
		fmt.Fprintln(out, "  no try yet")
		return
	}
	for _, e := range t.Events {
		fmt.Fprintf(out, "  %7.3fs  %s\n", e.Time.Sub(t.Events[0].Time).Seconds(), e.Text)
	}
}

func writePing(out io.Writer, p wire.NetPing) {
	way := ""
	switch p.Path {
	case wire.PathDirect:
		way = " via " + cmp.Or(p.Endpoint, "UDP") // The path may have just ended.
	case wire.PathSSH:
		way = " via SSH"
		if len(p.Via) > 0 {
			way += " (" + strings.Join(nodeNames(p.Via), ", ") + ")"
		}
	}
	fmt.Fprintf(out, "reply from %s (%s)%s in %.1f ms\n", p.Node.Name, p.Address, way, p.TimeMS)
}

func viaText(machines []wire.NodeInfo) string {
	if len(machines) == 0 {
		return ""
	}
	return " via " + strings.Join(nodeNames(machines), ", ")
}

func nodeNames(machines []wire.NodeInfo) []string {
	names := make([]string, len(machines))
	for i, n := range machines {
		names[i] = n.Name
	}
	return names
}

// writeLinks reports the check of the SSH links in gst ping -v.
func writeLinks(out io.Writer, p wire.Ping, err error) {
	fmt.Fprintln(out)
	if err != nil {
		fmt.Fprintf(out, "SSH links: no answer: %v\n", err)
		return
	}
	fmt.Fprintf(out, "SSH links: %s answered%s in %.1f ms\n", p.Node.Name, viaText(p.Via), p.TimeMS)
}
