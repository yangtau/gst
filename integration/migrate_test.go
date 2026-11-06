//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
)

// A version 3 configuration is converted by the first command: the links
// stay, and what gst no longer has is dropped with a note each.
func TestMigrateVersion3Config(t *testing.T) {
	t.Parallel()
	p := startPair(t)
	client := p.client

	legacy := fmt.Sprintf(`{
  "version": 3,
  "proxy": "127.0.0.1:1080",
  "links": {"srv": {"destination": %q}},
  "maps": [{"listen": "127.0.0.1:15432", "machine": "srv", "target": "localhost:3000"}],
  "routes": [{"match": "default", "via": "srv"}]
}`, p.destination())
	client.mustSh(`mkdir -p ~/.config/gst && printf '%s' "$1" >~/.config/gst/config.json`, legacy)

	code, _, stderr := client.gst("status")
	if code != 0 {
		t.Fatalf("gst status exited %d: %s", code, stderr)
	}
	for _, want := range []string{"<machine>.gst", "dropped proxy 127.0.0.1:1080", "dropped map 127.0.0.1:15432 to localhost:3000 on srv", "dropped route default via srv"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("notes lack %q:\n%s", want, stderr)
		}
	}
	client.waitLink("srv", "running")

	// The file is rewritten, so the notes appear once.
	if cfg := client.mustSh(`cat ~/.config/gst/config.json`); !strings.Contains(cfg, `"version": 4`) || strings.Contains(cfg, "maps") {
		t.Fatalf("configuration not converted:\n%s", cfg)
	}
	if _, _, stderr := client.gst("status"); strings.Contains(stderr, "note:") {
		t.Fatalf("notes repeated:\n%s", stderr)
	}
}
