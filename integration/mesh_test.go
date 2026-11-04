//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Three machines a -> b <- c, where a and c each link to b and know
// nothing of each other: a reaches c through b. The subtests share the
// machines and run in order.
func TestMesh(t *testing.T) {
	t.Parallel()
	a, b, c, _, _ := start3(t)
	c.startEcho(7000)
	c.startEchoOn("/tmp/echo.sock")
	c.mustSh(`setsid gst-it http-server 127.0.0.1:8000 'hello from c' </dev/null >/dev/null 2>&1 &`)
	c.waitListening(8000)
	b.startEcho(7100)

	t.Run("Topology", func(t *testing.T) {
		cNode, topo := a.waitReachable("c")
		if got := path(topo, cNode); got != "a,b,c" {
			t.Fatalf("a reaches c by %s, want a,b,c", got)
		}
		if len(topo.Nodes) != 3 || len(topo.Edges) != 2 {
			t.Fatalf("a sees %+v", topo)
		}
		bNode, _ := find(topo, "b")
		if got := path(topo, bNode); got != "a,b" || !strings.Contains(bNode.Software, "gst") {
			t.Fatalf("a reaches b by %s as %q", got, bNode.Software)
		}
		out := a.mustGst("nodes")
		if !strings.Contains(out, "this machine") || !strings.Contains(out, "b → c") {
			t.Fatalf("unexpected gst nodes:\n%s", out)
		}
		// The other end learns of a the same way.
		aNode, topo := c.waitReachable("a")
		if got := path(topo, aNode); got != "c,b,a" {
			t.Fatalf("c reaches a by %s, want c,b,a", got)
		}
	})

	t.Run("Connect", func(t *testing.T) {
		// gst connect joins standard input and output to the target.
		for _, machine := range []string{"c", "c.gst", "C"} {
			if out := a.mustSh(`printf hello | gst connect "$1" 7000`, machine); out != "reply:hello" {
				t.Fatalf("gst connect %s 7000: %q", machine, out)
			}
		}
		if out := a.mustSh(`printf hello | gst connect c localhost:7000`); out != "reply:hello" {
			t.Fatalf("gst connect c localhost:7000: %q", out)
		}
		if out := a.mustSh(`printf hello | gst connect c /tmp/echo.sock`); out != "reply:hello" {
			t.Fatalf("gst connect c /tmp/echo.sock: %q", out)
		}

		// A machine is named by its link, or by an id prefix.
		if out := a.mustSh(`printf hello | gst connect tob 7100`); out != "reply:hello" {
			t.Fatalf("gst connect tob 7100: %q", out)
		}
		cNode, _ := find(a.nodes(), "c")
		for _, ref := range []string{cNode.ID, cNode.ID[:8], cNode.ID[:4] + ".gst"} {
			if out := a.mustSh(`printf hello | gst connect "$1" 7000`, ref); out != "reply:hello" {
				t.Fatalf("gst connect %s 7000: %q", ref, out)
			}
		}

		// Failures are reported, not hung on.
		code, _, stderr := a.gst("connect", "nowhere", "7000")
		if code != 1 || !strings.Contains(stderr, "error:") {
			t.Fatalf("gst connect nowhere: exit %d, stderr %q", code, stderr)
		}
	})

	t.Run("Maps", func(t *testing.T) {
		a.mustGst("map", "add", "15432", "c:7000")
		a.mustGst("map", "add", "15433", "c:/tmp/echo.sock")
		a.mustGst("map", "add", "15434", "tob:7100")
		for _, port := range []int{15432, 15433, 15434} {
			addr := fmt.Sprintf("127.0.0.1:%d", port)
			eventually(t, 10*time.Second, addr+" did not round-trip", func() bool {
				return a.roundtrip("-conns", "4", "-repeat", "1000", addr) == nil
			})
		}
		out := a.mustGst("map", "list")
		for _, want := range []string{"15432", "c", "7000", "/tmp/echo.sock", "listening"} {
			if !strings.Contains(out, want) {
				t.Fatalf("gst map list lacks %q:\n%s", want, out)
			}
		}

		a.mustGst("map", "remove", "15432", "15433", "15434")
		eventually(t, 10*time.Second, "the map still listens", func() bool {
			_, err := a.sh(`nc -z 127.0.0.1 15432`)
			return err != nil
		})
	})

	t.Run("Proxy", func(t *testing.T) {
		const proxy = "127.0.0.1:1080"
		out := a.mustGst("proxy")
		if !strings.Contains(out, proxy) || !strings.Contains(out, "listening") || !strings.Contains(out, "/proxy.pac") {
			t.Fatalf("unexpected gst proxy:\n%s", out)
		}

		// SOCKS5 and HTTP CONNECT reach localhost on c through <machine>.gst.
		if out, err := a.helper("socks", proxy, "c.gst", "7000", "hello"); err != nil || out != "reply:hello" {
			t.Fatalf("SOCKS to c.gst: %q, %v", out, err)
		}
		if out, err := a.helper("connect", proxy, "c.gst", "7000", "hello"); err != nil || out != "reply:hello" {
			t.Fatalf("CONNECT to c.gst: %q, %v", out, err)
		}

		// A plain HTTP request is forwarded with its absolute URL.
		out, err := a.exec(nodeUser, "env", "HTTP_PROXY=http://"+proxy, "gst-it", "http", "GET", "http://c.gst:8000/")
		if err != nil || out != "200\nhello from c" {
			t.Fatalf("GET through the proxy: %q, %v", out, err)
		}

		// The same address serves the PAC file for browsers.
		out, err = a.helper("http", "GET", "http://"+proxy+"/proxy.pac")
		if err != nil || !strings.HasPrefix(out, "200\n") || !strings.Contains(out, `".gst"`) {
			t.Fatalf("proxy.pac: %q, %v", out, err)
		}

		// A machine that does not exist fails the connection.
		if _, err := a.helper("socks", proxy, "nowhere.gst", "7000", "hello"); err == nil || !strings.Contains(err.Error(), "SOCKS reply") {
			t.Fatalf("SOCKS to nowhere.gst: %v", err)
		}

		// The proxy moves and turns off.
		a.mustGst("proxy", "127.0.0.1:1081")
		eventually(t, 10*time.Second, "the proxy did not move", func() bool {
			out, err := a.helper("socks", "127.0.0.1:1081", "c.gst", "7000", "hello")
			return err == nil && out == "reply:hello"
		})
		if _, err := a.helper("socks", proxy, "c.gst", "7000", "hello"); err == nil {
			t.Fatal("the old proxy address still answers")
		}
		a.mustGst("proxy", "127.0.0.1:1080")
		a.mustGst("proxy", "off")
		eventually(t, 10*time.Second, "the proxy did not turn off", func() bool {
			_, err := a.helper("socks", "127.0.0.1:1080", "c.gst", "7000", "hello")
			return err != nil
		})
		a.mustGst("proxy", "127.0.0.1:1080")
	})

	// c reaches a service on b's address, which is not on c itself, so c
	// must be an exit to connect there for a.
	t.Run("Exit", func(t *testing.T) {
		const proxy = "127.0.0.1:1080"
		b.startEchoOn("0.0.0.0:7200")
		target := b.addr(7200)

		_, _, stderr := a.gst("route", "add", b.ip+"/32", "c")
		if !strings.Contains(stderr, "c is not an exit yet") {
			t.Fatalf("route add did not note that c is no exit: %q", stderr)
		}
		if out := a.mustGst("route", "list"); !strings.Contains(out, b.ip+"/32") {
			t.Fatalf("unexpected gst route list:\n%s", out)
		}

		// Everything through c to b's address is refused while c is no exit.
		_, err := a.helper("socks", proxy, b.ip, "7200", "hello")
		if err == nil || !strings.Contains(err.Error(), "SOCKS reply code 2 ") {
			t.Fatalf("SOCKS to %s through c: %v, want the not-allowed reply", b.ip, err)
		}
		_, err = a.helper("connect", proxy, b.ip, "7200", "hello")
		if err == nil || !strings.Contains(err.Error(), "is not an exit") {
			t.Fatalf("CONNECT to %s through c: %v", b.ip, err)
		}
		code, _, stderr := a.gst("connect", "c", target)
		if code != 1 || !strings.Contains(stderr, "is not an exit") || !strings.Contains(stderr, "gst exit on") {
			t.Fatalf("gst connect c %s: exit %d, stderr %q", target, code, stderr)
		}
		a.mustGst("map", "add", "15435", "c:"+target)
		if err := a.roundtrip("127.0.0.1:15435"); err == nil {
			t.Fatal("a map through a non-exit reached the address")
		}
		// Its own address stays allowed.
		if out := a.mustSh(`printf hello | gst connect c 127.0.0.1:7000`); out != "reply:hello" {
			t.Fatalf("gst connect c 127.0.0.1:7000: %q", out)
		}

		c.mustGst("exit", "on")
		eventually(t, 30*time.Second, "a does not see that c is an exit", func() bool {
			v, _ := find(a.nodes(), "c")
			return v.Exit
		})
		eventually(t, 10*time.Second, "SOCKS through the exit", func() bool {
			out, err := a.helper("socks", proxy, b.ip, "7200", "hello")
			return err == nil && out == "reply:hello"
		})
		if out, err := a.helper("connect", proxy, b.ip, "7200", "hello"); err != nil || out != "reply:hello" {
			t.Fatalf("CONNECT through the exit: %q, %v", out, err)
		}
		if out := a.mustSh(`printf hello | gst connect c "$1"`, target); out != "reply:hello" {
			t.Fatalf("gst connect through the exit: %q", out)
		}
		if err := a.roundtrip("127.0.0.1:15435"); err != nil {
			t.Fatalf("map through the exit: %v", err)
		}

		// Without the route, the proxy connects to b's address itself, and
		// c is an exit no longer.
		a.mustGst("route", "remove", b.ip+"/32")
		if out, err := a.helper("socks", proxy, b.ip, "7200", "hello"); err != nil || out != "reply:hello" {
			t.Fatalf("direct SOCKS: %q, %v", out, err)
		}
		c.mustGst("exit", "off")
		code, _, stderr = a.gst("connect", "c", target)
		if code != 1 || !strings.Contains(stderr, "is not an exit") {
			t.Fatalf("gst connect c %s after exit off: exit %d, stderr %q", target, code, stderr)
		}
	})
}
