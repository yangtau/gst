package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"text/template"
	"time"
)

type launchd struct {
	agentsDir string // ~/Library/LaunchAgents
	logDir    string // ~/Library/Logs/gst
	domain    string // gui/<uid>
	run       runner
	stream    streamer
	poll      time.Duration // interval for waiting until an agent is unloaded
}

func newLaunchd(home string, uid int) *launchd {
	return &launchd{
		agentsDir: filepath.Join(home, "Library", "LaunchAgents"),
		logDir:    filepath.Join(home, "Library", "Logs", "gst"),
		domain:    "gui/" + strconv.Itoa(uid),
		run:       execRun,
		stream:    execStream,
		poll:      250 * time.Millisecond,
	}
}

func label(name string) string { return "local.gst." + name }

func (l *launchd) target(name string) string { return l.domain + "/" + label(name) }

func (l *launchd) plistPath(name string) string {
	return filepath.Join(l.agentsDir, label(name)+".plist")
}

func (l *launchd) logPath(name string) string { return filepath.Join(l.logDir, name+".log") }

func (l *launchd) Start(name string, command []string) error {
	for _, dir := range []string{l.agentsDir, l.logDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	plist, err := launchdPlist(label(name), command, l.logPath(name))
	if err != nil {
		return err
	}
	if err := os.WriteFile(l.plistPath(name), plist, 0o644); err != nil {
		return err
	}
	// An agent disabled earlier, for example with launchctl disable,
	// refuses to load.
	if _, err := l.run("launchctl", "enable", l.target(name)); err != nil {
		return err
	}
	// Reloading is the only way to apply a changed plist.
	if err := l.unload(name); err != nil {
		return err
	}
	_, err = l.run("launchctl", "bootstrap", l.domain, l.plistPath(name))
	return err
}

func (l *launchd) Stop(name string) error {
	if !fileExists(l.plistPath(name)) {
		return nil
	}
	if err := l.unload(name); err != nil {
		return err
	}
	return os.Remove(l.plistPath(name))
}

func (l *launchd) loaded(name string) bool {
	_, err := l.run("launchctl", "print", l.target(name))
	return err == nil
}

// unload boots the agent out and waits until it is gone, because loading
// it again before then fails.
func (l *launchd) unload(name string) error {
	if !l.loaded(name) {
		return nil
	}
	if _, err := l.run("launchctl", "bootout", l.target(name)); err != nil {
		return err
	}
	// ExitTimeOut gives the tunnel 15 s to exit after SIGTERM.
	deadline := time.Now().Add(20 * time.Second)
	for l.loaded(name) {
		if time.Now().After(deadline) {
			return fmt.Errorf("launchd agent %s is still loaded", label(name))
		}
		time.Sleep(l.poll)
	}
	return nil
}

var launchdState = regexp.MustCompile(`(?m)^\tstate = (\S+)$`)

func (l *launchd) Status(name string) (Status, error) {
	if !fileExists(l.plistPath(name)) {
		return Stopped, nil
	}
	out, err := l.run("launchctl", "print", l.target(name))
	if err != nil {
		return Down, nil
	}
	if m := launchdState.FindStringSubmatch(out); m != nil && m[1] == "running" {
		return Running, nil
	}
	return Down, nil
}

func (l *launchd) Logs(ctx context.Context, name string, lines int, follow bool, w io.Writer) error {
	path := l.logPath(name)
	if !follow && !fileExists(path) {
		return nil
	}
	args := []string{"-n", strconv.Itoa(lines)}
	if follow {
		args = append(args, "-F")
	}
	return l.stream(ctx, w, "tail", append(args, path)...)
}

// The agent starts at login, restarts at least 5 s apart after every exit,
// and gets 15 s to exit after SIGTERM. Its output goes to a log file that
// is not rotated; the tunnel logs little.
var plistTemplate = template.Must(template.New("plist").Funcs(template.FuncMap{"xml": xmlEscape}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{xml .Label}}</string>
	<key>ProgramArguments</key>
	<array>
{{- range .Command}}
		<string>{{xml .}}</string>
{{- end}}
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>5</integer>
	<key>ExitTimeOut</key>
	<integer>15</integer>
	<key>StandardOutPath</key>
	<string>{{xml .Log}}</string>
	<key>StandardErrorPath</key>
	<string>{{xml .Log}}</string>
</dict>
</plist>
`))

func launchdPlist(label string, command []string, log string) ([]byte, error) {
	var buf bytes.Buffer
	err := plistTemplate.Execute(&buf, struct {
		Label   string
		Command []string
		Log     string
	}{label, command, log})
	return buf.Bytes(), err
}

func xmlEscape(s string) string {
	var buf bytes.Buffer
	xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
