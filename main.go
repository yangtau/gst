// Command reverse-ssh-tunnel runs the reverse SSH tunnel described by a
// JSON configuration file until it fails or receives SIGINT/SIGTERM.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"reverse-ssh-tunnel/internal/config"
	"reverse-ssh-tunnel/internal/tunnel"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) != 1 {
		log.Print("usage: reverse-ssh-tunnel <config.json>")
		return exitUsage
	}
	cfg, err := config.Load(args[0])
	if err != nil {
		log.Print(err)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := tunnel.Run(ctx, cfg); err != nil && ctx.Err() == nil {
		log.Print(err)
		return exitFailure
	}
	return exitOK
}
