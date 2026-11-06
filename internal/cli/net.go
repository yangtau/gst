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
	netWait = 15 * time.Second       // for gst subnet to see the network up
	netPoll = 200 * time.Millisecond // how often it looks
)

func (a *app) subnetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "subnet [cidr]",
		Short: "Show or change the subnet of the virtual network",
		Long: `Show the private subnet that the machines of the mesh share, or change it
for all of them. The first machine chooses a /16 that overlaps nothing on
the machines that answer; later ones take the subnet of the others.

The latest choice wins, on every machine of the network, and they all
change their addresses to it. It must be private: inside 10.0.0.0/8,
172.16.0.0/12, 192.168.0.0/16, or 100.64.0.0/10, with a prefix length of 8
to 28.`,
		Example: "  gst subnet\n  gst subnet 10.99.0.0/16",
		Args:    a.args(0, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return a.showSubnet(cmd.Context())
			}
			prefix, err := config.ParseSubnet(args[0])
			if err != nil {
				return &usageError{err}
			}
			subnet := prefix.String()
			err = a.update(func(f *config.File) error {
				f.Net = &config.Net{Subnet: subnet, Since: time.Now().UnixNano()}
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
}

func (a *app) showSubnet(ctx context.Context) error {
	st := a.status(ctx)
	if st == nil {
		return withHint(errors.New("the gst daemon does not answer"), "start it with `gst up`")
	}
	if st.Net == nil || st.Net.Subnet == "" {
		return withHint(errors.New("this machine has no subnet yet"), "see why with `gst status`")
	}
	fmt.Fprintln(a.stdout, st.Net.Subnet)
	return nil
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
				return nil, withHint(errors.New("the virtual network did not come up in time"), "see how it goes with `gst status`")
			}
			return nil, errors.New(last.Error)
		case <-time.After(netPoll):
		}
	}
}

// writePeers prints the other machines of the virtual network.
func writePeers(out io.Writer, st *wire.NetState) error {
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
	return withHint(errors.New("this command needs root"), "run it with `sudo gst "+command+"`")
}

func (a *app) installCommand() *cobra.Command {
	var userName string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Put this machine on the virtual network (as root, once)",
		Long: `Install the system service that creates the interface of the virtual
network, as root, for one user's daemon, which then joins the network by
itself. Each machine needs it once. It runs a copy of this gst that root
owns, so upgrading gst does not change it; run this command again to
update it. Run it with sudo; the user is the one who ran sudo, unless
--user says otherwise.`,
		Example: "  sudo gst install",
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
			fmt.Fprintf(a.stdout, "The helper runs for %s. This machine joins the network in a few seconds; see it with: gst status\n", name)
			return nil
		}),
	}
	cmd.Flags().StringVar(&userName, "user", "", "the user whose daemon the helper serves (default: $SUDO_USER)")
	return cmd
}

func (a *app) uninstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Take this machine off the virtual network (as root)",
		Long:  "Remove the helper's service and its copy of gst. Without it this machine has no address on the virtual network, and only passes on what other machines send through it.",
		Args:  a.args(0, 0),
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
		Use:    "net-helper",
		Short:  "Run the helper of the virtual network in the foreground",
		Hidden: true,
		Args:   a.args(0, 0),
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
