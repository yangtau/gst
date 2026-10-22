package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

const version = "0.1.0"

func (a *app) rootCommand() *cobra.Command {
	// Help lists commands in the order they are added, grouped by task.
	cobra.EnableCommandSorting = false
	root := asGroup(&cobra.Command{
		Use:   "gst",
		Short: "Keep SSH port forwards running in the background",
		Long: `gst keeps SSH port forwards running in the background.

A tunnel is a named SSH connection to a host from your ssh configuration,
with forwards in either direction:

  -R  listen on the server, connect from this machine (like ssh -R)
  -L  listen on this machine, connect from the server (like ssh -L)

A started tunnel runs in the background, starts at login, and reconnects
after failures.

Errors go to stderr, usually with a hint. Exit status is 0 on success,
1 when the work failed, and 2 for invalid arguments. Commands that
report state accept --json.`,
		Example: `  gst add myhost -R 2222:localhost:22             # tunnel to the ssh config host "myhost"
  gst forward add myhost -L 15432:127.0.0.1:5432  # add a forward; the tunnel restarts
  gst list                                        # tunnels and their status
  gst check myhost                                # diagnose connection problems`,
		SilenceErrors: true,
		SilenceUsage:  true,
	})
	root.PersistentFlags().StringVar(&a.configFlag, "config", "", "configuration file (default: $GST_CONFIG, else ~/.config/gst/config.json)")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err} })
	root.SetUsageTemplate(strings.NewReplacer(
		// Groups run only to reject unknown subcommands; show one usage line.
		"{{if .Runnable}}\n  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}\n  {{.CommandPath}} [command]{{end}}",
		"{{if .HasAvailableSubCommands}}\n  {{.CommandPath}} <command>{{else}}\n  {{.UseLine}}{{end}}",
		"{{.CommandPath}} [command] --help", "{{.CommandPath}} <command> --help",
	).Replace(root.UsageTemplate()))

	root.AddGroup(
		&cobra.Group{ID: "configure", Title: "Configure tunnels:"},
		&cobra.Group{ID: "run", Title: "Run tunnels:"},
		&cobra.Group{ID: "inspect", Title: "Inspect tunnels:"},
	)
	addGroup := func(id string, cmds ...*cobra.Command) {
		for _, cmd := range cmds {
			cmd.GroupID = id
			root.AddCommand(cmd)
		}
	}
	addGroup("configure", a.addCommand(), a.setCommand(), a.forwardCommand(), a.removeCommand())
	addGroup("run", a.startCommand(), a.stopCommand(), a.restartCommand())
	addGroup("inspect", a.listCommand(), a.showCommand(), a.logsCommand(), a.checkCommand())
	root.AddCommand(&cobra.Command{
		Use:     "version",
		Short:   "Show the gst version",
		GroupID: "inspect",
		Args:    a.args(0, 0),
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(a.stdout, "gst version", version)
		},
	}, a.runCommand())
	setHelpColors(root)
	return root
}

// asGroup makes cmd, which only holds subcommands, print its help when run
// alone. Unlike cobra's default, an unknown subcommand is an error.
func asGroup(cmd *cobra.Command) *cobra.Command {
	cmd.Args = cobra.ArbitraryArgs
	cmd.DisableFlagsInUseLine = true
	cmd.SuggestionsMinimumDistance = 2
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
			return usage("unknown command %q for %q; did you mean %s?", args[0], cmd.CommandPath(), quoteAll(suggestions, " or "))
		}
		return usage("unknown command %q for %q", args[0], cmd.CommandPath())
	}
	return cmd
}

// run wraps a command's work so that its errors exit with status 1 unless
// they are usage errors.
func run(fn func(cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		err := fn(cmd, args)
		if err == nil {
			return nil
		}
		var u *usageError
		var exit *exitError
		if errors.As(err, &u) || errors.As(err, &exit) {
			return err
		}
		return &failure{err}
	}
}

// args accepts minN to maxN positional arguments, or at least minN when
// maxN is negative, and names the missing or unexpected argument.
func (a *app) args(minN, maxN int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < minN {
			err := usage("missing argument %s", strings.TrimSuffix(placeholders(cmd)[len(args)], "..."))
			if cmd.ValidArgsFunction != nil {
				return a.tunnelsHint(err)
			}
			return err
		}
		if maxN >= 0 && len(args) > maxN {
			return usage("unexpected argument %q", args[maxN])
		}
		return nil
	}
}

// placeholders returns the <argument> names in cmd's Use line.
func placeholders(cmd *cobra.Command) []string {
	var names []string
	for _, field := range strings.Fields(cmd.Use) {
		if strings.HasPrefix(field, "<") {
			names = append(names, field)
		}
	}
	return names
}

// tunnelsHint adds the existing tunnel names to err.
func (a *app) tunnelsHint(err error) error {
	f, loadErr := a.load()
	if loadErr != nil {
		return err
	}
	return namesHint(f, err)
}

func quoteAll(values []string, sep string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = strconv.Quote(v)
	}
	return strings.Join(quoted, sep)
}

// completeTunnels completes tunnel names.
func (a *app) completeTunnels(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	f, err := a.load()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return sortedNames(f), cobra.ShellCompDirectiveNoFileComp
}
