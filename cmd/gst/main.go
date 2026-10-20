// Command gst keeps SSH port forwards running in the background.
package main

import (
	"os"

	"github.com/yangtau/ssh-tunnel/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:]))
}
