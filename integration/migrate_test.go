//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A version 2 configuration is converted by the first command: the tunnel
// becomes a link, its -L forward a map, and its -R forward is dropped with
// a note. The tunnel's own ssh settings are dropped: the destination, an
// alias of the ssh configuration, supplies them.
func TestMigrateVersion2Config(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client, server := p.client, p.server
	server.startEcho(3000)

	legacy := fmt.Sprintf(`{
  "version": 2,
  "tunnels": {
    "srv": {
      "destination": %q,
      "identity_file": %q,
      "known_hosts_file": %q,
      "connect_timeout_seconds": 10,
      "keepalive_interval_seconds": 1,
      "keepalive_count_max": 2,
      "forwards": [
        {"direction": "local", "listen": "127.0.0.1:15432", "target": "127.0.0.1:3000"},
        {"direction": "remote", "listen": "0.0.0.0:2222", "target": "127.0.0.1:3000"}
      ]
    }
  }
}`, p.destination(), p.identity, p.knownHosts)
	client.mustSh(`mkdir -p ~/.config/gst && printf '%s' "$1" >~/.config/gst/config.json`, legacy)

	code, _, stderr := client.gst("status")
	if code != 0 {
		t.Fatalf("gst status exited %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "gst 0.3 replaced tunnels with links") || !strings.Contains(stderr, "dropped tunnel srv: -R 0.0.0.0:2222:127.0.0.1:3000") {
		t.Fatalf("unexpected notes:\n%s", stderr)
	}
	if strings.Contains(stderr, "dropped tunnel srv: -L") {
		t.Fatalf("the -L forward was dropped:\n%s", stderr)
	}

	// The link runs and the map works.
	client.waitLink("srv", "running")
	var maps []struct {
		Listen  string `json:"listen"`
		Machine string `json:"machine"`
		Target  string `json:"target"`
	}
	if err := json.Unmarshal([]byte(client.mustGst("map", "list", "--json")), &maps); err != nil {
		t.Fatal(err)
	}
	if len(maps) != 1 || maps[0].Listen != "127.0.0.1:15432" || maps[0].Machine != "srv" || maps[0].Target != "127.0.0.1:3000" {
		t.Fatalf("maps after the upgrade: %+v", maps)
	}
	eventually(t, 20*time.Second, "the converted map did not work", func() bool {
		return client.roundtrip("127.0.0.1:15432") == nil
	})

	// The file is rewritten, so the notes appear once.
	if cfg := client.mustSh(`cat ~/.config/gst/config.json`); !strings.Contains(cfg, `"version": 3`) {
		t.Fatalf("configuration not converted:\n%s", cfg)
	}
	if _, _, stderr := client.gst("status"); strings.Contains(stderr, "note:") {
		t.Fatalf("notes repeated:\n%s", stderr)
	}
}
