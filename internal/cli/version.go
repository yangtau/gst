package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/tun"
	"github.com/yangtau/ssh-tunnel/internal/version"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// built describes this gst; gst version prints it.
var built = buildInfo{Info: version.Built, Protocol: wire.Protocol, Helper: tun.HelperVersion}

// software names this gst for people; see version.Software.
var software = version.Software

// buildInfo is what gst version prints.
type buildInfo struct {
	version.Info
	Protocol int
	Helper   int
}

func (b buildInfo) write(out io.Writer) {
	fmt.Fprintln(out, "gst", b.Release)
	if b.Commit != "" {
		dirty := ""
		if b.Dirty {
			dirty = " (dirty)"
		}
		fmt.Fprintf(out, "commit: %s%s\n", b.Commit, dirty)
	}
	if b.Committed != "" {
		fmt.Fprintln(out, "committed:", b.Committed)
	}
	fmt.Fprintln(out, "protocol:", b.Protocol)
	fmt.Fprintln(out, "helper:", b.Helper)
}

func (a *app) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the gst version",
		Long: `Show the gst release, the git commit it was built from (dirty when built
with uncommitted changes) and that commit's time, the gst protocol, which
must be the same on linked machines, and the version of the helper that
gst up installs.`,
		Args: a.args(0, 0),
		Run: func(*cobra.Command, []string) {
			built.write(a.stdout)
		},
	}
}
