package cli

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/yangtau/ssh-tunnel/internal/daemon"
	"github.com/yangtau/ssh-tunnel/internal/direct"
	"github.com/yangtau/ssh-tunnel/internal/logfile"
	"github.com/yangtau/ssh-tunnel/internal/service"
	"github.com/yangtau/ssh-tunnel/internal/tun"
	"github.com/yangtau/ssh-tunnel/internal/vnet"
)

// daemonRunCommand runs the gst daemon in the foreground. Its background
// service runs it; gst commands install that service as needed.
func (a *app) daemonRunCommand() *cobra.Command {
	var logPath string
	cmd := &cobra.Command{
		Use:    "daemon",
		Short:  "Run the gst daemon in the foreground",
		Long:   "Run the gst daemon, which runs this machine's part of the network, in the foreground until it receives SIGINT or SIGTERM; SIGHUP reloads the configuration. The background service runs this command.",
		Hidden: true,
		Args:   a.args(0, 0),
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			if logPath != "" {
				l, err := logfile.Open(logPath, []int{1, 2})
				if err != nil {
					log.Print("log file open failed")
					return &exitError{exitFailure}
				}
				defer l.Close()
				log.SetOutput(l)
			}
			fail := func(err error) error {
				log.Print(err)
				return &exitError{exitFailure}
			}
			configPath, err := a.configPath()
			if err != nil {
				return fail(err)
			}
			socket, err := service.Socket()
			if err != nil {
				return fail(err)
			}
			exe, err := a.executable()
			if err != nil {
				return fail(err)
			}
			build, err := daemon.Build(exe)
			if err != nil {
				return fail(err)
			}
			addresses := direct.Addresses
			if os.Getenv("GST_DIRECT") == "off" {
				addresses = nil
			}
			d := daemon.New(daemon.Config{
				ConfigPath: configPath,
				Socket:     socket,
				Software:   software,
				Build:      build,
				Connect:    a.connectLink,
				Addresses:  addresses,
				OpenNet: func(spec tun.Spec) (vnet.Interface, error) {
					c, err := tun.Dial(tun.Socket, spec)
					if err != nil {
						return nil, err
					}
					return c, nil
				},
				NetDNS:  tun.DNSAddress,
				NetEcho: tun.EchoPort,
			})

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			hup := make(chan os.Signal, 1)
			signal.Notify(hup, syscall.SIGHUP)
			defer signal.Stop(hup)
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case <-hup:
						if err := d.Reload(ctx); err != nil {
							log.Print(err)
						}
					}
				}
			}()
			if err := d.Serve(ctx); err != nil {
				return fail(err)
			}
			return nil
		}),
	}
	cmd.Flags().StringVar(&logPath, "log-file", "", "write the log to this file, rotating it at 1 MiB")
	return cmd
}
