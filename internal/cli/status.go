package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/vnet"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// directPoll is how often gst ping -v looks whether a try ended.
const directPoll = 500 * time.Millisecond

func (a *app) statusCommand() *cobra.Command {
	var verbose bool
	cmd := &cobra.Command{
		Use:   "status [machine]",
		Short: "Show the machines of the network, or one machine's links",
		Long: `Without a machine, show every machine that this one reaches, one per
line: its id, name, gst version, gst IP, and how packets go to it:
direct over UDP, or relayed along the SSH links, through the machines
named. Problems on this machine, such as a link that is down, follow.
With -v, also show the links between all the machines, and the traffic
with each machine.

With a machine, show its gst IP, exposed ports, and links. A machine is
given by its name, or its id or an id prefix of at least 4 characters,
optionally followed by .gst.`,
		Example: "  gst status\n  gst status -v\n  gst status devbox",
		Args:    a.args(0, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			b, err := a.backend()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				if verbose {
					return usage("-v shows the whole network; give no machine")
				}
				machine, err := config.ParseMachine(args[0])
				if err != nil {
					return &usageError{err}
				}
				st, err := b.NodeStatus(cmd.Context(), machine)
				if err != nil {
					return withHint(err, "see the machines with `gst status`")
				}
				return writeStatus(a.stdout, st)
			}
			st, ok := b.Status(cmd.Context())
			if !ok {
				return withHint(errNoDaemon, "start it with `gst up`")
			}
			topo, err := b.Nodes(cmd.Context())
			if err != nil {
				return noDaemon(err)
			}
			return writeMachines(a.stdout, st, topo, verbose)
		}),
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "also show the links between all the machines, and the traffic with each")
	return cmd
}

// noDaemon explains err from a daemon that does not answer.
func noDaemon(err error) error {
	return withHint(fmt.Errorf("%w: %w", errNoDaemon, err), "start it with `gst up`")
}

// wayText says how packets go to a machine: direct, or relayed along the
// SSH links through the machines named.
func wayText(direct bool, via []string) string {
	switch {
	case direct:
		return "direct"
	case len(via) > 0:
		return "relay via " + strings.Join(via, ", ")
	}
	return "relay"
}

// writeMachines prints a line for each machine, this one first, with
// all the running links between them when verbose, and then the problems
// of this machine.
func writeMachines(out io.Writer, st wire.Status, topo wire.Topology, verbose bool) error {
	names := displayNames(topo)
	onNet := slices.ContainsFunc(topo.Nodes, func(n wire.TopoNode) bool { return n.Net != nil })
	w := table(out)
	for _, n := range topo.Nodes {
		var way string
		switch {
		case n.ID == topo.Self:
			way = "-"
		case !n.Reachable && !n.Direct:
			way = "offline"
		default:
			var via []string
			if len(n.Path) > 2 {
				for _, id := range n.Path[1 : len(n.Path)-1] {
					via = append(via, names[id])
				}
			}
			way = wayText(n.Direct, via)
		}
		line := []string{shortID(n.ID), names[n.ID], cmp.Or(n.Software, "-")}
		if onNet {
			ip := "-"
			if n.Net != nil {
				ip = n.Net.Address
			}
			line = append(line, ip)
		}
		fmt.Fprintln(w, strings.Join(append(line, way), "\t"))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(topo.Nodes) == 1 && len(st.Links) == 0 {
		fmt.Fprintln(out, "\nNo links yet. Link to another machine with `gst link <destination>`.")
	}
	if verbose && len(topo.Edges) > 0 {
		var edges []string
		for _, e := range topo.Edges {
			edges = append(edges, names[e.From]+" -> "+names[e.To])
		}
		slices.Sort(edges)
		fmt.Fprintln(out, "\nLINKS")
		for _, e := range edges {
			fmt.Fprintln(out, e)
		}
	}
	if verbose && st.Net != nil && len(st.Net.Peers) > 0 {
		fmt.Fprintln(out)
		if err := writePeers(out, st.Net); err != nil {
			return err
		}
	}
	var problems []string
	for _, l := range st.Links {
		if l.State != wire.StateRunning {
			problems = append(problems, "link to "+destinationText(l)+": "+linkStateText(l))
		}
	}
	if st.Net != nil && st.Net.Error != "" {
		text := "network: " + st.Net.Error
		if st.Net.Hint != "" {
			text += "; " + st.Net.Hint
		}
		problems = append(problems, text)
	}
	for _, e := range st.Exposes {
		if e.State != wire.StateRunning {
			problems = append(problems, "exposed "+config.ExposeKey(e.Protocol, e.Port)+": "+exposeStateText(e))
		}
	}
	if len(problems) > 0 {
		fmt.Fprintln(out, "\nProblems:")
		for _, p := range problems {
			fmt.Fprintln(out, "  - "+p)
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

// writeStatus prints a machine's status: the machine, its gst IP, its
// exposed ports, and a table of its links.
func writeStatus(out io.Writer, st wire.Status) error {
	fmt.Fprintf(out, "%s (%s), %s\n", st.Node.Name, shortID(st.Node.ID), st.Software)
	if st.Net != nil {
		fmt.Fprintf(out, "gst IP: %s (subnet %s)\n", cmp.Or(st.Net.Address, "-"), cmp.Or(st.Net.Subnet, "-"))
	}

	if len(st.Exposes) > 0 {
		fmt.Fprintln(out)
		if err := writeExposes(out, st.Exposes); err != nil {
			return err
		}
	}
	fmt.Fprintln(out)
	if len(st.Links) == 0 {
		fmt.Fprintln(out, "No links.")
		return nil
	}
	w := table(out)
	fmt.Fprintln(w, "LINK TO\tMACHINE\tSTATE")
	for _, l := range st.Links {
		machine := "-"
		if l.Peer != nil {
			machine = l.Peer.Name
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", l.Destination, machine, linkStateText(l))
	}
	return w.Flush()
}

func (a *app) pingCommand() *cobra.Command {
	var count, size int
	var verbose bool
	cmd := &cobra.Command{
		Use:   "ping <machine>",
		Short: "Check that a machine answers over the network",
		Long: `Send a packet to a machine and back once a second, and print each reply:
the machine's gst IP, how the packet went, and the time it took. The packet
goes the way that every program's packets go, so the time is what programs
get. Exit status is 1 if no reply came.

A packet goes direct over UDP, to the address shown, or is relayed along
the SSH links, through the machines named. Traffic is what starts a direct
path, so the first replies may be relayed.

With -s, send packets of that size. Small packets can pass where large ones
do not; past the interface's MTU they go in fragments.

With -v, also check the SSH links, and show the UDP path, or why there is
none, what both machines did in their latest try at one, and where this
machine lost packets.`,
		Example: "  gst ping devbox\n  gst ping gateway -c 10\n  gst ping devbox -s 1140\n  gst ping devbox -v",
		Args:    a.args(1, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			if count < 1 {
				return usage("-c must be at least 1")
			}
			if size < 16 || size > vnet.MaxPingSize {
				return usage("-s must be from 16 to %d", vnet.MaxPingSize)
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
				p, err := b.NetPing(cmd.Context(), machine, size)
				if err != nil {
					last = err
					fmt.Fprintf(a.stdout, "no reply: %v\n", err)
					continue
				}
				pings = append(pings, p)
				writePing(a.stdout, p)
			}
			if count > 1 {
				writePingSummary(a.stdout, count, pings)
			}
			if verbose {
				links, linksErr := b.Ping(cmd.Context(), machine)
				writeLinks(a.stdout, links, linksErr)
				report, err := directReport(cmd.Context(), b, machine)
				if err != nil {
					fmt.Fprintf(a.stderr, "no report on the UDP path: %v\n", err)
				} else {
					writeDirect(a.stdout, report)
				}
			}
			if len(pings) == 0 {
				return withHint(fmt.Errorf("%s did not answer: %w", args[0], last), "see the machines with `gst status`")
			}
			return nil
		}),
	}
	cmd.Flags().IntVarP(&count, "count", "c", 3, "number of replies to ask for")
	cmd.Flags().IntVarP(&size, "size", "s", 16, "bytes of data in each packet")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "also show the UDP path, the latest try at one, and lost packets")
	return cmd
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
	if !r.Off {
		s := r.Stats
		fmt.Fprintf(out, "UDP sockets: port %d, ", s.Port)
		if len(s.Interfaces) > 0 {
			fmt.Fprintf(out, "bound to %s, ", strings.Join(s.Interfaces, " and "))
		}
		fmt.Fprintf(out, "buffers of %d and %d bytes\n", s.ReadBuffer, s.WriteBuffer)
	}
	fmt.Fprintf(out, "Packets lost: %d to UDP errors (these took SSH), %d to the SSH queue, %d from unknown sources, %d on the way to WireGuard\n",
		r.Stats.WriteErrors, r.Stats.QueueDrops, r.Stats.Strays, r.Stats.InboundDrops)
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
		way = wayText(true, nil)
		if p.Endpoint != "" { // The path may have just ended.
			way += " " + p.Endpoint
		}
		way += ", "
	case wire.PathSSH:
		way = wayText(false, nodeNames(p.Via)) + ", "
	}
	fmt.Fprintf(out, "reply from %s (%s): %s%.1f ms\n", p.Node.Name, p.Address, way, p.TimeMS)
}

// writePingSummary prints what came of count packets.
func writePingSummary(out io.Writer, count int, pings []wire.NetPing) {
	fmt.Fprintf(out, "%d sent, %d received, %.0f%% lost", count, len(pings), 100*float64(count-len(pings))/float64(count))
	if len(pings) > 0 {
		lowest, highest, sum := pings[0].TimeMS, pings[0].TimeMS, 0.0
		for _, p := range pings {
			lowest, highest, sum = min(lowest, p.TimeMS), max(highest, p.TimeMS), sum+p.TimeMS
		}
		fmt.Fprintf(out, "; %.1f/%.1f/%.1f ms min/avg/max", lowest, sum/float64(len(pings)), highest)
	}
	fmt.Fprintln(out)
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
