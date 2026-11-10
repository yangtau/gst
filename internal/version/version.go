// Package version says which gst this is: its release and the git commit
// it was built from. The gst command and the iOS app are both built from
// it, show it to people, and tell it to the other nodes.
package version

import (
	"runtime/debug"
	"strings"
)

// Release is the gst release. `make release` sets it; see AGENTS.md.
const Release = "0.7.0"

// commit ("<revision>[-dirty]") and committed (RFC 3339) describe the git
// commit gst is built from, for builds without Go's VCS stamp, such as
// nix's: -ldflags "-X <this package>.commit=... -X <this package>.committed=...".
var commit, committed string

// Built describes this gst.
var Built = Read(commit, committed, buildSettings())

// Software names this gst for people, in gst status and the handshake:
// the release and the short commit, such as "gst 0.5.0+a2ffc87.dirty".
// Only the build tells executables apart.
var Software = "gst " + Built.Short()

// Info is a release and the commit it was built from.
type Info struct {
	Release   string
	Commit    string
	Dirty     bool
	Committed string
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

// Read takes the commit from commit and committed when set, else from
// Go's VCS stamp in settings.
func Read(commit, committed string, settings map[string]string) Info {
	b := Info{Release: Release}
	if commit != "" {
		b.Commit, b.Dirty = strings.CutSuffix(commit, "-dirty")
		b.Committed = committed
	} else {
		b.Commit, b.Dirty = settings["vcs.revision"], settings["vcs.modified"] == "true"
		b.Committed = settings["vcs.time"]
	}
	return b
}

// Short is the release with the short commit as semver build metadata.
func (b Info) Short() string {
	if b.Commit == "" {
		return b.Release
	}
	s := b.Release + "+" + b.Commit[:min(len(b.Commit), 7)]
	if b.Dirty {
		s += ".dirty"
	}
	return s
}
