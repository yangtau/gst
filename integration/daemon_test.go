//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// After gst is upgraded, the next command restarts the daemon with the new
// gst; without an upgrade, the daemon keeps running.
func TestDaemonRestartsAfterUpgrade(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	server.startEcho(3000)
	client.mustGst("link", "add", "srv", p.destination())
	client.mustGst("map", "add", "15432", "srv:3000")
	client.waitLink("srv", "running")
	before := client.daemonPID()

	// gst up makes the daemon apply the configuration, and nothing else.
	client.mustGst("up")
	if pid := client.daemonPID(); pid != before {
		t.Fatalf("daemon %s replaced by %s without an upgrade", before, pid)
	}

	// Replacing the file keeps the running daemon's executable intact; the
	// appended byte gives the new gst another build.
	if _, err := client.exec("root", "sh", "-c", `cp /usr/local/bin/gst /usr/local/bin/gst.new && printf x >>/usr/local/bin/gst.new && mv /usr/local/bin/gst.new /usr/local/bin/gst`); err != nil {
		t.Fatal(err)
	}
	client.mustGst("up")
	if pid := client.daemonPID(); pid == before || pid == "" {
		t.Fatalf("daemon %s not restarted after the upgrade (now %q)", before, pid)
	}
	client.waitLink("srv", "running")
	eventually(t, 20*time.Second, "the map did not work after the upgrade", func() bool {
		return client.roundtrip("127.0.0.1:15432") == nil
	})
}

// gst down stops everything on a machine, including accepting links, until
// gst up.
func TestDownAndUp(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	client.mustGst("link", "add", "srv", p.destination())
	client.waitLink("srv", "running")
	if server.daemonPID() == "" {
		t.Fatal("the link did not start the server's daemon")
	}

	if out := server.mustGst("down"); !strings.Contains(out, "gst is down") {
		t.Fatalf("unexpected gst down output %q", out)
	}
	eventually(t, 10*time.Second, "the daemon still runs after gst down", func() bool { return server.daemonPID() == "" })
	client.waitLink("srv", "down")

	// The link cannot come back: peer-bridge refuses, and says why.
	eventually(t, 60*time.Second, "the link does not report that gst is down", func() bool {
		return strings.Contains(client.links()["srv"].Error, "gst is down")
	})
	if server.daemonPID() != "" {
		t.Fatal("peer-bridge started a daemon on a machine that is down")
	}
	out := client.mustSh(`ssh -i "$1" -o UserKnownHostsFile="$2" -o BatchMode=yes "tunnel@$3" gst peer-bridge </dev/null 2>&1; true`,
		p.identity, p.knownHosts, server.ip)
	if !strings.Contains(out, "gst is down") {
		t.Fatalf("gst peer-bridge on a machine that is down: %q", out)
	}

	server.mustGst("up")
	client.waitLink("srv", "running")
	if server.daemonPID() == "" {
		t.Fatal("no daemon after gst up")
	}
	cfg := server.mustSh(`cat ~/.config/gst/config.json`)
	var parsed struct {
		Down bool `json:"down"`
	}
	if err := json.Unmarshal([]byte(cfg), &parsed); err != nil || parsed.Down {
		t.Fatalf("configuration after gst up: %v\n%s", err, cfg)
	}

	// Down on the client stops its links and its proxy.
	client.mustGst("down")
	eventually(t, 10*time.Second, "the client's daemon still runs", func() bool { return client.daemonPID() == "" })
	code, _, stderr := client.gst("status")
	if code != 1 || !strings.Contains(stderr, "the gst daemon does not answer") || !strings.Contains(stderr, "gst up") {
		t.Fatalf("gst status while gst is down: exit %d, stderr %q", code, stderr)
	}
	client.mustGst("up")
	client.waitLink("srv", "running")
}
