package cli

import (
	"errors"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/tunnel"
)

// runCommand runs a tunnel in the foreground. The background service runs
// it; its log omits addresses, paths, and users.
func (a *app) runCommand() *cobra.Command {
	var logPath string
	cmd := &cobra.Command{
		Use:    "run <name>",
		Short:  "Run a tunnel in the foreground until it fails or is interrupted",
		Long:   "Run a tunnel in the foreground until it fails or receives SIGINT or SIGTERM. The background service uses this command; do not run it while the tunnel is started.",
		Hidden: true,
		Args:   a.args(1, 1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			if logPath != "" {
				l, err := openLog(logPath, []int{1, 2})
				if err != nil {
					log.Print("log file open failed")
					return &exitError{exitFailure}
				}
				defer l.Close()
				log.SetOutput(l)
			}
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
			var notBefore time.Time
			opts, err := a.options(ctx, t)
			if err != nil {
				err = errors.New("ssh config resolution failed")
			} else {
				if runtime.GOOS == "darwin" && logPath != "" {
					// TN3179 / FB16131937: staying alive gives macOS time to show
					// local network authorization; 30s is best effort, not a guarantee.
					notBefore = time.Now().Add(30 * time.Second)
				}
				err = tunnel.Run(ctx, opts)
			}
			return finishRun(ctx, err, notBefore)
		}),
	}
	cmd.Flags().StringVar(&logPath, "log-file", "", "write the log to this file, rotating it at 1 MiB")
	return cmd
}
