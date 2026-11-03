//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The daemon runs a tunnel between two containers through its whole life:
// add, change, stop, start, reconnect, and remove.
func TestDaemonLifecycle(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	server.serve(3000, "pong")
	dest := p.flags()

	client.mustGst(append([]string{"add", "srv"}, append(dest, "-L", "15432:127.0.0.1:3000")...)...)
	client.waitStatus("srv", "running")
	client.eventuallyFetches(15432, "pong")

	// The log records the tunnel but, for privacy, not the server.
	log := client.mustSh(`cat ~/.local/state/gst/logs/srv.log`)
	if !strings.Contains(log, "tunnel ready") || strings.Contains(log, server.ip) {
		t.Fatalf("unexpected tunnel log:\n%s", log)
	}

	// Adding a forward restarts the tunnel with it.
	if out := client.mustGst("forward", "add", "srv", "-L", "15433:127.0.0.1:3000"); !strings.Contains(out, "Restarted tunnel srv") {
		t.Fatalf("unexpected output %q", out)
	}
	client.waitStatus("srv", "running")
	client.eventuallyFetches(15433, "pong")

	// Stopping the last tunnel removes the daemon.
	client.mustGst("stop", "srv")
	if client.tunnels()["srv"].Status != "stopped" || client.daemonPID() != "" {
		t.Fatalf("daemon %q still runs after stop", client.daemonPID())
	}
	if _, err := client.fetch(15432); err == nil {
		t.Fatal("forward still listens after stop")
	}
	client.mustGst("start", "srv")
	client.waitStatus("srv", "running")
	client.eventuallyFetches(15432, "pong")

	// The daemon reconnects after the server drops the session.
	pid := client.daemonPID()
	readyLines := func() int { return strings.Count(client.mustSh(`cat ~/.local/state/gst/logs/srv.log`), "tunnel ready") }
	before := readyLines()
	server.signalSessions("TERM")
	eventually(t, 30*time.Second, "the tunnel did not reconnect", func() bool { return readyLines() > before })
	client.eventuallyFetches(15432, "pong")
	if client.daemonPID() != pid {
		t.Fatal("the daemon restarted instead of reconnecting the tunnel")
	}

	client.mustGst("remove", "srv")
	if client.daemonPID() != "" {
		t.Fatal("daemon still runs after removing the last tunnel")
	}
	if _, err := client.sh(`test -e ~/.local/state/gst/logs/srv.log`); err == nil {
		t.Fatal("log remains after remove")
	}
}

// A configuration from gst 0.1 moves to the daemon: tunnels with a
// per-tunnel service keep running, the others become stopped.
func TestDaemonMigratesLegacyServices(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	server.serve(3000, "pong")
	dest := p.flags()

	// Write the 0.1 configuration with gst itself, then drop the version
	// and leave a 0.1 unit for "srv" only.
	client.mustGst(append([]string{"add", "srv"}, append(dest, "-L", "15432:127.0.0.1:3000")...)...)
	client.mustGst(append([]string{"add", "idle"}, dest...)...)
	client.mustGst("stop", "--all")
	var cfg map[string]any
	if err := json.Unmarshal([]byte(client.mustSh(`cat ~/.config/gst/config.json`)), &cfg); err != nil {
		t.Fatal(err)
	}
	delete(cfg, "version")
	for _, tunnel := range cfg["tunnels"].(map[string]any) {
		delete(tunnel.(map[string]any), "stopped")
	}
	legacy, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client.mustSh(`printf '%s' "$1" >~/.config/gst/config.json && printf '[Service]\nExecStart=/bin/true\n' >~/.config/systemd/user/gst-srv.service`, string(legacy))

	code, _, stderr := client.gst("list")
	if code != 0 || !strings.Contains(stderr, "the gst daemon now runs tunnels srv") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	client.waitStatus("srv", "running")
	client.eventuallyFetches(15432, "pong")
	if status := client.tunnels()["idle"].Status; status != "stopped" {
		t.Fatalf("idle is %s, want stopped", status)
	}
	if _, err := client.sh(`test -e ~/.config/systemd/user/gst-srv.service`); err == nil {
		t.Fatal("the 0.1 unit remains")
	}
	if config := client.mustSh(`cat ~/.config/gst/config.json`); !strings.Contains(config, `"version": 2`) {
		t.Fatalf("configuration not migrated:\n%s", config)
	}
}

// After gst is upgraded, the next command restarts the daemon with the new
// gst; without an upgrade, the daemon keeps running.
func TestDaemonRestartsAfterUpgrade(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	server.serve(3000, "pong")
	dest := p.flags()
	client.mustGst(append([]string{"add", "srv"}, append(dest, "-L", "15432:127.0.0.1:3000")...)...)
	client.waitStatus("srv", "running")
	before := client.daemonPID()

	client.mustGst("restart", "srv")
	if pid := client.daemonPID(); pid != before {
		t.Fatalf("daemon %s replaced by %s without an upgrade", before, pid)
	}

	// Replacing the file keeps the running daemon's executable intact; the
	// appended byte gives the new gst another build.
	if _, err := client.exec("root", "sh", "-c", `cp /usr/local/bin/gst /usr/local/bin/gst.new && printf x >>/usr/local/bin/gst.new && mv /usr/local/bin/gst.new /usr/local/bin/gst`); err != nil {
		t.Fatal(err)
	}
	client.mustGst("restart", "--all")
	if pid := client.daemonPID(); pid == before || pid == "" {
		t.Fatalf("daemon %s not restarted after the upgrade (now %q)", before, pid)
	}
	client.waitStatus("srv", "running")
	client.eventuallyFetches(15432, "pong")
}
