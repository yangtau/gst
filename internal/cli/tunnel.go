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
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/mesh"
	"github.com/yangtau/ssh-tunnel/internal/tunnel"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func (a *app) startCommand() *cobra.Command {
	return a.lifecycleCommand("start", "Start tunnels in the background",
		"Start stopped tunnels in the background. Starting a running tunnel does\nnothing.",
		"  gst start myhost\n  gst start --all", a.startTunnels)
}

func (a *app) stopCommand() *cobra.Command {
	return a.lifecycleCommand("stop", "Stop tunnels but keep them configured",
		"Stop tunnels, so they no longer run in the background or start at login.\nThe tunnels stay configured; start them again with gst start.",
		"  gst stop myhost\n  gst stop --all", a.stopTunnels)
}

func (a *app) restartCommand() *cobra.Command {
	return a.lifecycleCommand("restart", "Restart running tunnels",
		"Restart started tunnels, for example after upgrading gst or changing\n~/.ssh/config.",
		"  gst restart myhost\n  gst restart --all", a.restartTunnels)
}

func (a *app) addCommand() *cobra.Command {
	var settings settingsFlags
	var forwards forwardFlags
	cmd := &cobra.Command{
		Use:   "add <name> [destination]",
		Short: "Add a tunnel and start it in the background",
		Long: `Add a tunnel and start it in the background.

<name> uses 1-32 lowercase letters, digits, and '-'. <destination> is an
ssh destination such as host, user@host, or ssh://user@host:port, and
defaults to <name>. It is resolved with "ssh -G" every time the tunnel
starts, so the tunnel follows your ssh configuration: HostName, Port,
User, IdentityFile, known_hosts files, ConnectTimeout, ServerAliveInterval,
and ServerAliveCountMax. Flags override these values. ProxyJump,
ProxyCommand, and HostKeyAlias are not supported.

Before saving, gst checks that an identity without a passphrase exists
and that known_hosts records the server; it never accepts unknown host
keys. Forwards are optional and can be added later with gst forward add.

With --via mesh, <destination> is a machine of gst mesh nodes instead of
an ssh destination, and the tunnel needs no SSH settings: the daemon
listens on this machine and connects each forward's connections through
the mesh, from that machine, so targets such as localhost are that
machine's. It needs gst mesh enable and supports -L and -D only.

As in ssh, a forward without bind_address listens on loopback only;
use "*" or an empty bind_address for all interfaces.`,
		Example: `  # Use the "myhost" entry of ~/.ssh/config; reach this machine's SSH from the server's port 2222
  gst add myhost -R 2222:localhost:22

  # Reach a database next to the server on local port 15432
  gst add db admin@db.example.com -L 15432:127.0.0.1:5432

  # Expose a local web app on all interfaces of the server
  gst add web ssh://deploy@203.0.113.7:2200 -R '*:8080:localhost:3000'

  # Run a SOCKS5 proxy on local port 1080 whose connections leave from the server
  gst add proxy myhost -D 1080

  # Run a SOCKS5 proxy on server port 1080 whose connections leave from this machine
  gst add reverse-proxy myhost -R 1080

  # Reach port 5432 of the mesh machine devcn, through whichever gst links lead there
  gst add devcn-db devcn --via mesh -L 15432:localhost:5432`,
		Args: a.args(1, 2),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := config.ValidateName(name); err != nil {
				return &usageError{err}
			}
			t := &config.Tunnel{Destination: name, Forwards: []config.Forward{}}
			if len(args) == 2 {
				t.Destination = args[1]
			}
			if err := config.ValidateDestination(t.Destination); err != nil {
				return &usageError{err}
			}
			if _, err := settings.apply(cmd, t); err != nil {
				return err
			}
			fwds, err := forwards.parse(false)
			if err != nil {
				return err
			}
			for _, fwd := range fwds {
				if _, err := addForward(name, t, fwd); err != nil {
					return &usageError{err}
				}
			}
			if err := a.preflight(cmd.Context(), t); err != nil {
				return err
			}
			if _, err := a.daemonCommand(); err != nil {
				return err
			}
			err = a.update(func(f *config.File) error {
				if f.Tunnels[name] != nil {
					return withHint(fmt.Errorf("tunnel %s already exists", name),
						fmt.Sprintf("change it with `gst set %s` or `gst forward add %s`, or remove it with `gst remove %s`", name, name, name))
				}
				if t.Via == config.ViaMesh && f.Mesh == nil {
					return withHint(errors.New("a tunnel through the mesh needs the mesh"), "enable it with `gst mesh enable`")
				}
				f.Tunnels[name] = t
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return withHint(fmt.Errorf("tunnel %s was added but did not start: %w", name, err), "retry with `gst start "+name+"`")
			}
			fmt.Fprintf(a.stdout, "Added tunnel %s and started it in the background.\n", name)
			for _, fwd := range t.Forwards {
				fmt.Fprintf(a.stdout, "  %s\n", fwd.Spec())
			}
			return nil
		}),
	}
	settings.register(cmd.Flags(), false)
	forwards.register(cmd.Flags(), false)
	return cmd
}

func (a *app) setCommand() *cobra.Command {
	var settings settingsFlags
	var destination string
	var noRestart bool
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Change a tunnel's settings",
		Long: `Change a tunnel's destination or the settings that override your ssh
configuration. Only the given flags change. A running tunnel restarts to
apply the change unless --no-restart is given. Use gst forward to change
forwards.`,
		Example: `  gst set myhost --identity ~/.ssh/gst_ed25519
  gst set myhost --identity ''           # use IdentityFile from ssh config again
  gst set myhost --destination user@myhost.example.com --keepalive-interval 30s`,
		Args:              a.args(1, 1),
		ValidArgsFunction: a.completeTunnels,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			name := args[0]
			unchanged := false
			err := a.update(func(f *config.File) error {
				t, err := lookup(f, name)
				if err != nil {
					return err
				}
				before := *t
				changed, err := settings.apply(cmd, t)
				if err != nil {
					return err
				}
				if cmd.Flags().Changed("destination") {
					if err := config.ValidateDestination(destination); err != nil {
						return &usageError{err}
					}
					t.Destination, changed = destination, true
				}
				if !changed {
					return usage("nothing to change; give at least one setting flag")
				}
				if reflect.DeepEqual(before, *t) {
					unchanged = true
					return nil
				}
				return a.preflight(cmd.Context(), t)
			})
			if err != nil {
				return err
			}
			if unchanged {
				fmt.Fprintf(a.stdout, "Tunnel %s already has these settings; nothing changed.\n", name)
				return nil
			}
			fmt.Fprintf(a.stdout, "Updated tunnel %s.\n", name)
			return a.applyChange(cmd.Context(), name, noRestart)
		}),
	}
	cmd.Flags().StringVar(&destination, "destination", "", "ssh destination, such as host, user@host, or ssh://user@host:port")
	settings.register(cmd.Flags(), true)
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "do not restart a running tunnel")
	return cmd
}

// lifecycleCommand builds start, stop, and restart, which act on the named
// tunnels or, with --all, on every tunnel the action applies to.
func (a *app) lifecycleCommand(use, short, long, example string, action func(ctx context.Context, out io.Writer, f *config.File, names []string, all bool) error) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:               use + " <name>... | --all",
		Short:             short,
		Long:              long,
		Example:           example,
		ValidArgsFunction: a.completeTunnels,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			if all && len(args) > 0 {
				return usage("give tunnel names or --all, not both")
			}
			if !all && len(args) == 0 {
				return a.tunnelsHint(usage("missing argument <name> or --all"))
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
	cmd.Flags().BoolVar(&all, "all", false, "act on every tunnel")
	return cmd
}

// joinErrors returns nil, the only error, or all of errs.
func joinErrors(errs []error) error {
	if len(errs) == 1 {
		return errs[0]
	}
	return errors.Join(errs...)
}

// setStopped records whether the named tunnels are stopped.
func (a *app) setStopped(names []string, stopped bool) error {
	if len(names) == 0 {
		return nil
	}
	return a.update(func(f *config.File) error {
		for _, name := range names {
			if t := f.Tunnels[name]; t != nil {
				t.Stopped = stopped
			}
		}
		return nil
	})
}

func (a *app) startTunnels(ctx context.Context, out io.Writer, f *config.File, names []string, all bool) error {
	var errs []error
	var started []string
	for _, name := range names {
		t := f.Tunnels[name]
		if !t.Stopped {
			if !all {
				fmt.Fprintf(out, "Tunnel %s is already started.\n", name)
			}
			continue
		}
		if err := a.preflight(ctx, t); err != nil {
			errs = append(errs, fmt.Errorf("start %s: %w", name, err))
			continue
		}
		started = append(started, name)
	}
	if err := a.setStopped(started, false); err != nil {
		return err
	}
	// Syncing also brings up a daemon that is not running.
	if err := a.sync(ctx); err != nil {
		return joinErrors(append(errs, fmt.Errorf("start: %w", err)))
	}
	for _, name := range started {
		fmt.Fprintf(out, "Started tunnel %s.\n", name)
	}
	return joinErrors(errs)
}

func (a *app) stopTunnels(ctx context.Context, out io.Writer, f *config.File, names []string, all bool) error {
	var stopped []string
	for _, name := range names {
		if f.Tunnels[name].Stopped {
			if !all {
				fmt.Fprintf(out, "Tunnel %s is already stopped.\n", name)
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
		fmt.Fprintf(out, "Stopped tunnel %s.\n", name)
	}
	return nil
}

func (a *app) restartTunnels(ctx context.Context, out io.Writer, f *config.File, names []string, all bool) error {
	var errs []error
	var restart []string
	for _, name := range names {
		t := f.Tunnels[name]
		if t.Stopped {
			if !all {
				errs = append(errs, withHint(fmt.Errorf("tunnel %s is stopped", name), "start it with `gst start "+name+"`"))
			}
			continue
		}
		if err := a.preflight(ctx, t); err != nil {
			errs = append(errs, fmt.Errorf("restart %s: %w", name, err))
			continue
		}
		restart = append(restart, name)
	}
	if len(restart) > 0 {
		if err := a.restart(ctx, restart); err != nil {
			return joinErrors(append(errs, fmt.Errorf("restart: %w", err)))
		}
	}
	for _, name := range restart {
		fmt.Fprintf(out, "Restarted tunnel %s.\n", name)
	}
	return joinErrors(errs)
}

func (a *app) removeCommand() *cobra.Command {
	return &cobra.Command{
		Use:               "remove <name>...",
		Aliases:           []string{"rm"},
		Short:             "Stop tunnels and delete them",
		Long:              "Stop tunnels and delete them, including their forwards and settings.",
		Example:           "  gst remove myhost\n  gst remove myhost db",
		Args:              a.args(1, -1),
		ValidArgsFunction: a.completeTunnels,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			f, err := a.load()
			if err != nil {
				return err
			}
			for _, name := range args {
				if _, err := lookup(f, name); err != nil {
					return err
				}
			}
			err = a.update(func(f *config.File) error {
				for _, name := range args {
					delete(f.Tunnels, name)
				}
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return withHint(fmt.Errorf("tunnels were removed from the configuration, but the daemon may still run them: %w", err), "retry with `gst stop --all` or `gst restart --all`")
			}
			b, err := a.backend()
			if err != nil {
				return err
			}
			for _, name := range args {
				if err := b.RemoveLogs(name); err != nil {
					return err
				}
			}
			for _, name := range args {
				fmt.Fprintf(a.stdout, "Removed tunnel %s.\n", name)
			}
			return nil
		}),
	}
}

func (a *app) listCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List tunnels and their status",
		Long: `List tunnels and their status: running (connected, with every forward
listening), connecting, down (failed and retrying; see gst logs and
gst check), or stopped.`,
		Args: a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			f, err := a.load()
			if err != nil {
				return err
			}
			views := make([]tunnelView, 0, len(f.Tunnels))
			for _, name := range sortedNames(f) {
				views = append(views, a.view(cmd.Context(), name, f.Tunnels[name]))
			}
			if asJSON {
				return writeJSON(a.stdout, views)
			}
			if len(views) == 0 {
				fmt.Fprintln(a.stdout, "No tunnels. Add one with: gst add <name> [destination]")
				return nil
			}
			w := table(a.stdout)
			fmt.Fprintln(w, "NAME\tSTATUS\tDESTINATION\tFORWARDS")
			for _, v := range views {
				specs := make([]string, len(v.Forwards))
				for i, fwd := range v.Forwards {
					specs[i] = fwd.Spec
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", v.Name, v.Status, v.Destination, strings.Join(specs, ", "))
			}
			return w.Flush()
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func (a *app) showCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a tunnel's settings and resolved destination",
		Example: `  gst show myhost
  gst show myhost --json`,
		Args:              a.args(1, 1),
		ValidArgsFunction: a.completeTunnels,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			name := args[0]
			f, err := a.load()
			if err != nil {
				return err
			}
			t, err := lookup(f, name)
			if err != nil {
				return err
			}
			v := a.view(cmd.Context(), name, t)
			if t.Via == "" {
				if opts, err := a.options(cmd.Context(), t); err != nil {
					v.ResolveError = err.Error()
				} else {
					v.Resolved = resolvedView(opts)
				}
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

func (a *app) logsCommand() *cobra.Command {
	var follow bool
	var lines int
	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Print a tunnel's log",
		Long: `Print the last lines of a tunnel's log. The log records connection
failures and reconnects but, for privacy, no addresses; run gst check
for details.`,
		Example: `  gst logs myhost
  gst logs myhost -f`,
		Args:              a.args(1, 1),
		ValidArgsFunction: a.completeTunnels,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if lines < 1 {
				return usage("--lines must be at least 1")
			}
			f, err := a.load()
			if err != nil {
				return err
			}
			if _, err := lookup(f, name); err != nil {
				return err
			}
			b, err := a.backend()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return b.Logs(ctx, name, lines, follow, a.stdout)
		}),
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new lines")
	cmd.Flags().IntVarP(&lines, "lines", "n", 50, "number of lines to print")
	return cmd
}

func (a *app) checkSSH(ctx context.Context, t *config.Tunnel) []tunnel.Step {
	step := tunnel.Step{Name: "ssh config", OK: true}
	opts, err := a.options(ctx, t)
	if err != nil {
		step.OK = false
		step.Detail = err.Error()
		step.Hint = "check the destination and your ssh configuration with `ssh -G " + t.Destination + "`"
		return []tunnel.Step{step}
	}
	step.Detail = fmt.Sprintf("%s resolves to %s@%s", t.Destination, opts.User, opts.Server)
	return append([]tunnel.Step{step}, tunnel.Diagnose(ctx, opts, true)...)
}

// checkRoute checks that the mesh leads to a tunnel's machine.
func (a *app) checkRoute(ctx context.Context, f *config.File, t *config.Tunnel) []tunnel.Step {
	step := tunnel.Step{Name: "mesh"}
	if f.Mesh == nil {
		step.Detail = "the mesh is not enabled on this machine"
		step.Hint = "enable it with `gst mesh enable`"
		return []tunnel.Step{step}
	}
	b, err := a.backend()
	if err == nil {
		var topo wire.Topology
		if topo, err = b.Nodes(ctx); err == nil {
			step.OK = true
			step.Detail = fmt.Sprintf("this machine is %s", f.Mesh.Name)
			route := tunnel.Step{Name: "route"}
			path, err := routeTo(topo, t.Destination)
			if err != nil {
				route.Detail = err.Error()
				route.Hint = "see the machines and links with `gst mesh nodes`"
			} else {
				route.OK = true
				route.Detail = path
			}
			return []tunnel.Step{step, route}
		}
	}
	step.Detail = fmt.Sprintf("the gst daemon does not answer: %v", err)
	step.Hint = "start it with `gst mesh enable`"
	return []tunnel.Step{step}
}

// routeTo describes the route to a machine as names joined by arrows.
func routeTo(topo wire.Topology, ref string) (string, error) {
	node, err := mesh.FindNode(topo, ref)
	if err != nil {
		return "", err
	}
	path, err := mesh.Route(topo, node.ID)
	if err != nil {
		return "", err
	}
	names := map[string]string{}
	for _, n := range topo.Nodes {
		names[n.ID] = n.Name
	}
	for i, id := range path {
		path[i] = names[id]
	}
	return strings.Join(path, " → "), nil
}

func (a *app) checkCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "check <name>",
		Short: "Diagnose a tunnel's connection",
		Long: `Check step by step that a tunnel can connect: its destination resolves
with ssh, an identity is usable, known_hosts records the server, the
server is reachable, and the login succeeds. For a tunnel through the
mesh, check that the mesh has a route to its machine. Each failure comes
with a hint. Exit status is 1 if a step fails.`,
		Example:           "  gst check myhost",
		Args:              a.args(1, 1),
		ValidArgsFunction: a.completeTunnels,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			f, err := a.load()
			if err != nil {
				return err
			}
			t, err := lookup(f, args[0])
			if err != nil {
				return err
			}
			var steps []tunnel.Step
			if t.Via == config.ViaMesh {
				steps = a.checkRoute(cmd.Context(), f, t)
			} else {
				steps = a.checkSSH(cmd.Context(), t)
			}
			ok := steps[len(steps)-1].OK
			if asJSON {
				if err := writeJSON(a.stdout, struct {
					OK    bool          `json:"ok"`
					Steps []tunnel.Step `json:"steps"`
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
