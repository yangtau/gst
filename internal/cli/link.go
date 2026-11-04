package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"reflect"
	"slices"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/sshconn"
)

func (a *app) linkCommand() *cobra.Command {
	cmd := asGroup(&cobra.Command{
		Use:   "link",
		Short: "Link this machine with other machines over SSH",
		Long: `A link is an SSH connection from this machine to another machine that
also runs gst. Over it, the gst daemons of the two machines join each
other's mesh: each learns every machine that the other knows, and
connections can pass through it in both directions. A started link runs
in the background, starts at login, and reconnects after failures.

The other machine needs gst 0.3 or later in the PATH of its login shell,
or set with --remote-gst. It does not need its own links: being linked
to is enough.`,
	})
	cmd.AddCommand(
		a.linkAddCommand(), a.linkSetCommand(), a.linkRemoveCommand(),
		a.linkActionCommand("start", "Start stopped links",
			"Start stopped links. Starting a running link does nothing.", a.startLinks),
		a.linkActionCommand("stop", "Stop links but keep them configured",
			"Stop links, so they no longer run in the background or start at login.\nThe links stay configured; start them again with gst link start.", a.stopLinks),
		a.linkActionCommand("restart", "Restart running links",
			"Restart started links, for example after changing ~/.ssh/config.", a.restartLinks),
		a.linkListCommand(), a.linkShowCommand(), a.linkLogsCommand(), a.linkCheckCommand(),
	)
	return cmd
}

func (a *app) linkAddCommand() *cobra.Command {
	var settings settingsFlags
	cmd := &cobra.Command{
		Use:   "add <name> [destination]",
		Short: "Add a link and start it",
		Long: `Add a link and start it in the background.

<name> uses 1-32 lowercase letters, digits, and '-', and also names the
machine at the other end until it reports its own name. <destination> is
an ssh destination such as host, user@host, or ssh://user@host:port, and
defaults to <name>. It is resolved with "ssh -G" every time the link
connects, so the link follows your ssh configuration: HostName, Port,
User, IdentityFile, known_hosts files, ConnectTimeout, ServerAliveInterval,
and ServerAliveCountMax. Flags override these values. ProxyJump,
ProxyCommand, and HostKeyAlias are not supported.

Before saving, gst checks that an identity without a passphrase exists
and that known_hosts records the server; it never accepts unknown host
keys. Run gst link check to test the connection itself.`,
		Example: `  gst link add devcn                          # the "devcn" entry of ~/.ssh/config
  gst link add db admin@db.example.com
  gst link add web ssh://deploy@203.0.113.7:2200 --remote-gst ~/bin/gst`,
		Args: a.args(1, 2),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := config.ValidateName(name); err != nil {
				return &usageError{err}
			}
			l := &config.Link{Destination: name}
			if len(args) == 2 {
				l.Destination = args[1]
			}
			if err := config.ValidateDestination(l.Destination); err != nil {
				return &usageError{err}
			}
			if _, err := settings.apply(cmd, l); err != nil {
				return err
			}
			if _, err := a.daemonCommand(); err != nil {
				return err
			}
			err := a.update(func(f *config.File) error {
				if f.Links[name] != nil {
					return withHint(fmt.Errorf("link %s already exists", name),
						fmt.Sprintf("change it with `gst link set %s`, or remove it with `gst link remove %s`", name, name))
				}
				if err := a.preflight(cmd.Context(), l); err != nil {
					return err
				}
				f.Links[name] = l
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return withHint(fmt.Errorf("link %s was added but did not start: %w", name, err), "retry with `gst link start "+name+"`")
			}
			fmt.Fprintf(a.stdout, "Added link %s; it connects in the background. See it with: gst link list\n", name)
			return nil
		}),
	}
	settings.register(cmd.Flags(), false)
	return cmd
}

func (a *app) linkSetCommand() *cobra.Command {
	var settings settingsFlags
	var destination string
	var noRestart bool
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Change a link's settings",
		Long: `Change a link's destination or the settings that override your ssh
configuration. Only the given flags change. A running link restarts to
apply the change unless --no-restart is given.`,
		Example: `  gst link set devcn --identity ~/.ssh/gst_ed25519
  gst link set devcn --identity ''           # use IdentityFile from ssh config again
  gst link set devcn --destination user@devcn.example.com --keepalive-interval 30s`,
		Args:              a.args(1, 1),
		ValidArgsFunction: a.completeLinks,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			name := args[0]
			unchanged := false
			err := a.update(func(f *config.File) error {
				l, err := lookup(f, name)
				if err != nil {
					return err
				}
				before := *l
				changed, err := settings.apply(cmd, l)
				if err != nil {
					return err
				}
				if cmd.Flags().Changed("destination") {
					if err := config.ValidateDestination(destination); err != nil {
						return &usageError{err}
					}
					l.Destination, changed = destination, true
				}
				if !changed {
					return usage("nothing to change; give at least one setting flag")
				}
				if reflect.DeepEqual(before, *l) {
					unchanged = true
					return nil
				}
				return a.preflight(cmd.Context(), l)
			})
			if err != nil {
				return err
			}
			if unchanged {
				fmt.Fprintf(a.stdout, "Link %s already has these settings; nothing changed.\n", name)
				return nil
			}
			fmt.Fprintf(a.stdout, "Updated link %s.\n", name)
			return a.applyChange(cmd.Context(), name, noRestart)
		}),
	}
	cmd.Flags().StringVar(&destination, "destination", "", "ssh destination, such as host, user@host, or ssh://user@host:port")
	settings.register(cmd.Flags(), true)
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "do not restart a running link")
	return cmd
}

func (a *app) linkRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:               "remove <name>...",
		Aliases:           []string{"rm"},
		Short:             "Stop links and delete them",
		Example:           "  gst link remove devcn\n  gst link remove devcn db",
		Args:              a.args(1, -1),
		ValidArgsFunction: a.completeLinks,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			err := a.update(func(f *config.File) error {
				for _, name := range args {
					if _, err := lookup(f, name); err != nil {
						return err
					}
					delete(f.Links, name)
				}
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return withHint(fmt.Errorf("the links were removed from the configuration, but the daemon may still run them: %w", err), "retry with `gst up`")
			}
			b, err := a.backend()
			if err != nil {
				return err
			}
			for _, name := range args {
				if err := b.RemoveLogs(name); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Removed link %s.\n", name)
			}
			return nil
		}),
	}
}

// linkAction starts, stops, or restarts the named links, which exist, and
// writes what it did to out. With all, it skips links that the action
// does not apply to without saying so.
type linkAction func(ctx context.Context, out io.Writer, f *config.File, names []string, all bool) error

// linkActionCommand builds start, stop, and restart, which act on the named
// links or, with --all, on every link the action applies to.
func (a *app) linkActionCommand(use, short, long string, action linkAction) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:               use + " <name>... | --all",
		Short:             short,
		Long:              long,
		Example:           fmt.Sprintf("  gst link %s devcn\n  gst link %s --all", use, use),
		ValidArgsFunction: a.completeLinks,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			if all && len(args) > 0 {
				return usage("give link names or --all, not both")
			}
			if !all && len(args) == 0 {
				return a.linksHint(usage("missing argument <name> or --all"))
			}
			f, err := a.load()
			if err != nil {
				return err
			}
			names := args
			if all {
				names = sortedNames(f)
			}
			for _, name := range names {
				if _, err := lookup(f, name); err != nil {
					return err
				}
			}
			return action(cmd.Context(), a.stdout, f, slices.Compact(slices.Sorted(slices.Values(names))), all)
		}),
	}
	cmd.Flags().BoolVar(&all, "all", false, "act on every link")
	return cmd
}

// setStopped records whether the named links are stopped.
func (a *app) setStopped(names []string, stopped bool) error {
	if len(names) == 0 {
		return nil
	}
	return a.update(func(f *config.File) error {
		for _, name := range names {
			if l := f.Links[name]; l != nil {
				l.Stopped = stopped
			}
		}
		return nil
	})
}

func (a *app) startLinks(ctx context.Context, out io.Writer, f *config.File, names []string, all bool) error {
	var errs []error
	var started []string
	for _, name := range names {
		l := f.Links[name]
		if !l.Stopped {
			if !all {
				fmt.Fprintf(out, "Link %s is already started.\n", name)
			}
			continue
		}
		if err := a.preflight(ctx, l); err != nil {
			errs = append(errs, fmt.Errorf("start %s: %w", name, err))
			continue
		}
		started = append(started, name)
	}
	if err := a.setStopped(started, false); err != nil {
		return err
	}
	if err := a.sync(ctx); err != nil {
		return errors.Join(append(errs, fmt.Errorf("start: %w", err))...)
	}
	for _, name := range started {
		fmt.Fprintf(out, "Started link %s.\n", name)
	}
	return errors.Join(errs...)
}

func (a *app) stopLinks(ctx context.Context, out io.Writer, f *config.File, names []string, all bool) error {
	var stopped []string
	for _, name := range names {
		if f.Links[name].Stopped {
			if !all {
				fmt.Fprintf(out, "Link %s is already stopped.\n", name)
			}
			continue
		}
		stopped = append(stopped, name)
	}
	if err := a.setStopped(stopped, true); err != nil {
		return err
	}
	if err := a.sync(ctx); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	for _, name := range stopped {
		fmt.Fprintf(out, "Stopped link %s.\n", name)
	}
	return nil
}

func (a *app) restartLinks(ctx context.Context, out io.Writer, f *config.File, names []string, all bool) error {
	var errs []error
	var restart []string
	for _, name := range names {
		l := f.Links[name]
		if l.Stopped {
			if !all {
				errs = append(errs, withHint(fmt.Errorf("link %s is stopped", name), "start it with `gst link start "+name+"`"))
			}
			continue
		}
		if err := a.preflight(ctx, l); err != nil {
			errs = append(errs, fmt.Errorf("restart %s: %w", name, err))
			continue
		}
		restart = append(restart, name)
	}
	if len(restart) > 0 {
		if err := a.restart(ctx, restart); err != nil {
			return errors.Join(append(errs, fmt.Errorf("restart: %w", err))...)
		}
	}
	for _, name := range restart {
		fmt.Fprintf(out, "Restarted link %s.\n", name)
	}
	return errors.Join(errs...)
}

func (a *app) linkListCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List links and their status",
		Long: `List links and their status: running (the two gst daemons are linked),
connecting, down (failed and retrying; see gst link check), or stopped.
PEER is the machine at the other end.`,
		Args: a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			f, err := a.load()
			if err != nil {
				return err
			}
			st := a.status(cmd.Context())
			views := make([]linkView, 0, len(f.Links))
			for _, name := range sortedNames(f) {
				views = append(views, newLinkView(name, f.Links[name], st))
			}
			if asJSON {
				return writeJSON(a.stdout, views)
			}
			if len(views) == 0 {
				fmt.Fprintln(a.stdout, "No links. Add one with: gst link add <name> [destination]")
				return nil
			}
			w := table(a.stdout)
			fmt.Fprintln(w, "NAME\tSTATUS\tDESTINATION\tPEER")
			for _, v := range views {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", v.Name, v.statusText(), v.Destination, v.peerText())
			}
			return w.Flush()
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func (a *app) linkShowCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:               "show <name>",
		Short:             "Show a link's settings and resolved destination",
		Example:           "  gst link show devcn\n  gst link show devcn --json",
		Args:              a.args(1, 1),
		ValidArgsFunction: a.completeLinks,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			f, err := a.load()
			if err != nil {
				return err
			}
			l, err := lookup(f, args[0])
			if err != nil {
				return err
			}
			v := newLinkView(args[0], l, a.status(cmd.Context()))
			if opts, err := a.options(cmd.Context(), l); err != nil {
				v.ResolveError = err.Error()
			} else {
				v.Resolved = resolvedView(opts)
			}
			if asJSON {
				return writeJSON(a.stdout, v)
			}
			return v.write(a.stdout)
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func (a *app) linkLogsCommand() *cobra.Command {
	var follow bool
	var lines int
	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Print a link's log",
		Long: `Print the last lines of a link's log. The log records connection
failures and reconnects but, for privacy, no addresses and no errors
that the other machine reports; gst link list shows those, and gst link
check finds the cause.`,
		Example:           "  gst link logs devcn\n  gst link logs devcn -f",
		Args:              a.args(1, 1),
		ValidArgsFunction: a.completeLinks,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			if lines < 1 {
				return usage("--lines must be at least 1")
			}
			f, err := a.load()
			if err != nil {
				return err
			}
			if _, err := lookup(f, args[0]); err != nil {
				return err
			}
			b, err := a.backend()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return b.Logs(ctx, args[0], lines, follow, a.stdout)
		}),
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new lines")
	cmd.Flags().IntVarP(&lines, "lines", "n", 50, "number of lines to print")
	return cmd
}

func (a *app) linkCheckCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "check <name>",
		Short: "Diagnose a link's connection",
		Long: `Check step by step that a link can connect: its destination resolves
with ssh, an identity is usable, known_hosts records the server, the
server is reachable, the login succeeds, and gst runs there. Each failure
comes with a hint. Exit status is 1 if a step fails.`,
		Example:           "  gst link check devcn",
		Args:              a.args(1, 1),
		ValidArgsFunction: a.completeLinks,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			f, err := a.load()
			if err != nil {
				return err
			}
			l, err := lookup(f, args[0])
			if err != nil {
				return err
			}
			steps := a.check(cmd.Context(), l)
			ok := steps[len(steps)-1].OK
			if asJSON {
				if err := writeJSON(a.stdout, struct {
					OK    bool           `json:"ok"`
					Steps []sshconn.Step `json:"steps"`
				}{ok, steps}); err != nil {
					return err
				}
			} else {
				writeSteps(a.stdout, steps)
			}
			if !ok {
				return &exitError{exitFailure}
			}
			return nil
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// check diagnoses link l online, down to running gst on its server.
func (a *app) check(ctx context.Context, l *config.Link) []sshconn.Step {
	step := sshconn.Step{Name: "ssh config", OK: true}
	opts, err := a.options(ctx, l)
	if err != nil {
		step.OK = false
		step.Detail = err.Error()
		step.Hint = "check the destination and your ssh configuration with `ssh -G " + l.Destination + "`"
		return []sshconn.Step{step}
	}
	step.Detail = fmt.Sprintf("%s resolves to %s@%s", l.Destination, opts.User, opts.Server)
	return append([]sshconn.Step{step}, sshconn.Diagnose(ctx, opts, true, remoteGst(l)+" version")...)
}
