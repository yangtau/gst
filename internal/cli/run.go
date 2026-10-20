package cli

import (
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/tunnel"
)

// runCommand runs a tunnel in the foreground. The background service runs
// it; its log omits addresses, paths, and users.
func (a *app) runCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "run <name>",
		Short:  "Run a tunnel in the foreground until it fails or is interrupted",
		Long:   "Run a tunnel in the foreground until it fails or receives SIGINT or SIGTERM. The background service uses this command; do not run it while the tunnel is started.",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			f, err := a.load()
			if err != nil {
				log.Print(err)
				return &exitError{exitUsage}
			}
			t := f.Tunnels[args[0]]
			if t == nil {
				log.Printf("no tunnel named %s", args[0])
				return &exitError{exitUsage}
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			opts, err := a.options(ctx, t)
			if err != nil {
				err = errors.New("ssh config resolution failed")
			} else {
				err = tunnel.Run(ctx, opts)
			}
			if err != nil && ctx.Err() == nil {
				log.Print(err)
				return &exitError{exitFailure}
			}
			return nil
		}),
	}
}
