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
		Long: `Without a machine, show every machine of the network, one per line: its
id, name, gst version, gst IP, and how packets go to it: direct over UDP,
or relayed along the SSH links, through the machines named, or offline,
with when it was last online. Problems on this machine, such as a link
that is down, follow.
With -v, also show the links between all the machines, as "from -> to"
with their destinations and states, and the traffic with each machine. A
link belongs to the machine it starts from, and only that machine knows its
destination and whether it is down: of the links that other machines made,
only the running ones are known.

With a machine, show its gst IP, exposed ports, and every link that
starts or ends at it, in the same way. A machine is given by its name, or its id or an id prefix of at
least 4 characters, optionally followed by .gst.`,
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
				ref, err := config.ParseNodeRef(args[0])
				if err != nil {
					return &usageError{err}
				}
				st, err := call[wire.Status](cmd.Context(), b, wire.MethodNodeStatus, wire.NodeParams{Node: ref})
				if err != nil {
					return withHint(err, "see the machines with `gst status`")
				}
				// The links that other machines made to it are the network's.
				topo, _ := call[wire.Topology](cmd.Context(), b, wire.MethodTopology, nil)
				return writeStatus(a.stdout, st, topo)
			}
			st, ok := status(cmd.Context(), b)
			if !ok {
				return withHint(errNoDaemon, "start it with `gst up`")
			}
			topo, err := call[wire.Topology](cmd.Context(), b, wire.MethodTopology, nil)
			if err != nil {
				return noDaemon(err)
			}
			return writeNodes(a.stdout, st, topo, verbose)
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
func writeNodes(out io.Writer, st wire.Status, topo wire.Topology, verbose bool) error {
	names := displayNames(topo)
	onNet := slices.ContainsFunc(topo.Nodes, func(n wire.TopoNode) bool { return n.Net != nil })
	w := table(out)
	for _, n := range topo.Nodes {
		var way string
		switch {
		case n.ID == topo.Self:
			way = "-"
		case !n.Online():
			way = "offline"
			if !n.Seen.IsZero() {
				way += ", seen " + agoText(time.Since(n.Seen))
			}
		default:
			var via []string
			if len(n.Route) > 2 {
				for _, id := range n.Route[1 : len(n.Route)-1] {
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
	if r := st.Removed; r != nil {
		if r.By.ID == st.Node.ID {
			fmt.Fprintf(out, "\nThis machine left the network %s.", agoText(time.Since(r.At)))
		} else {
			fmt.Fprintf(out, "\nThis machine was removed from the network by %s %s.", r.By.Name, agoText(time.Since(r.At)))
		}
		fmt.Fprintln(out, " Join again with `gst add <destination>`, here or on a machine of the network.")
	} else if len(topo.Nodes) == 1 && len(st.Links) == 0 {
		fmt.Fprintln(out, "\nNo links yet. Add another machine with `gst add <destination>`.")
	}
	if verbose {
		self := wire.NodeInfo{ID: topo.Self, Name: names[topo.Self]}
		rows := linkRows(self, st.Links, topo, func(wire.TopoLink) bool { return true })
		if len(rows) > 0 {
			fmt.Fprintln(out)
			if err := writeLinkRows(out, rows); err != nil {
				return err
			}
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
	// The network's changes travel along the links only, not directly.
	var cut []string
	for _, n := range topo.Nodes {
		if n.ID != topo.Self && n.Direct && !n.Reachable {
			cut = append(cut, names[n.ID])
		}
	}
	if len(cut) > 0 {
		problems = append(problems, "no link leads to "+strings.Join(cut, ", ")+", reached only directly: removals and new names do not travel between them and this machine; link to them with `gst add`")
	}
	// A removed machine is in no network, which the removal says already.
	if st.Net != nil && st.Net.Error != "" && st.Removed == nil {
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

// agoText says how long ago something was, as d.
func agoText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
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
// exposed ports, and a table of the links from and to it. Its own links
// come from st; the links that other machines made to it from topo.
func writeStatus(out io.Writer, st wire.Status, topo wire.Topology) error {
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
	rows := linkRows(st.Node, st.Links, topo, func(e wire.TopoLink) bool { return e.To == st.Node.ID })
	fmt.Fprintln(out)
	if len(rows) == 0 {
		fmt.Fprintln(out, "No links.")
		return nil
	}
	return writeLinkRows(out, rows)
}

// linkRows lists links as "from -> to", with their destinations and
// states: first the links that self made, which links holds, and then the
// running links of topo that keep admits and another machine made, sorted.
// Only the machine that made a link knows its destination and whether it
// is down.
func linkRows(self wire.NodeInfo, links []wire.LinkState, topo wire.Topology, keep func(wire.TopoLink) bool) [][3]string {
	names := displayNames(topo)
	name := func(id, fallback string) string { return cmp.Or(names[id], fallback) }
	from := name(self.ID, self.Name)
	var rows, others [][3]string
	for _, l := range links {
		to := l.Destination
		if l.Peer != nil {
			to = name(l.Peer.ID, l.Peer.Name)
		}
		rows = append(rows, [3]string{from + " -> " + to, l.Destination, linkStateText(l)})
	}
	for _, e := range topo.Links {
		if e.From != self.ID && keep(e) {
			others = append(others, [3]string{name(e.From, shortID(e.From)) + " -> " + name(e.To, shortID(e.To)), "-", wire.StateRunning})
		}
	}
	slices.SortFunc(others, func(a, b [3]string) int { return strings.Compare(a[0], b[0]) })
	return append(rows, others...)
}

func writeLinkRows(out io.Writer, rows [][3]string) error {
	w := table(out)
	fmt.Fprintln(w, "LINK\tDESTINATION\tSTATE")
	for _, r := range rows {
		fmt.Fprintln(w, strings.Join(r[:], "\t"))
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
			ref, err := config.ParseNodeRef(args[0])
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
				p, err := call[wire.NetPing](cmd.Context(), b, wire.MethodNetPing, wire.NetPingParams{Node: ref, Size: size})
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
				links, linksErr := call[wire.Ping](cmd.Context(), b, wire.MethodPing, wire.NodeParams{Node: ref})
				writeLinks(a.stdout, links, linksErr)
				report, err := directReport(cmd.Context(), b, ref)
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
func directReport(ctx context.Context, b backend, ref string) (wire.DirectReport, error) {
	for {
		r, err := call[wire.DirectReport](ctx, b, wire.MethodDirect, wire.NodeParams{Node: ref})
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

// writeTrace prints a machine's latest tries, each event with the time
// since the first: the try before, which shows how it ended, and the
// latest.
func writeTrace(out io.Writer, ref string, t wire.DirectTrace, failure string) {
	fmt.Fprintf(out, "\n%s:\n", ref)
	if failure != "" {
		fmt.Fprintf(out, "  no answer: %s\n", failure)
		return
	}
	all := append(slices.Clone(t.Previous), t.Events...)
	if len(all) == 0 {
		fmt.Fprintln(out, "  no try yet")
		return
	}
	for i, e := range all {
		switch {
		case len(t.Previous) == 0:
		case i == 0:
			fmt.Fprintln(out, "  the try before:")
		case i == len(t.Previous):
			fmt.Fprintln(out, "  the latest try:")
		}
		fmt.Fprintf(out, "  %7.3fs  %s\n", e.Time.Sub(all[0].Time).Seconds(), e.Text)
	}
}

func writePing(out io.Writer, p wire.NetPing) {
	way := ""
	switch p.Way {
	case wire.WayDirect:
		way = wayText(true, nil)
		if p.Endpoint != "" { // The path may have just ended.
			way += " " + p.Endpoint
		}
		way += ", "
	case wire.WayRelay:
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

func viaText(nodes []wire.NodeInfo) string {
	if len(nodes) == 0 {
		return ""
	}
	return " via " + strings.Join(nodeNames(nodes), ", ")
}

func nodeNames(nodes []wire.NodeInfo) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
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
