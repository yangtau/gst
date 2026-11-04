package sshconn

import (
	"fmt"
	"net"
	"strings"
)

// CheckError is a problem that Check found, with what the user can do
// about it.
type CheckError struct {
	Problem string
	Hint    string
}

func (e *CheckError) Error() string { return e.Problem }

// Check checks, without connecting, that an identity is usable and that
// known_hosts records the server. Unlike Dial, it reports paths and
// addresses, because it is meant for the user to read.
func Check(opts Options) error {
	if err := checkIdentities(opts); err != nil {
		return err
	}
	return checkKnownHosts(opts)
}

func checkIdentities(opts Options) error {
	var skipped []string
	for _, id := range loadIdentities(opts.IdentityFiles) {
		switch {
		case id.signer != nil:
			return nil
		case id.problem != "missing" || len(opts.IdentityFiles) == 1:
			skipped = append(skipped, id.path+" ("+id.problem+")")
		}
	}
	problem := "none of the identity files exist: " + strings.Join(opts.IdentityFiles, ", ")
	if len(skipped) > 0 {
		problem = "no usable identity among " + strings.Join(skipped, ", ")
	}
	return &CheckError{problem, "gst needs a private key without a passphrase. Create one with " +
		"`ssh-keygen -t ed25519 -N '' -f ~/.ssh/gst_ed25519`, authorize it on the server with " +
		"`ssh-copy-id -i ~/.ssh/gst_ed25519 " + loginTarget(opts) + "`, and name it with IdentityFile in ~/.ssh/config"}
}

func checkKnownHosts(opts Options) error {
	callback, files, err := loadKnownHosts(opts.KnownHostsFiles)
	var problem string
	switch {
	case files == nil:
		problem = "none of the known_hosts files exist: " + strings.Join(opts.KnownHostsFiles, ", ")
	case err != nil:
		problem = err.Error()
	case len(knownHostKeyAlgorithms(callback, opts.Server)) == 0:
		problem = "no host key for " + opts.Server + " in " + strings.Join(files, ", ")
	default:
		return nil
	}
	return &CheckError{problem, "verify the server's host key and record it by connecting once with `ssh " + loginTarget(opts) + "`"}
}

// loginTarget formats the server for ssh and ssh-copy-id command lines.
func loginTarget(opts Options) string {
	host, port, _ := net.SplitHostPort(opts.Server)
	if port == "22" {
		return opts.User + "@" + host
	}
	return fmt.Sprintf("-p %s %s@%s", port, opts.User, host)
}
