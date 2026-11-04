package cli

import (
	"context"
	"fmt"
	"net"
	"slices"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/mesh"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func (a *app) proxyCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "proxy [address | off]",
		Short: "Show, turn on, or turn off the proxy that reaches the mesh",
		Long: `Show the proxy and how to use it, turn it on at an address, or turn it
off. The gst daemon serves a SOCKS5 and HTTP proxy on one port; it is off
until you give it an address. Through it, <machine>.gst reaches a
machine of the mesh, the hosts that routes match go out through their
exits, and other hosts are connected to directly.`,
		Example: `  gst proxy
  gst proxy 127.0.0.1:1080
  gst proxy off`,
		Args: a.args(0, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				address := ""
				if args[0] != "off" {
					var err error
					if address, err = config.ParseListen(args[0]); err != nil {
						return &usageError{err}
					}
					if config.IsSocket(address) {
						return usage("the proxy listens on a TCP port, not a socket")
					}
				}
				err := a.update(func(f *config.File) error {
					f.Proxy = address
					for _, m := range f.Maps {
						if m.Listen == address {
							return withHint(fmt.Errorf("a map listens on %s", address), "remove it with `gst map remove "+address+"`, or choose another address")
						}
					}
					return nil
				})
				if err == nil {
					err = a.sync(cmd.Context())
				}
				if err != nil {
					return err
				}
			}
			f, err := a.load()
			if err != nil {
				return err
			}
			v := newProxyView(f, a.status(cmd.Context()))
			if len(args) == 1 && v.Status == statusFailed {
				return listenError(v.Address, v.Error)
			}
			if asJSON {
				return writeJSON(a.stdout, v)
			}
			if v.Address == "" {
				fmt.Fprintln(a.stdout, "The proxy is off. Turn it on with: gst proxy 127.0.0.1:1080")
				return nil
			}
			fmt.Fprintf(a.stdout, "Proxy: %s (%s)\n\n", v.Address, statusLine(v.Status, v.Error))
			host, port, _ := net.SplitHostPort(v.Address)
			if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
				v.Address = net.JoinHostPort("127.0.0.1", port)
			}
			fmt.Fprintf(a.stdout, "export ALL_PROXY=socks5h://%[1]s http_proxy=http://%[1]s https_proxy=http://%[1]s\n", v.Address)
			return nil
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// proxyView is the proxy as gst proxy --json prints it.
type proxyView struct {
	Address string `json:"address"` // empty when off
	Status  string `json:"status,omitempty"`
	Error   string `json:"error,omitempty"`
}

// newProxyView returns f's proxy with its state in st, the daemon's
// status, which is nil when no daemon answers.
func newProxyView(f *config.File, st *wire.Status) proxyView {
	v := proxyView{Address: f.Proxy}
	if v.Address == "" {
		return v
	}
	var states []wire.Listener
	if st != nil {
		states = []wire.Listener{st.Proxy}
	}
	v.Status, v.Error = listenerStatus(v.Address, states)
	return v
}

// Listener statuses.
const (
	statusListening = "listening"
	statusFailed    = "failed"
	statusNotRun    = "not running" // the daemon does not answer
)

// listenerStatus returns the status of the listener on address among
// those that the daemon reports, which is nil when no daemon answers.
func listenerStatus(address string, states []wire.Listener) (status, lastErr string) {
	if states == nil {
		return statusNotRun, ""
	}
	for _, s := range states {
		if s.Listen == address {
			if s.Error != "" {
				return statusFailed, s.Error
			}
			return statusListening, ""
		}
	}
	return statusNotRun, ""
}

func statusLine(status, lastErr string) string {
	switch {
	case lastErr != "":
		return status + ": " + lastErr
	case status == statusNotRun:
		return "gst is not running; start it with gst up"
	}
	return status
}

// checkListening fails when the daemon reports that it cannot listen on
// address, after a change to the configuration.
func (a *app) checkListening(ctx context.Context, address string) error {
	st := a.status(ctx)
	if st == nil {
		return nil
	}
	if status, lastErr := listenerStatus(address, mapListeners(st)); status == statusFailed {
		return listenError(address, lastErr)
	}
	return nil
}

func listenError(address, reason string) error {
	return withHint(fmt.Errorf("cannot listen on %s: %s", address, reason), "choose another address, or free this one and run `gst up`")
}

func (a *app) mapCommand() *cobra.Command {
	cmd := asGroup(&cobra.Command{
		Use:   "map",
		Short: "Map local ports and sockets to machines",
		Long: `A map listens on a port or Unix socket of this machine and connects each
connection to a port or socket of a machine in the mesh, for programs
that cannot use the proxy. Ports listen on 127.0.0.1 unless an address
is given.`,
	})
	cmd.AddCommand(a.mapAddCommand(), a.mapRemoveCommand(), a.mapListCommand())
	return cmd
}

func (a *app) mapAddCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "add <listen> <machine>:<target>",
		Short: "Map a local port or socket to a machine",
		Long: `Map a local port or socket to a machine. <listen> is a port, which
listens on 127.0.0.1, an address:port, or an absolute socket path.
<target> is a port, which means localhost on that machine, a host:port,
or an absolute socket path there. A host:port other than that machine's
own needs the machine to be an exit.`,
		Example: `  gst map add 15432 devcn:5432
  gst map add 0.0.0.0:8080 devcn:8080
  gst map add 18082 devcn:/run/user/1000/app.sock
  gst map add /tmp/devcn-docker.sock devcn:/var/run/docker.sock`,
		Args: a.args(2, 2),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			listen, err := config.ParseListen(args[0])
			if err != nil {
				return &usageError{err}
			}
			machine, target, err := config.ParseMachineTarget(args[1])
			if err != nil {
				return &usageError{err}
			}
			err = a.update(func(f *config.File) error {
				if listen == f.Proxy {
					return withHint(fmt.Errorf("the proxy listens on %s", listen), "choose another address, or move the proxy with `gst proxy <address>`")
				}
				for _, m := range f.Maps {
					if m.Listen == listen {
						return withHint(fmt.Errorf("%s is already mapped to %s:%s", listen, m.Machine, m.Target), "remove it first with `gst map remove "+listen+"`")
					}
				}
				f.Maps = append(f.Maps, config.Map{Listen: listen, Machine: machine, Target: target})
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return err
			}
			if err := a.checkListening(cmd.Context(), listen); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Mapped %s to %s on %s.\n", listen, target, machine)
			return nil
		}),
	}
}

func (a *app) mapRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <listen>...",
		Aliases: []string{"rm"},
		Short:   "Remove maps",
		Example: "  gst map remove 15432",
		Args:    a.args(1, -1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			listens := make([]string, len(args))
			for i, arg := range args {
				var err error
				if listens[i], err = config.ParseListen(arg); err != nil {
					return &usageError{err}
				}
			}
			err := a.update(func(f *config.File) error {
				for _, listen := range listens {
					i := slices.IndexFunc(f.Maps, func(m config.Map) bool { return m.Listen == listen })
					if i < 0 {
						return withHint(fmt.Errorf("no map listens on %s", listen), "see the maps with `gst map list`")
					}
					f.Maps = slices.Delete(f.Maps, i, i+1)
				}
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return err
			}
			for _, listen := range listens {
				fmt.Fprintf(a.stdout, "Removed map %s.\n", listen)
			}
			return nil
		}),
	}
}

// mapView is a map as gst map list prints it. Its JSON form is part of
// the command line interface.
type mapView struct {
	config.Map
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// mapViews returns f's maps with their states in st, the daemon's
// status, which is nil when no daemon answers.
func mapViews(f *config.File, st *wire.Status) []mapView {
	states := mapListeners(st)
	views := make([]mapView, len(f.Maps))
	for i, m := range f.Maps {
		views[i].Map = m
		views[i].Status, views[i].Error = listenerStatus(m.Listen, states)
	}
	return views
}

// mapListeners returns the map listeners in st, the daemon's status, and
// nil when st is nil because no daemon answers.
func mapListeners(st *wire.Status) []wire.Listener {
	if st == nil {
		return nil
	}
	states := make([]wire.Listener, len(st.Maps))
	for i, m := range st.Maps {
		states[i] = m.Listener
	}
	return states
}

func (a *app) mapListCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List maps",
		Args:    a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			f, err := a.load()
			if err != nil {
				return err
			}
			views := mapViews(f, a.status(cmd.Context()))
			if asJSON {
				return writeJSON(a.stdout, views)
			}
			if len(views) == 0 {
				fmt.Fprintln(a.stdout, "No maps. Add one with: gst map add <listen> <machine>:<target>")
				return nil
			}
			w := table(a.stdout)
			fmt.Fprintln(w, "LISTEN\tMACHINE\tTARGET\tSTATUS")
			for _, v := range views {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", v.Listen, v.Machine, v.Target, statusLine(v.Status, v.Error))
			}
			return w.Flush()
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func (a *app) routeCommand() *cobra.Command {
	cmd := asGroup(&cobra.Command{
		Use:   "route",
		Short: "Send proxied connections out through other machines",
		Long: `A route sends the proxy's connections to some hosts out through another
machine, an exit, as if they were made there: hosts of a domain and its
subdomains, IP addresses in a prefix, or, with default, every host. The
most specific route wins: longer domains and prefixes first, default
last. Hosts that no route matches are connected to from this machine.
Prefixes match IP addresses only; a domain name is resolved by the exit,
not to pick a route. The machine must be an exit: run gst exit on there.`,
	})
	cmd.AddCommand(a.routeAddCommand(), a.routeRemoveCommand(), a.routeListCommand())
	return cmd
}

func (a *app) routeAddCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "add <match> <machine>",
		Short: "Send connections to matching hosts through a machine",
		Long: `Send the proxy's connections to the hosts that <match> matches through
<machine>. <match> is default, a domain such as example.com, an IP
address, or a CIDR prefix such as 10.0.0.0/8. A route with the same
match is replaced.`,
		Example: `  gst route add corp.example.com devcn
  gst route add 10.20.0.0/16 devcn
  gst route add default devsg`,
		Args: a.args(2, 2),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			match, err := config.ParseRouteMatch(args[0])
			if err != nil {
				return &usageError{err}
			}
			via, err := config.ParseMachine(args[1])
			if err != nil {
				return &usageError{err}
			}
			err = a.update(func(f *config.File) error {
				f.Routes = slices.DeleteFunc(f.Routes, func(r config.Route) bool { return r.Match == match })
				f.Routes = append(f.Routes, config.Route{Match: match, Via: via})
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Route %s goes through %s.\n", match, via)
			a.noteExit(cmd.Context(), via)
			return nil
		}),
	}
}

// noteExit warns when the mesh shows that machine is not an exit, or does
// not know it.
func (a *app) noteExit(ctx context.Context, machine string) {
	b, err := a.backend()
	if err != nil {
		return
	}
	topo, err := b.Nodes(ctx)
	if err != nil {
		return
	}
	switch node, err := mesh.FindNode(topo, machine); {
	case err != nil:
		fmt.Fprintf(a.stderr, "note: %v\n", err)
	case !node.Exit:
		fmt.Fprintf(a.stderr, "note: %s is not an exit yet; run `gst exit on` there\n", node.Name)
	}
}

func (a *app) routeRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <match>...",
		Aliases: []string{"rm"},
		Short:   "Remove routes",
		Example: "  gst route remove default",
		Args:    a.args(1, -1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			matches := make([]string, len(args))
			for i, arg := range args {
				var err error
				if matches[i], err = config.ParseRouteMatch(arg); err != nil {
					return &usageError{err}
				}
			}
			err := a.update(func(f *config.File) error {
				for _, match := range matches {
					i := slices.IndexFunc(f.Routes, func(r config.Route) bool { return r.Match == match })
					if i < 0 {
						return withHint(fmt.Errorf("no route for %s", match), "see the routes with `gst route list`")
					}
					f.Routes = slices.Delete(f.Routes, i, i+1)
				}
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return err
			}
			for _, match := range matches {
				fmt.Fprintf(a.stdout, "Removed route %s.\n", match)
			}
			return nil
		}),
	}
}

func (a *app) routeListCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List routes",
		Args:    a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			f, err := a.load()
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(a.stdout, f.Routes)
			}
			if len(f.Routes) == 0 {
				fmt.Fprintln(a.stdout, "No routes; the proxy connects to hosts outside the mesh from this machine. Add one with: gst route add <match> <machine>")
				return nil
			}
			w := table(a.stdout)
			fmt.Fprintln(w, "MATCH\tVIA")
			for _, r := range f.Routes {
				fmt.Fprintf(w, "%s\t%s\n", r.Match, r.Via)
			}
			return w.Flush()
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
