package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"text/template"
	"time"
)

type launchd struct {
	agentsDir string // ~/Library/LaunchAgents
	logPath   string // the daemon's own log
	domain    string // gui/<uid>
	run       runner
	poll      time.Duration // interval for waiting until an agent is unloaded
}

func newLaunchd(home string, uid int) *launchd {
	return &launchd{
		agentsDir: filepath.Join(home, "Library", "LaunchAgents"),
		logPath:   filepath.Join(home, "Library", "Logs", "gst-daemon.log"),
		domain:    "gui/" + strconv.Itoa(uid),
		run:       execRun,
		poll:      250 * time.Millisecond,
	}
}

const daemonLabel = "local.gst"

// legacyLabel is gst 0.1's agent for one tunnel.
func legacyLabel(name string) string { return "local.gst." + name }

func (l *launchd) target(label string) string { return l.domain + "/" + label }

func (l *launchd) plistPath(label string) string {
	return filepath.Join(l.agentsDir, label+".plist")
}

func (l *launchd) Install(command []string, restart bool) error {
	for _, dir := range []string{l.agentsDir, filepath.Dir(l.logPath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	command = append(slices.Clip(command), "--log-file", l.logPath)
	plist, err := launchdPlist(daemonLabel, command, l.logPath)
	if err != nil {
		return err
	}
	changed, err := writeIfChanged(l.plistPath(daemonLabel), plist)
	if err != nil {
		return err
	}
	if !changed && l.loaded(daemonLabel) {
		if restart {
			_, err := l.run("launchctl", "kickstart", "-k", l.target(daemonLabel))
			return err
		}
		return nil
	}
	// An agent disabled earlier, for example with launchctl disable,
	// refuses to load.
	if _, err := l.run("launchctl", "enable", l.target(daemonLabel)); err != nil {
		return err
	}
	// Reloading is the only way to apply a changed plist.
	if err := l.unload(daemonLabel); err != nil {
		return err
	}
	_, err = l.run("launchctl", "bootstrap", l.domain, l.plistPath(daemonLabel))
	return err
}

func (l *launchd) Uninstall() error { return l.remove(daemonLabel) }

func (l *launchd) HasLegacy(name string) bool { return fileExists(l.plistPath(legacyLabel(name))) }

func (l *launchd) RemoveLegacy(name string) error { return l.remove(legacyLabel(name)) }

func (l *launchd) remove(label string) error {
	if !fileExists(l.plistPath(label)) {
		return nil
	}
	if err := l.unload(label); err != nil {
		return err
	}
	return os.Remove(l.plistPath(label))
}

func (l *launchd) loaded(label string) bool {
	_, err := l.run("launchctl", "print", l.target(label))
	return err == nil
}

// unload boots the agent out and waits until it is gone, because loading
// it again before then fails.
func (l *launchd) unload(label string) error {
	if !l.loaded(label) {
		return nil
	}
	if _, err := l.run("launchctl", "bootout", l.target(label)); err != nil {
		return err
	}
	// ExitTimeOut gives the process 15 s to exit after SIGTERM.
	deadline := time.Now().Add(20 * time.Second)
	for l.loaded(label) {
		if time.Now().After(deadline) {
			return fmt.Errorf("launchd agent %s is still loaded", label)
		}
		time.Sleep(l.poll)
	}
	return nil
}

// The agent starts at login, restarts at least 5 s apart after every exit,
// and gets 15 s to exit after SIGTERM. The daemon writes and rotates its
// log itself; launchd sends output there only until the daemon opens it.
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
