//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

func TestExpose(t *testing.T) {
	t.Parallel()
	a, b, c, _, _ := start3(t)
	for _, n := range []*node{a, b, c} {
		n.installHelper()
	}
	for _, n := range []*node{a, b, c} {
		n.waitNet(2)
	}
	a.waitPing("c.gst")
	c.startEchoOn("127.0.0.1:8080")
	c.mustSh(`setsid gst-it udp-echo 127.0.0.1:8080 </dev/null >/dev/null 2>&1 &`)
	c.mustSh(`setsid gst-it echo-server unix:///tmp/service.sock </dev/null >/dev/null 2>&1 &`)
	eventually(t, 5*time.Second, "Unix backend did not start", func() bool { _, err := c.sh(`test -S /tmp/service.sock`); return err == nil })
	c.mustGst("expose", "8080", "--port", "5999")
	c.mustGst("expose", "8080", "--port", "5999", "--udp")
	c.mustGst("expose", "unix:///tmp/service.sock", "--port", "6000")
	c.mustGst("expose", "8080", "--port", "5999") // idempotent
	check := func() {
		t.Helper()
		eventually(t, 15*time.Second, "exposed TCP/Unix ports did not answer", func() bool { return a.roundtrip("-conns", "4", "c.gst:5999", "c.gst:6000") == nil })
		if err := c.roundtrip("c.gst:5999"); err != nil {
			t.Fatalf("local gst address: %v", err)
		}
		out, err := a.helper("udp", "c.gst:5999", "hello")
		if err != nil || strings.TrimSpace(out) != "reply:hello" {
			t.Fatalf("UDP: %q %v", out, err)
		}
	}
	check()
	if _, err := a.sh(`nc -z -w 1 "$1" 5999`, c.ip); err == nil {
		t.Fatal("TCP exposed on physical IP")
	}
	if _, err := a.helper("udp", c.addr(5999), "hidden"); err == nil {
		t.Fatal("UDP exposed on physical IP")
	}
	var states []wire.ExposeState
	if err := json.Unmarshal([]byte(c.mustGst("expose", "list", "--json")), &states); err != nil || len(states) != 3 {
		t.Fatalf("states: %+v %v", states, err)
	}
	for _, s := range states {
		if s.State != "running" || !strings.HasPrefix(s.Address, c.net().Address+":") {
			t.Fatal(s)
		}
	}
	c.killDaemon()
	c.mustGst("up")
	c.waitNet(2)
	check()
	old := c.net().Address
	c.mustGst("subnet", "10.99.0.0/16")
	for _, n := range []*node{a, b, c} {
		n.waitNet(2)
	}
	check()
	if old == c.net().Address {
		t.Fatal("subnet did not change the address")
	}
	c.mustGst("expose", "remove", "5999", "--udp")
	if err := a.roundtrip("c.gst:5999"); err != nil {
		t.Fatal("UDP removal stopped TCP:", err)
	}
	if _, err := a.helper("udp", "c.gst:5999", "removed"); err == nil {
		t.Fatal("removed UDP still answered")
	}
	c.mustGst("expose", "remove", "5999")
	if _, err := a.sh(`nc -z -w 1 c.gst 5999`); err == nil {
		t.Fatal("removed TCP still answered")
	}
}
