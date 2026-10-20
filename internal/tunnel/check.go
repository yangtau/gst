package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Step is the outcome of one diagnostic check.
type Step struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// Diagnose checks the identities and known_hosts entries and, if online,
// the connection and login, stopping at the first failure. Unlike Run, it
// reports paths, addresses, and server errors, because it is meant for the
// user to read.
func Diagnose(ctx context.Context, opts Options, online bool) []Step {
	var steps []Step
	add := func(s Step) bool {
		steps = append(steps, s)
		return s.OK
	}
	if !add(checkIdentities(opts)) || !add(checkKnownHosts(opts)) || !online {
		return steps
	}
	conn, step := checkConnect(ctx, opts)
	if !add(step) {
		return steps
	}
	add(checkLogin(opts, conn))
	return steps
}

func checkIdentities(opts Options) Step {
	step := Step{Name: "identity"}
	var usable, skipped []string
	for _, id := range loadIdentities(opts.IdentityFiles) {
		switch {
		case id.signer != nil:
			usable = append(usable, id.path)
		case id.problem != "missing" || len(opts.IdentityFiles) == 1:
			skipped = append(skipped, id.path+" ("+id.problem+")")
		}
	}
	step.OK = len(usable) > 0
	switch {
	case step.OK:
		step.Detail = "using " + strings.Join(usable, ", ")
		if len(skipped) > 0 {
			step.Detail += "; skipped " + strings.Join(skipped, ", ")
		}
	case len(skipped) > 0:
		step.Detail = "no usable key among " + strings.Join(skipped, ", ")
	default:
		step.Detail = "none of the identity files exist: " + strings.Join(opts.IdentityFiles, ", ")
	}
	if !step.OK {
		step.Hint = "gst needs a private key without a passphrase. Create one with " +
			"`ssh-keygen -t ed25519 -N '' -f ~/.ssh/gst_ed25519`, authorize it on the server with " +
			"`ssh-copy-id -i ~/.ssh/gst_ed25519 " + loginTarget(opts) + "`, and select it with --identity ~/.ssh/gst_ed25519"
	}
	return step
}

func checkKnownHosts(opts Options) Step {
	step := Step{Name: "known_hosts"}
	callback, files, err := loadKnownHosts(opts.KnownHostsFiles)
	switch {
	case files == nil:
		step.Detail = "none of the known_hosts files exist: " + strings.Join(opts.KnownHostsFiles, ", ")
	case err != nil:
		step.Detail = err.Error()
	case len(knownHostKeyAlgorithms(callback, opts.Server)) == 0:
		step.Detail = "no host key for " + opts.Server + " in " + strings.Join(files, ", ")
	default:
		step.OK = true
		step.Detail = "host key for " + opts.Server + " found in " + strings.Join(files, ", ")
	}
	if !step.OK {
		step.Hint = "verify the server's host key and record it by connecting once with `ssh " + loginTarget(opts) + "`"
	}
	return step
}

func checkConnect(ctx context.Context, opts Options) (net.Conn, Step) {
	step := Step{Name: "connect"}
	dialer := net.Dialer{Timeout: opts.ConnectTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", opts.Server)
	if err != nil {
		step.Detail = err.Error()
		step.Hint = "check that the server is reachable and sshd listens on " + opts.Server
		return nil, step
	}
	step.OK = true
	step.Detail = "reached " + opts.Server
	return conn, step
}

func checkLogin(opts Options, conn net.Conn) Step {
	defer conn.Close()
	step := Step{Name: "login"}
	clientConfig, err := newClientConfig(opts)
	if err != nil {
		step.Detail = err.Error()
		return step
	}
	conn.SetDeadline(time.Now().Add(opts.ConnectTimeout))
	sshConn, channels, requests, err := ssh.NewClientConn(conn, opts.Server, clientConfig)
	if err != nil {
		step.Detail = err.Error()
		var keyErr *knownhosts.KeyError
		switch {
		case errors.As(err, &keyErr) && len(keyErr.Want) > 0:
			step.Hint = "the server's host key differs from the one in known_hosts. If it changed on purpose, remove the old entry with `ssh-keygen -R " + knownHostsName(opts.Server) + "` and connect once with `ssh " + loginTarget(opts) + "`"
		case strings.Contains(err.Error(), "unable to authenticate"):
			step.Hint = "authorize the identity on the server, for example with `ssh-copy-id " + loginTarget(opts) + "`"
		}
		return step
	}
	ssh.NewClient(sshConn, channels, requests).Close()
	step.OK = true
	step.Detail = "logged in as " + opts.User
	return step
}

// loginTarget formats the server for ssh and ssh-copy-id command lines.
func loginTarget(opts Options) string {
	host, port, _ := net.SplitHostPort(opts.Server)
	if port == "22" {
		return opts.User + "@" + host
	}
	return fmt.Sprintf("-p %s %s@%s", port, opts.User, host)
}

// knownHostsName is how known_hosts names the server.
func knownHostsName(server string) string {
	host, port, _ := net.SplitHostPort(server)
	if port == "22" {
		return host
	}
	return "[" + host + "]:" + port
}
