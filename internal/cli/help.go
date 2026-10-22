package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func setHelpColors(root *cobra.Command) {
	plainHelp := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		out := cmd.OutOrStdout()
		if !helpUsesColor(out) {
			plainHelp(cmd, args)
			return
		}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		defer cmd.SetOut(out)
		plainHelp(cmd, args)
		fmt.Fprint(out, colorHelp(buf.String()))
	})
}

func helpUsesColor(out io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	file, ok := out.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

// Style after Cobra lays out the help, so escape codes cannot affect alignment.
func colorHelp(help string) string {
	lines := strings.Split(help, "\n")
	for i, line := range lines {
		body := strings.TrimLeft(line, " ")
		if body == "" {
			continue
		}
		if body == line {
			if strings.HasSuffix(line, ":") {
				lines[i] = "\x1b[1m" + line + "\x1b[0m"
			}
			continue
		}
		if label, description, ok := strings.Cut(body, "  "); ok {
			indent := line[:len(line)-len(body)]
			lines[i] = indent + "\x1b[36m" + label + "\x1b[0m  " + description
		}
	}
	return strings.Join(lines, "\n")
}
