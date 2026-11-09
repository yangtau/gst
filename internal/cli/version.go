package cli

import (
	"fmt"
	"io"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/tun"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// version is the gst release. `make release` sets it; see AGENTS.md.
const version = "0.5.0"

// commit ("<revision>[-dirty]") and committed (RFC 3339) describe the git
// commit gst is built from, for builds without Go's VCS stamp, such as
// nix's: -ldflags "-X <this package>.commit=... -X <this package>.committed=...".
var commit, committed string

// built describes this gst; gst version prints it.
var built = readBuildInfo(commit, committed, buildSettings())

// software names this gst for people, in gst status and the handshake:
// the release and the short commit, such as "gst 0.5.0+a2ffc87.dirty".
// Only the build tells executables apart.
var software = "gst " + built.short()

// buildInfo is what gst version prints.
type buildInfo struct {
	Version   string
	Commit    string
	Dirty     bool
	Committed string
	Protocol  int
	Helper    int
}

func buildSettings() map[string]string {
	settings := map[string]string{}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			settings[s.Key] = s.Value
		}
	}
	return settings
}

// readBuildInfo takes the commit from commit and committed when set, else
// from Go's VCS stamp in settings.
func readBuildInfo(commit, committed string, settings map[string]string) buildInfo {
	b := buildInfo{Version: version, Protocol: wire.Protocol, Helper: tun.HelperVersion}
	if commit != "" {
		b.Commit, b.Dirty = strings.CutSuffix(commit, "-dirty")
		b.Committed = committed
	} else {
		b.Commit, b.Dirty = settings["vcs.revision"], settings["vcs.modified"] == "true"
		b.Committed = settings["vcs.time"]
	}
	return b
}

// short is the release with the short commit as semver build metadata.
func (b buildInfo) short() string {
	if b.Commit == "" {
		return b.Version
	}
	s := b.Version + "+" + b.Commit[:min(len(b.Commit), 7)]
	if b.Dirty {
		s += ".dirty"
	}
	return s
}

func (b buildInfo) write(out io.Writer) {
	fmt.Fprintln(out, "gst", b.Version)
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
