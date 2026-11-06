package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/service"
	"github.com/yangtau/ssh-tunnel/internal/tun"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

const (
	netWait = 15 * time.Second       // for gst net on to see the network up
	netPoll = 200 * time.Millisecond // how often it looks
)

func (a *app) netCommand() *cobra.Command {
	cmd := asGroup(&cobra.Command{
		Use:   "net",
		Short: "Give the machines IP addresses on a private network",
		Long: `Put this machine on a virtual network with the other machines that turn
it on. Each of them gets an IP address in one private subnet and the
name <machine>.gst, for every program and any protocol, TCP and UDP
alike. WireGuard encrypts the packets end to end; they go directly over
UDP where the network allows, and along the links of the mesh where not.

The interface needs root once: install the helper with sudo gst net
install, which also lets the network start with the daemon.`,
		Example: `  sudo gst net install                 # once on each machine
  gst net on                           # on each machine
  ping devbox.gst
  gst net status`,
	})
	cmd.AddCommand(a.netOnCommand(), a.netOffCommand(), a.netStatusCommand(), a.netInstallCommand(), a.netUninstallCommand())
	return cmd
}

func (a *app) netOnCommand() *cobra.Command {
	var subnet string
	cmd := &cobra.Command{
		Use:   "on",
		Short: "Join the virtual network",
		Long: `Put this machine on the virtual network, and keep it there across
restarts. The machines of the mesh share one subnet. The first machine
chooses a /16 that overlaps nothing on the machines that answer; later
ones take the subnet of the others.

With --subnet, use that subnet instead, on every machine of the network:
the latest choice wins, and they all change their addresses to it. It
must be private: inside 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, or
100.64.0.0/10, with a prefix length of 8 to 28.`,
		Example: "  gst net on\n  gst net on --subnet 10.99.0.0/16",
		Args:    a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			if subnet != "" {
				prefix, err := config.ParseSubnet(subnet)
				if err != nil {
					return &usageError{err}
				}
				subnet = prefix.String()
			}
			err := a.update(func(f *config.File) error {
				if subnet != "" {
					f.Net = &config.Net{Subnet: subnet, Since: time.Now().UnixNano()}
				} else if f.Net == nil {
					f.Net = &config.Net{}
				}
				return nil
			})
			if err != nil {
				return err
			}
			if err := a.sync(cmd.Context()); err != nil {
				return err
			}
			f, err := a.load()
			if err != nil {
				return err
			}
			if f.Down {
				return nil
			}
			st, err := a.waitNet(cmd.Context(), subnet)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "This machine is %s on %s (subnet %s).\n", st.Address, st.Interface, st.Subnet)
			return nil
		}),
	}
	cmd.Flags().StringVar(&subnet, "subnet", "", "the private subnet of the whole network, such as 10.99.0.0/16")
	return cmd
}

// waitNet waits until the network runs, on subnet when it is not empty,
// and returns its state, or the error that keeps it from running.
func (a *app) waitNet(ctx context.Context, subnet string) (*wire.NetState, error) {
	ctx, cancel := context.WithTimeout(ctx, netWait)
	defer cancel()
	var last *wire.NetState
	for {
		if st := a.status(ctx); st != nil && st.Net != nil {
			last = st.Net
			if last.Hint != "" {
				// A command can fix it; waiting cannot.
				return nil, withHint(errors.New(last.Error), last.Hint)
			}
			if last.Error == "" && last.Interface != "" && (subnet == "" || last.Subnet == subnet) {
				return last, nil
			}
		}
		select {
		case <-ctx.Done():
			switch {
			case last == nil:
				return nil, noDaemon(ctx.Err())
			case last.Error == "":
				return nil, withHint(errors.New("the virtual network did not come up in time"), "see how it goes with `gst net status`")
			}
			return nil, errors.New(last.Error)
		case <-time.After(netPoll):
		}
	}
}

func (a *app) netOffCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "Leave the virtual network",
		Long:  "Take this machine off the virtual network: its interface, routes, and .gst names go away, on this machine. The other machines stay on the network.",
		Args:  a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			err := a.update(func(f *config.File) error {
				f.Net = nil
				return nil
			})
			if err != nil {
				return err
			}
			return a.sync(cmd.Context())
		}),
	}
}

// netView is what gst net status --json prints. Its JSON form is part of
// the command line interface.
type netView struct {
	On bool `json:"on"`
	*wire.NetState
}

func (a *app) netStatusCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show this machine's address and the other machines",
		Long: `Show this machine's address on the virtual network and, for each other
machine, its address, how packets go to it (direct over UDP, or along the
mesh), and when WireGuard last confirmed the machine's key.`,
		Args: a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			b, err := a.backend()
			if err != nil {
				return err
			}
			st, ok := b.Status(cmd.Context())
			if !ok {
				return withHint(errors.New("the gst daemon does not answer"), "start it with `gst up`")
			}
			if asJSON {
				return writeJSON(a.stdout, netView{st.Net != nil, st.Net})
			}
			return writeNet(a.stdout, st.Net)
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func writeNet(out io.Writer, st *wire.NetState) error {
	if st == nil {
		fmt.Fprintln(out, "The virtual network is off. Join it with: gst net on")
		return nil
	}
	fmt.Fprintf(out, "Address: %s\nSubnet: %s\nInterface: %s\n", cmp.Or(st.Address, "-"), cmp.Or(st.Subnet, "-"), cmp.Or(st.Interface, "-"))
	if st.Error != "" {
		fmt.Fprintf(out, "\n# Problem: %s\n", st.Error)
		if st.Hint != "" {
			fmt.Fprintf(out, "# Hint: %s\n", st.Hint)
		}
	}
	if len(st.Peers) == 0 {
		fmt.Fprintln(out, "\nNo other machine is on the network yet.")
		return nil
	}
	fmt.Fprintln(out)
	w := table(out)
	fmt.Fprintln(w, "MACHINE\tADDRESS\tPATH\tHANDSHAKE\tRECEIVED\tSENT")
	for _, p := range st.Peers {
		handshake := "never"
		if !p.Handshake.IsZero() {
			handshake = time.Since(p.Handshake).Round(time.Second).String() + " ago"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, p.Address, p.Path, handshake, byteCount(p.Received), byteCount(p.Sent))
	}
	return w.Flush()
}

func byteCount(n uint64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatUint(n, 10) + " B"
	}
	value, prefix := float64(n)/unit, 0
	for value >= unit && prefix < 3 {
		value /= unit
		prefix++
	}
	return fmt.Sprintf("%.1f %ciB", value, "KMGT"[prefix])
}

// asRoot fails unless gst runs as root.
func asRoot(command string) error {
	if os.Geteuid() == 0 {
		return nil
	}
	return withHint(errors.New("this command needs root"), "run it with `sudo gst net "+command+"`")
}

func (a *app) netInstallCommand() *cobra.Command {
	var userName string
	cmd := &cobra.Command{
		Use:         "install",
		Annotations: standalone,
		Short:       "Install the helper that sets up the network interface (as root)",
		Long: `Install the system service that creates the interface of the virtual
network, as root, for one user's daemon. It runs a copy of this gst that
root owns, so upgrading gst does not change it; run this command again to
update it. Run it with sudo; the user is the one who ran sudo, unless
--user says otherwise.`,
		Example: "  sudo gst net install",
		Args:    a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			if err := asRoot("install"); err != nil {
				return err
			}
			name := cmp.Or(userName, os.Getenv("SUDO_USER"))
			if name == "" {
				return usage("give the user whose daemon the helper serves with --user")
			}
			if _, err := user.Lookup(name); err != nil {
				return usage("unknown user %q", name)
			}
			exe, err := a.executable()
			if err != nil {
				return err
			}
			h, err := service.NewHelper()
			if err != nil {
				return err
			}
			if err := h.Install(exe, name); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "The helper runs for %s. Join the network with: gst net on\n", name)
			return nil
		}),
	}
	cmd.Flags().StringVar(&userName, "user", "", "the user whose daemon the helper serves (default: $SUDO_USER)")
	return cmd
}

func (a *app) netUninstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "uninstall",
		Annotations: standalone,
		Short:       "Remove the helper (as root)",
		Long:        "Remove the helper's service and its copy of gst. Without it the virtual network cannot run on this machine.",
		Args:        a.args(0, 0),
		RunE: run(func(*cobra.Command, []string) error {
			if err := asRoot("uninstall"); err != nil {
				return err
			}
			h, err := service.NewHelper()
			if err != nil {
				return err
			}
			return h.Uninstall()
		}),
	}
}

// netHelperCommand is what the helper's service runs.
func (a *app) netHelperCommand() *cobra.Command {
	var userName string
	cmd := &cobra.Command{
		Use:         "net-helper",
		Annotations: standalone,
		Short:       "Run the helper of the virtual network in the foreground",
		Hidden:      true,
		Args:        a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			u, err := user.Lookup(userName)
			if err != nil {
				return usage("unknown user %q", userName)
			}
			uid, err := strconv.Atoi(u.Uid)
			if err != nil {
				return err
			}
			gid, err := strconv.Atoi(u.Gid)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			h := tun.Helper{Socket: tun.Socket, UID: uid, GID: gid, Log: log.Default()}
			return h.Serve(ctx)
		}),
	}
	cmd.Flags().StringVar(&userName, "user", "", "the user whose daemon it serves")
	return cmd
}
