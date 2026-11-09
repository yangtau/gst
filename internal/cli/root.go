package cli

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func (a *app) rootCommand() *cobra.Command {
	// Help lists commands in the order they are added: the order of use.
	cobra.EnableCommandSorting = false
	root := asGroup(&cobra.Command{
		Use:   "gst",
		Short: "Link your machines into a private network over SSH",
		Long: `gst links your machines into a private network over SSH.

Every machine gets a gst IP and the name <machine>.gst, which any program
can use with any protocol. You link machines with SSH, and linked machines
tell each other what they reach, so every machine reaches every other.
WireGuard encrypts the traffic end to end. It goes directly over UDP where
the network allows, and is relayed along the SSH links where not.

Getting started:
  1. Run gst up on each machine. It asks before it uses sudo.
  2. Run gst link <destination> on one machine to link it to another.
  3. Reach the other machine as <machine>.gst; see them all with gst status.

Errors go to stderr, usually with a hint. Exit status is 0 on success,
1 when the work failed, and 2 for invalid arguments.`,
		Example: `  gst up
  gst link devbox
  gst status
  ping devbox.gst`,
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

	root.AddCommand(
		a.upCommand(), a.linkCommand(), a.statusCommand(), a.pingCommand(),
		a.exposeCommand(), a.unexposeCommand(), a.unlinkCommand(),
		a.nameCommand(), a.subnetCommand(), a.uiCommand(), a.downCommand(), a.versionCommand(),
	)
	root.AddCommand(a.daemonRunCommand(), a.peerBridgeCommand(), a.installCommand(), a.uninstallCommand(), a.netHelperCommand(), a.helperVersionCommand())
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

// linksHint adds this machine's links to err.
func (a *app) linksHint(err error) error {
	f, loadErr := a.load()
	if loadErr != nil {
		return err
	}
	return linksHint(f, err)
}

func quoteAll(values []string, sep string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = strconv.Quote(v)
	}
	return strings.Join(quoted, sep)
}

// completeLinks completes the destinations of this machine's links.
func (a *app) completeLinks(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	f, err := a.load()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return destinations(f), cobra.ShellCompDirectiveNoFileComp
}
