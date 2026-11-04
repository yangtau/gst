package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

// migrate rewrites a configuration file of an earlier gst in the current
// format, says what it could not keep, and applies the result.
func (a *app) migrate(cmd *cobra.Command) error {
	if !needsConfig(cmd) {
		return nil
	}
	path, err := a.configPath()
	if err != nil {
		return err
	}
	upgraded, notes, err := config.Upgrade(path)
	if err != nil || !upgraded {
		// A broken configuration is reported by the command itself.
		return nil
	}
	fmt.Fprintln(a.stderr, "note: gst 0.3 replaced tunnels with links and their -L forwards with maps (see gst link list and gst map list)")
	for _, note := range notes {
		fmt.Fprintf(a.stderr, "note: dropped %s; recreate it with gst map on the other machine, or with gst route\n", note)
	}
	if err := a.sync(cmd.Context()); err != nil {
		return withHint(fmt.Errorf("apply the upgraded configuration: %w", err), "retry with `gst up`")
	}
	return nil
}

// standalone marks a command that does not use the configuration, such as
// version and the daemon, so that it skips migration.
var standalone = map[string]string{"gst.standalone": "true"}

// needsConfig reports whether cmd uses the configuration, as opposed to
// cobra's help and completion and the standalone commands.
func needsConfig(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "help", "completion", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
			return false
		}
		if c.Annotations["gst.standalone"] != "" {
			return false
		}
	}
	return cmd.HasParent()
}
