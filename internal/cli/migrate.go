package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

// migrate moves a configuration from gst 0.1, where each started tunnel
// had its own background service, to the daemon: tunnels with a service
// stay started and move to the daemon; the others are marked stopped.
// Services that an interrupted migration left behind are removed later.
func (a *app) migrate(cmd *cobra.Command) error {
	if !needsConfig(cmd) {
		return nil
	}
	f, err := a.load()
	if err != nil {
		// A broken configuration is reported by the command itself.
		return nil
	}
	b, err := a.backend()
	if err != nil {
		return err
	}
	if f.Version < config.Version {
		err = a.update(func(f *config.File) error {
			if f.Version >= config.Version {
				return nil
			}
			for _, name := range sortedNames(f) {
				if !b.HasLegacy(name) {
					f.Tunnels[name].Stopped = true
				}
			}
			f.Version = config.Version
			return nil
		})
		if err != nil {
			return err
		}
	}
	var moved []string
	for _, name := range sortedNames(f) {
		if b.HasLegacy(name) {
			moved = append(moved, name)
		}
	}
	for _, name := range moved {
		if err := b.RemoveLegacy(name); err != nil {
			return fmt.Errorf("remove the gst 0.1 service of tunnel %s: %w", name, err)
		}
	}
	if len(moved) == 0 {
		return nil
	}
	if err := a.sync(cmd.Context()); err != nil {
		return withHint(fmt.Errorf("start the gst daemon: %w", err), "retry with `gst restart --all`")
	}
	fmt.Fprintf(a.stderr, "note: the gst daemon now runs tunnels %s, which ran as separate services before gst 0.2.\n", strings.Join(moved, ", "))
	return nil
}

// standalone marks a command that does not work with the configured
// tunnels, such as version and the commands that the background runs, so
// that it skips migration.
var standalone = map[string]string{"gst.standalone": "true"}

// needsConfig reports whether cmd works with the configured tunnels, as
// opposed to cobra's help and completion and the standalone commands.
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
