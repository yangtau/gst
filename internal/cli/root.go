package cli

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

const version = "0.3.0"

func (a *app) rootCommand() *cobra.Command {
	// Help lists commands in the order they are added, grouped by task.
	cobra.EnableCommandSorting = false
	root := asGroup(&cobra.Command{
		Use:   "gst",
		Short: "Link your machines into a private mesh over SSH",
		Long: `gst links your machines into a private mesh over SSH, without root.

Each machine runs a gst daemon in the background. A link is an SSH
connection from one machine to another that also has gst; linked
machines pass on what they know, so every machine can reach every other
one, through the links between them. Reach them by:

  connect  gst connect <machine> <port>, also as an ssh ProxyCommand
  proxy    a SOCKS5 and HTTP proxy where <machine>.gst is that machine
  map      a local port or socket that leads to a machine's port or socket
  route    send the proxy's connections to some hosts out through an exit

Errors go to stderr, usually with a hint. Exit status is 0 on success,
1 when the work failed, and 2 for invalid arguments. Commands that
report state accept --json.`,
		Example: `  gst link add devcn                   # link to the ssh config host "devcn"
  gst status                           # every machine in the mesh
  gst map add 15432 devcn:5432         # local port 15432 leads to devcn's 5432
  curl http://devcn.gst:8080/          # with ALL_PROXY from gst proxy`,
		SilenceErrors: true,
		SilenceUsage:  true,
	})
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if err := a.migrate(cmd); err != nil {
			return &failure{err}
		}
		return nil
	}
	root.PersistentFlags().StringVar(&a.configFlag, "config", "", "configuration file (default: $GST_CONFIG, else ~/.config/gst/config.json)")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err} })
	root.SetUsageTemplate(strings.NewReplacer(
		// Groups run only to reject unknown subcommands; show one usage line.
		"{{if .Runnable}}\n  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}\n  {{.CommandPath}} [command]{{end}}",
		"{{if .HasAvailableSubCommands}}\n  {{.CommandPath}} <command>{{else}}\n  {{.UseLine}}{{end}}",
		"{{.CommandPath}} [command] --help", "{{.CommandPath}} <command> --help",
	).Replace(root.UsageTemplate()))

	root.AddGroup(
		&cobra.Group{ID: "mesh", Title: "Build the mesh:"},
		&cobra.Group{ID: "reach", Title: "Reach machines:"},
		&cobra.Group{ID: "machine", Title: "This machine:"},
	)
	addGroup := func(id string, cmds ...*cobra.Command) {
		for _, cmd := range cmds {
			cmd.GroupID = id
			root.AddCommand(cmd)
		}
	}
	addGroup("mesh", a.linkCommand(), a.statusCommand(), a.pingCommand())
	addGroup("reach", a.connectCommand(), a.proxyCommand(), a.mapCommand(), a.routeCommand())
	addGroup("machine", a.nameCommand(), a.exitCommand(), a.upCommand(), a.downCommand(), a.uiCommand(), &cobra.Command{
		Use:         "version",
		Annotations: standalone,
		Short:       "Show the gst version",
		Args:        a.args(0, 0),
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(a.stdout, "gst version", version)
		},
	})
	root.AddCommand(a.daemonRunCommand(), a.peerBridgeCommand())
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
				return a.linksHint(err)
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
	return placeholderPattern.FindAllString(cmd.Use, -1)
}

var placeholderPattern = regexp.MustCompile(`<[^>]*>([^\s<]*<[^>]*>)*(\.\.\.)?`)

// linksHint adds the existing link names to err.
func (a *app) linksHint(err error) error {
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

// completeLinks completes link names.
func (a *app) completeLinks(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	f, err := a.load()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return sortedNames(f), cobra.ShellCompDirectiveNoFileComp
}
