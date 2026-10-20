package cli

import (
	"errors"
	"runtime/debug"

	"github.com/spf13/cobra"
)

func (a *app) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "gst",
		Short: "Keep SSH port forwards running in the background",
		Long: `gst keeps SSH port forwards running in the background.

A tunnel is a named SSH connection, resolved from your ssh configuration,
with forwards in either direction:

  -R  listen on the server and connect from this machine, like ssh -R
  -L  listen on this machine and connect from the server, like ssh -L

Every started tunnel runs as a background service (a systemd user unit on
Linux, a launchd agent on macOS) that starts at login and reconnects after
failures. gst stores tunnels in ~/.config/gst/config.json; use gst commands
instead of editing it.

Commands that change tunnels print what they did. Errors go to stderr,
often with a hint. Exit status is 0 on success, 1 when the work failed,
and 2 for invalid arguments. Commands that report state accept --json.`,
		Example: `  gst tunnel add myhost -R 2222:localhost:22       # start a tunnel to the ssh config host "myhost"
  gst forward add myhost -L 15432:127.0.0.1:5432   # add a forward; the tunnel restarts
  gst tunnel list                                  # show tunnels and their status
  gst tunnel check myhost                          # diagnose connection problems
  gst tunnel stop myhost                           # stop the tunnel and its service`,
		Version:       version(),
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.PersistentFlags().StringVar(&a.configFlag, "config", "", "configuration file (default: $GST_CONFIG, else ~/.config/gst/config.json)")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err} })
	root.AddCommand(a.tunnelCommand(), a.forwardCommand(), a.runCommand())
	return root
}

// group returns a command that only holds subcommands. Unlike cobra's
// default, an unknown subcommand is an error rather than a help page.
func group(use, short string, subcommands ...*cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:                        use,
		Short:                      short,
		Args:                       cobra.ArbitraryArgs,
		DisableFlagsInUseLine:      true,
		SuggestionsMinimumDistance: 2,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
				return usage("unknown command %q for %q; did you mean %q?", args[0], cmd.CommandPath(), suggestions[0])
			}
			return usage("unknown command %q for %q", args[0], cmd.CommandPath())
		},
	}
	cmd.AddCommand(subcommands...)
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

// completeTunnels completes tunnel names.
func (a *app) completeTunnels(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	f, err := a.load()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return sortedNames(f), cobra.ShellCompDirectiveNoFileComp
}

func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 12 {
			return "devel-" + s.Value[:12]
		}
	}
	return "devel"
}
