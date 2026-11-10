package cli

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
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
		Short: "Show or change the subnet of the network",
		Long: `Show the private subnet that the gst IPs of all the machines are in, or
change it. gst chooses a free /16 by itself, so this is needed only when
that one collides with another network.

A change applies to every machine of the network, and all of them change
their gst IPs: the machines online at once, the others when they come
back. The subnet must be private: inside 10.0.0.0/8, 172.16.0.0/12,
192.168.0.0/16, or 100.64.0.0/10, with a prefix length of 8 to 28.`,
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
			b, err := a.backend()
			if err != nil {
				return err
			}
			if a.status(cmd.Context()) == nil {
				return withHint(errNoDaemon, "start it with `gst up`")
			}
			if err := b.Call(cmd.Context(), wire.MethodSubnet, wire.SubnetParams{Subnet: subnet}, nil); err != nil {
				return err
			}
			st, err := a.waitNet(cmd.Context(), subnet)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "The subnet is %s. This machine's gst IP is %s.\n", st.Subnet, st.Address)
			return nil
		}),
	}
}

func (a *app) showSubnet(ctx context.Context) error {
	st := a.status(ctx)
	if st == nil {
		return withHint(errNoDaemon, "start it with `gst up`")
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
			// Until the network reports subnet, its state is of the one
			// before.
			current := subnet == "" || last.Subnet == subnet
			if current && last.Hint != "" {
				// A command can fix it; waiting cannot.
				return nil, withHint(errors.New(last.Error), last.Hint)
			}
			if current && last.Error == "" && last.Interface != "" {
				return last, nil
			}
		}
		select {
		case <-ctx.Done():
			switch {
			case last == nil:
				return nil, noDaemon(ctx.Err())
			case last.Error == "" || subnet != "" && last.Subnet != subnet:
				return nil, withHint(errors.New("the network did not come up in time"), "see how it goes with `gst status`")
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
		path := strings.TrimSpace(p.Way + " " + p.Endpoint)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, p.Address, path, handshake, byteCount(p.Received), byteCount(p.Sent))
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

// helperService is the helper of the virtual network as seen without root.
type helperService interface {
	Installed() bool
	Current(version int, socket string) bool
	Installs() []string
	Removes() []string
}

// rootPlan is a command that takes root, and what people need to know
// before they agree to it.
type rootPlan struct {
	why   string   // why gst wants root
	argv  []string // what runs; it starts with sudo unless gst runs as root
	does  []string // what it does to this machine
	then  []string // what the helper goes on to do; may be empty
	undo  string   // how to undo it; may be empty
	again string   // the gst command that tries this again
}

// asRoot returns the command line that runs args as root. The path of gst
// is absolute and has no links, because sudo may not have gst in its PATH
// and root should run the file that the person read here.
func (a *app) asRoot(args ...string) ([]string, error) {
	exe, err := a.executable()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	argv := append([]string{exe}, args...)
	if a.geteuid() != 0 {
		argv = append([]string{"sudo"}, argv...)
	}
	return argv, nil
}

// confirm tells what plan does, and returns whether to run it: gst never
// runs anything as root that the person did not see, and sudo may not ask
// for a password. yes takes the answer from the command line. It needs a
// terminal to ask.
func (a *app) confirm(plan rootPlan, yes bool) bool {
	fmt.Fprintf(a.stderr, "%s It will run:\n\n  %s\n\nThat command:\n", plan.why, strings.Join(plan.argv, " "))
	for _, line := range plan.does {
		fmt.Fprintf(a.stderr, "  - %s\n", line)
	}
	if len(plan.then) > 0 {
		fmt.Fprintln(a.stderr, "\nThe network helper then does only this, for your gst:")
		for _, line := range plan.then {
			fmt.Fprintf(a.stderr, "  - %s\n", line)
		}
	}
	fmt.Fprintln(a.stderr)
	if plan.undo != "" {
		fmt.Fprintln(a.stderr, plan.undo)
	}
	if plan.argv[0] == "sudo" {
		fmt.Fprintln(a.stderr, "sudo may ask for your password; gst never sees it.")
	}
	if yes {
		return true
	}
	fmt.Fprint(a.stderr, "Run it? [y/N] ")
	line, _ := bufio.NewReader(a.stdin).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// runPlan runs plan with this terminal.
func (a *app) runPlan(plan rootPlan) error {
	if err := a.runRoot(plan.argv); err != nil {
		return withHint(fmt.Errorf("%s failed: %w", strings.Join(plan.argv, " "), err), "try again with `"+plan.again+"`")
	}
	return nil
}

// joinNetwork installs or updates the helper of the virtual network if
// this machine needs it and the person agrees.
func (a *app) joinNetwork(yes bool) error {
	h, err := a.newHelper()
	if err != nil {
		return err
	}
	if h.Current(tun.HelperVersion, tun.Socket) {
		return nil
	}
	if !yes && !a.interactive() {
		fmt.Fprintln(a.stderr, "note: this machine is not on the gst network yet: that takes root, and gst asks first")
		fmt.Fprintln(a.stderr, "note: run `gst up` in a terminal to see what it does, or `gst up --yes` to go ahead")
		return nil
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	argv, err := a.asRoot("install", "--user", u.Username)
	if err != nil {
		return err
	}
	why := "gst needs root to put this machine on the gst network."
	if h.Installed() {
		why = "The network helper needs an update, which takes root."
	}
	plan := rootPlan{why: why, argv: argv, does: h.Installs(), then: tun.Changes(), undo: "gst down removes all of this.", again: "gst up"}
	if !a.confirm(plan, yes) {
		fmt.Fprintln(a.stderr, "note: nothing was run. This machine stays off the gst network until you agree with `gst up`.")
		return nil
	}
	return a.runPlan(plan)
}

// leaveNetwork removes the helper of the virtual network. It removes
// itself when asked, which needs no root; one that does not answer takes
// root, which gst asks for first, as it does in joinNetwork.
func (a *app) leaveNetwork(yes bool) error {
	h, err := a.newHelper()
	if err != nil {
		return err
	}
	if !h.Installed() {
		return nil
	}
	if a.removeHelper() == nil {
		fmt.Fprintln(a.stdout, "The network helper is removed. `gst up` asks to install it again.")
		return nil
	}
	if !yes && !a.interactive() {
		return withHint(errors.New("the network helper does not answer, so removing it takes root, which gst asks for first"),
			"run `gst down` in a terminal, or `gst down --yes` to go ahead")
	}
	argv, err := a.asRoot("uninstall")
	if err != nil {
		return err
	}
	plan := rootPlan{why: "The network helper does not answer, so removing it takes root.", argv: argv, does: h.Removes(), again: "gst down"}
	if !a.confirm(plan, yes) {
		return withHint(errors.New("the network helper is still installed"), "remove it with `gst down`")
	}
	return a.runPlan(plan)
}

// installCommand is what gst up runs as root.
func (a *app) installCommand() *cobra.Command {
	var userName string
	cmd := &cobra.Command{
		Use:    "install",
		Short:  "Install the network helper, as root",
		Long:   "Install the system service that creates the interface of the gst network, as root, for one user's daemon. gst up runs this command, after saying what it does.",
		Hidden: true,
		Args:   a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			if a.geteuid() != 0 {
				return withHint(errors.New("this command needs root"), "run `gst up`, which says what it does first")
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
			fmt.Fprintf(a.stdout, "The network helper runs for %s. This machine joins the network in a few seconds; see it with `gst status`.\n", name)
			return nil
		}),
	}
	cmd.Flags().StringVar(&userName, "user", "", "the user whose daemon the helper serves (default: $SUDO_USER)")
	return cmd
}

// uninstallCommand is what gst down runs as root when the helper cannot
// remove itself.
func (a *app) uninstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "uninstall",
		Short:  "Remove the network helper, as root",
		Long:   "Remove the helper's service and its copy of gst. gst down runs this command, after saying what it does, when the helper does not answer.",
		Hidden: true,
		Args:   a.args(0, 0),
		RunE: run(func(*cobra.Command, []string) error {
			if a.geteuid() != 0 {
				return withHint(errors.New("this command needs root"), "run `gst down`")
			}
			h, err := service.NewHelper()
			if err != nil {
				return err
			}
			return h.Uninstall()
		}),
	}
}

// helperVersionCommand prints the version of the helper in this gst, for
// gst up to ask root's copy without root.
func (a *app) helperVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "helper-version",
		Short:  "Print the version of the helper in this gst",
		Hidden: true,
		Args:   a.args(0, 0),
		Run: func(*cobra.Command, []string) {
			fmt.Fprintln(a.stdout, tun.HelperVersion)
		},
	}
}

// netHelperCommand is what the helper's service runs.
func (a *app) netHelperCommand() *cobra.Command {
	var userName string
	cmd := &cobra.Command{
		Use:    "net-helper",
		Short:  "Run the network helper in the foreground",
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
			h := tun.Helper{Socket: tun.Socket, UID: uid, GID: gid, Log: log.Default(), Remove: func() (func(), error) {
				installed, err := service.NewHelper()
				if err != nil {
					return nil, err
				}
				return installed.Detach()
			}}
			return h.Serve(ctx)
		}),
	}
	cmd.Flags().StringVar(&userName, "user", "", "the user whose daemon it serves")
	return cmd
}
