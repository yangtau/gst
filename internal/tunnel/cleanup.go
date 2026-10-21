package tunnel

import (
	_ "embed"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/yangtau/ssh-tunnel/internal/config"
)

//go:embed cleanup.sh
var cleanupScript string

// cleanupListeners kills processes owned by the login user that listen on
// the ports and socket paths of remote forwards on the server, and removes
// the login user's socket files that nothing listens on, so that stale
// tunnels free their listeners. Remote output is discarded because it may
// reveal server details.
func cleanupListeners(client *ssh.Client, forwards []config.Forward, timeout time.Duration) error {
	args := []string{"bash", "-s", "--"}
	for _, fwd := range forwards {
		switch {
		case fwd.Direction != config.Remote:
		case config.IsSocket(fwd.Listen):
			args = append(args, shellQuote(fwd.Listen))
		default:
			args = append(args, strconv.Itoa(int(fwd.ListenAddr().Port())))
		}
	}
	// The script expects at least one listener.
	if len(args) == 3 {
		return nil
	}

	// Bound the whole cleanup, even if the server keeps answering
	// keepalives while the command hangs.
	timer := time.AfterFunc(timeout, func() { client.Close() })
	defer timer.Stop()

	session, err := client.NewSession()
	if err != nil {
		return errors.New("cleanup session failed")
	}
	defer session.Close()

	session.Stdin = strings.NewReader(cleanupScript)
	session.Stdout = io.Discard
	session.Stderr = io.Discard
	if err := session.Run(strings.Join(args, " ")); err != nil {
		return errors.New("remote listener cleanup failed")
	}
	return nil
}

// shellQuote quotes s for the login shell that runs the cleanup command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
