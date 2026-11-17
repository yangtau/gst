package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func (a *app) logCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "log [machine]",
		Short: "Show the end of gst's log, on this machine or another",
		Long: `Show the latest lines of gst's log on this machine, or with a machine, on
that machine, asked through the network: when links came up and ended,
when direct paths did, and when the machine slept or moved to another
network. gst keeps the end of its log since it started, up to 128 KiB;
the log names no servers, users, or addresses.

A phone has no other way to show its log than its app, so this is how to
read it from another machine.

A machine is given by its name, or its id or an id prefix of at least 4
characters, optionally followed by .gst.`,
		Example: "  gst log\n  gst log myphone",
		Args:    a.args(0, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			b, err := a.backend()
			if err != nil {
				return err
			}
			var p wire.NodeParams
			if len(args) == 1 {
				ref, err := config.ParseNodeRef(args[0])
				if err != nil {
					return &usageError{err}
				}
				p.Node = ref
			}
			l, err := call[wire.Log](cmd.Context(), b, wire.MethodLog, p)
			switch {
			case err != nil && p.Node == "":
				return noDaemon(err)
			case err != nil:
				return withHint(err, "see the machines with `gst status`")
			}
			_, err = fmt.Fprint(a.stdout, l.Text)
			return err
		}),
	}
}
