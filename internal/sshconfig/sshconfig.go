// Package sshconfig resolves ssh destinations the way OpenSSH does, by
// asking "ssh -G", so that gst follows the user's ssh configuration,
// including Include and Match blocks.
package sshconfig

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ConfigFileEnv names an environment variable that, when set, replaces the
// user's ssh configuration file; it is passed to "ssh -F".
const ConfigFileEnv = "GST_SSH_CONFIG"

// Host is what ssh would use to connect to a destination.
type Host struct {
	Hostname        string
	Port            string
	User            string
	IdentityFiles   []string      // candidate private keys in ssh's order
	KnownHostsFiles []string      // user files, then global files
	ConnectTimeout  time.Duration // zero when unset
}

// Address returns the host:port to connect to.
func (h Host) Address() string {
	return net.JoinHostPort(h.Hostname, h.Port)
}

// Resolve runs "ssh -G" for destination, with the ssh options sshArgs,
// such as -p 2222. It only evaluates the configuration and does not
// connect.
func Resolve(ctx context.Context, destination string, sshArgs []string) (Host, error) {
	args := []string{"-G"}
	if file := os.Getenv(ConfigFileEnv); file != "" {
		args = append(args, "-F", file)
	}
	args = append(append(args, sshArgs...), "--", destination)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := sshMessage(stderr.String()); msg != "" {
			return Host{}, fmt.Errorf("ssh -G %s: %s", destination, msg)
		}
		return Host{}, fmt.Errorf("ssh -G %s: %w", destination, err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Host{}, errors.New("home directory lookup failed")
	}
	return parse(out, home)
}

// sshMessage drops ssh's TTY notice, which some configurations trigger even
// for -G, and joins the remaining lines.
func sshMessage(stderr string) string {
	var lines []string
	for line := range strings.Lines(stderr) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "Pseudo-terminal will not be allocated") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "; ")
}

// parse reads "ssh -G" output, one lowercase "keyword value" per line.
func parse(out []byte, home string) (Host, error) {
	var h Host
	var userKnownHosts, globalKnownHosts []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		key, value, _ := strings.Cut(scanner.Text(), " ")
		var err error
		switch key {
		case "hostname":
			h.Hostname = value
		case "port":
			h.Port = value
		case "user":
			h.User = value
		case "identityfile":
			h.IdentityFiles = append(h.IdentityFiles, expandHome(value, home))
		case "userknownhostsfile":
			userKnownHosts = expandAll(value, home)
		case "globalknownhostsfile":
			globalKnownHosts = expandAll(value, home)
		case "connecttimeout":
			h.ConnectTimeout, err = seconds(value)
		case "proxycommand", "proxyjump":
			if value != "none" {
				return Host{}, fmt.Errorf("ssh config sets %s, which gst does not support", key)
			}
		case "hostkeyalias":
			if value != "none" {
				return Host{}, errors.New("ssh config sets hostkeyalias, which gst does not support")
			}
		}
		if err != nil {
			return Host{}, fmt.Errorf("unexpected ssh -G value for %s: %q", key, value)
		}
	}
	if h.Hostname == "" || h.Port == "" || h.User == "" {
		return Host{}, errors.New("ssh -G output lacks hostname, port, or user")
	}
	h.KnownHostsFiles = append(userKnownHosts, globalKnownHosts...)
	return h, nil
}

func seconds(value string) (time.Duration, error) {
	if value == "none" {
		return 0, nil
	}
	n, err := strconv.ParseInt(value, 10, 32)
	if err != nil || n < 0 {
		return 0, errors.New("invalid seconds")
	}
	return time.Duration(n) * time.Second, nil
}

func expandAll(value, home string) []string {
	var paths []string
	for _, path := range strings.Fields(value) {
		paths = append(paths, expandHome(path, home))
	}
	return paths
}

func expandHome(path, home string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		return filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	return path
}

// Literal reads what a destination says by itself, "user@host" or
// "ssh://user@host:port", for machines that have no ssh to ask and no ssh
// configuration, such as phones. The destination must name the user, as
// nothing else does there; the port is 22 unless it names one.
func Literal(destination string) (Host, error) {
	h := Host{Port: "22"}
	if rest, ok := strings.CutPrefix(destination, "ssh://"); ok {
		u, err := url.Parse("ssh://" + rest)
		if err != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
			return Host{}, errors.New("invalid destination: use user@host or ssh://user@host:port")
		}
		h.User, h.Hostname = u.User.Username(), u.Hostname()
		if port := u.Port(); port != "" {
			h.Port = port
		}
	} else if i := strings.LastIndex(destination, "@"); i >= 0 {
		h.User, h.Hostname = destination[:i], strings.TrimSuffix(strings.TrimPrefix(destination[i+1:], "["), "]")
	} else {
		h.Hostname = destination
	}
	if h.Hostname == "" || strings.ContainsAny(h.Hostname, " /@") {
		return Host{}, errors.New("invalid destination: use user@host or ssh://user@host:port")
	}
	if h.User == "" {
		return Host{}, errors.New("the destination must name the user: user@host")
	}
	if n, err := strconv.Atoi(h.Port); err != nil || n < 1 || n > 65535 {
		return Host{}, errors.New("invalid destination: the port must be between 1 and 65535")
	}
	return h, nil
}
