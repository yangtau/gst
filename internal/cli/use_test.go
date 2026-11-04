package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// --- proxy ---

func TestProxyShowsDefault(t *testing.T) {
	e := newEnv(t)
	out := e.mustGst("proxy")
	contains(t, out,
		"Proxy: 127.0.0.1:1080 (gst is not running; start it with gst up)\n",
		"ALL_PROXY=socks5h://127.0.0.1:1080 http_proxy=http://127.0.0.1:1080 https_proxy=http://127.0.0.1:1080",
		"http://127.0.0.1:1080/proxy.pac",
		"Host *.gst",
		"ProxyCommand gst connect %h %p")
	if len(e.back.calls) != 0 {
		t.Fatalf("showing the proxy reached the daemon: %v", e.back.calls)
	}

	e.mustGst("up")
	contains(t, e.mustGst("proxy"), "Proxy: 127.0.0.1:1080 (listening)\n")
	v := decode[proxyView](t, e.mustGst("proxy", "--json"))
	if v != (proxyView{Address: "127.0.0.1:1080", Status: statusListening}) {
		t.Fatalf("view %+v", v)
	}
}

func TestProxySet(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	e.resetCalls()

	out := e.mustGst("proxy", "1081")
	contains(t, out, "Proxy: 127.0.0.1:1081 (listening)\n", "socks5h://127.0.0.1:1081")
	if got := e.load().Proxy; got != "127.0.0.1:1081" {
		t.Fatalf("saved %q", got)
	}
	if !slices.Equal(e.back.calls, []string{"sync"}) {
		t.Fatalf("calls %v", e.back.calls)
	}

	// The default address is not stored.
	e.mustGst("proxy", "localhost:1080")
	if got := e.load().Proxy; got != "" {
		t.Fatalf("saved %q for the default address", got)
	}

	// All interfaces are reachable through loopback.
	out = e.mustGst("proxy", "*:1090")
	contains(t, out, "Proxy: 0.0.0.0:1090 (listening)\n", "socks5h://127.0.0.1:1090")

	v := decode[proxyView](t, e.mustGst("proxy", "1085", "--json"))
	if v.Address != "127.0.0.1:1085" || v.Status != statusListening {
		t.Fatalf("view %+v", v)
	}
}

func TestProxyOff(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	e.resetCalls()
	out := e.mustGst("proxy", "off")
	if want := "The proxy is off. Turn it on with: gst proxy 127.0.0.1:1080\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if e.load().Proxy != config.ProxyOff || !slices.Equal(e.back.calls, []string{"sync"}) {
		t.Fatalf("proxy %q, calls %v", e.load().Proxy, e.back.calls)
	}
	if out := e.mustGst("proxy"); !strings.Contains(out, "The proxy is off.") {
		t.Fatalf("output %q", out)
	}
	if v := decode[proxyView](t, e.mustGst("proxy", "--json")); v != (proxyView{}) {
		t.Fatalf("view %+v", v)
	}
	// A map may then use the default proxy address.
	e.mustGst("map", "add", "1080", "devcn:80")
	e.mustGst("map", "rm", "1080")
	e.mustGst("proxy", "127.0.0.1:1080")
	if e.load().Proxy != "" {
		t.Fatalf("proxy %q", e.load().Proxy)
	}
}

func TestProxyRejects(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{
		{"proxy", "/tmp/proxy.sock"},
		{"proxy", "bogus"},
		{"proxy", "0"},
		{"proxy", "70000"},
		{"proxy", "1081", "extra"},
	} {
		if stderr := e.expectCode(exitUsage, args...); !strings.Contains(stderr, "usage: gst proxy [address | off]") {
			t.Errorf("gst %v: %q", args, stderr)
		}
	}
	e.expectError(exitUsage, []string{"proxy", "/tmp/proxy.sock"}, "error: the proxy listens on a TCP port, not a socket\n")
	if len(e.back.calls) != 0 || e.load().Proxy != "" {
		t.Fatalf("a rejected proxy changed state: %v %q", e.back.calls, e.load().Proxy)
	}
}

func TestProxyConflictsWithMap(t *testing.T) {
	e := newEnv(t)
	e.mustGst("map", "add", "1081", "devcn:5432")
	e.resetCalls()
	e.expectError(exitFailure, []string{"proxy", "1081"},
		"error: a map listens on 127.0.0.1:1081\n", "hint: remove it with `gst map remove 127.0.0.1:1081`, or choose another address\n")
	if e.load().Proxy != "" || len(e.back.calls) != 0 {
		t.Fatalf("a conflict changed state: %q %v", e.load().Proxy, e.back.calls)
	}
}

func TestProxyListenFailure(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	e.back.listenErrors["127.0.0.1:1081"] = "address already in use"
	e.expectError(exitFailure, []string{"proxy", "1081"},
		"error: cannot listen on 127.0.0.1:1081: address already in use\n", "hint: choose another address, or free this one and run `gst up`\n")
	e.expectCode(exitFailure, "proxy", "1081", "--json")

	// Showing it later reports the failure without failing.
	out := e.mustGst("proxy")
	contains(t, out, "Proxy: 127.0.0.1:1081 (failed: address already in use)\n")
	v := decode[proxyView](t, e.mustGst("proxy", "--json"))
	if v.Status != statusFailed || v.Error != "address already in use" {
		t.Fatalf("view %+v", v)
	}
}

// --- map ---

func TestMapAdd(t *testing.T) {
	e := newEnv(t)
	out := e.mustGst("map", "add", "15432", "DevCN.gst:5432")
	if want := "Mapped 127.0.0.1:15432 to devcn:localhost:5432.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if !e.back.installed || !slices.Equal(e.back.calls, []string{"sync"}) {
		t.Fatalf("installed %v, calls %v", e.back.installed, e.back.calls)
	}
	e.mustGst("map", "add", "0.0.0.0:8080", "devcn:db.internal:80")
	e.mustGst("map", "add", "*:8081", "devcn:/run/app.sock")
	e.mustGst("map", "add", "/tmp/devcn-docker.sock", "devcn:/var/run/docker.sock")
	e.mustGst("map", "add", "localhost:18082", "far:8082")

	want := []config.Map{
		{Listen: "127.0.0.1:15432", Machine: "devcn", Target: "localhost:5432"},
		{Listen: "0.0.0.0:8080", Machine: "devcn", Target: "db.internal:80"},
		{Listen: "0.0.0.0:8081", Machine: "devcn", Target: "/run/app.sock"},
		{Listen: "/tmp/devcn-docker.sock", Machine: "devcn", Target: "/var/run/docker.sock"},
		{Listen: "127.0.0.1:18082", Machine: "far", Target: "localhost:8082"},
	}
	if got := e.load().Maps; !slices.Equal(got, want) {
		t.Fatalf("maps\n got %+v\nwant %+v", got, want)
	}
}

func TestMapListenNormalization(t *testing.T) {
	e := newEnv(t)
	e.mustGst("map", "add", "15432", "devcn:5432")
	// The same listen address, spelled differently, is a duplicate.
	for _, spelling := range []string{"15432", "127.0.0.1:15432", "localhost:15432"} {
		e.expectError(exitFailure, []string{"map", "add", spelling, "far:80"},
			"error: 127.0.0.1:15432 is already mapped to devcn:localhost:5432\n", "hint: remove it first with `gst map remove 127.0.0.1:15432`\n")
	}
	if len(e.load().Maps) != 1 {
		t.Fatalf("maps %+v", e.load().Maps)
	}
	// And any spelling removes it.
	if out := e.mustGst("map", "remove", "localhost:15432"); out != "Removed map 127.0.0.1:15432.\n" {
		t.Fatalf("output %q", out)
	}
	e.mustGst("map", "add", "127.0.0.1:15432", "devcn:5432")
	if out := e.mustGst("map", "rm", "15432"); out != "Removed map 127.0.0.1:15432.\n" {
		t.Fatalf("output %q", out)
	}
}

func TestMapAddRejects(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{
		{"map", "add", "15432"},
		{"map", "add", "15432", "devcn"},
		{"map", "add", "0", "devcn:80"},
		{"map", "add", "99999", "devcn:80"},
		{"map", "add", "example.com:80", "devcn:80"},
		{"map", "add", "15432", "Bad_Name:80"},
		{"map", "add", "15432", "devcn:0"},
		{"map", "add", "15432", "devcn:run/app.sock"},
		{"map", "add", "15432", "devcn:80", "extra"},
	} {
		if stderr := e.expectCode(exitUsage, args...); !strings.Contains(stderr, "usage: gst map add") {
			t.Errorf("gst %v: %q", args, stderr)
		}
	}
	if len(e.load().Maps) != 0 || len(e.back.calls) != 0 {
		t.Fatalf("rejected maps changed state: %+v %v", e.load().Maps, e.back.calls)
	}
}

func TestMapConflictsWithProxy(t *testing.T) {
	e := newEnv(t)
	e.expectError(exitFailure, []string{"map", "add", "1080", "devcn:80"},
		"error: the proxy listens on 127.0.0.1:1080\n", "hint: choose another address, or move the proxy with `gst proxy <address>`\n")
	e.mustGst("proxy", "1081")
	e.mustGst("map", "add", "1080", "devcn:80")
	e.expectCode(exitFailure, "map", "add", "1081", "devcn:80")
	// An off proxy listens nowhere.
	e.mustGst("proxy", "off")
	e.mustGst("map", "add", "1081", "devcn:80")
}

func TestMapListenFailure(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	e.back.listenErrors["127.0.0.1:15432"] = "address already in use"
	e.expectError(exitFailure, []string{"map", "add", "15432", "devcn:5432"},
		"error: cannot listen on 127.0.0.1:15432: address already in use\n", "hint: choose another address, or free this one and run `gst up`\n")
	// A listener that works is not affected by one that does not.
	if out := e.mustGst("map", "add", "15433", "devcn:5432"); !strings.Contains(out, "Mapped 127.0.0.1:15433") {
		t.Fatalf("output %q", out)
	}
	// The failed map stays configured, so that freeing the port and gst up fixes it.
	if len(e.load().Maps) != 2 {
		t.Fatalf("maps %+v", e.load().Maps)
	}
}

func TestMapRemove(t *testing.T) {
	e := newEnv(t)
	e.mustGst("map", "add", "15432", "devcn:5432")
	e.mustGst("map", "add", "15433", "devcn:5433")
	e.mustGst("map", "add", "/tmp/a.sock", "devcn:/run/a.sock")
	e.resetCalls()

	e.expectError(exitFailure, []string{"map", "remove", "15432", "9999"}, "error: no map listens on 127.0.0.1:9999\n", "hint: see the maps with `gst map list`\n")
	if len(e.load().Maps) != 3 || len(e.back.calls) != 0 {
		t.Fatalf("a failed remove changed state: %+v %v", e.load().Maps, e.back.calls)
	}
	out := e.mustGst("map", "remove", "15432", "/tmp/a.sock")
	if want := "Removed map 127.0.0.1:15432.\nRemoved map /tmp/a.sock.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if got := e.load().Maps; len(got) != 1 || got[0].Listen != "127.0.0.1:15433" {
		t.Fatalf("maps %+v", got)
	}
	if !slices.Equal(e.back.calls, []string{"sync"}) {
		t.Fatalf("calls %v", e.back.calls)
	}
	e.expectError(exitUsage, []string{"map", "remove"}, "error: missing argument <listen>\n")
	e.expectCode(exitUsage, "map", "remove", "bogus")
}

func TestMapList(t *testing.T) {
	e := newEnv(t)
	if out := e.mustGst("map", "list"); out != "No maps. Add one with: gst map add <listen> <machine>:<target>\n" {
		t.Fatalf("output %q", out)
	}
	if out := e.mustGst("map", "list", "--json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty JSON %q", out)
	}

	e.mustGst("map", "add", "15432", "devcn:5432")
	e.mustGst("map", "add", "15433", "far:5433")
	e.back.listenErrors["127.0.0.1:15433"] = "address already in use"

	want := [][]string{
		{"LISTEN", "MACHINE", "TARGET", "STATUS"},
		{"127.0.0.1:15432", "devcn", "localhost:5432", "listening"},
		{"127.0.0.1:15433", "far", "localhost:5433", "failed:", "address", "already", "in", "use"},
	}
	if got := rows(e.mustGst("map", "ls")); !equalRows(got, want) {
		t.Fatalf("list\n got %q\nwant %q", got, want)
	}

	views := decode[[]mapView](t, e.mustGst("map", "list", "--json"))
	wantViews := []mapView{
		{Map: config.Map{Listen: "127.0.0.1:15432", Machine: "devcn", Target: "localhost:5432"}, Status: statusListening},
		{Map: config.Map{Listen: "127.0.0.1:15433", Machine: "far", Target: "localhost:5433"}, Status: statusFailed, Error: "address already in use"},
	}
	if !slices.Equal(views, wantViews) {
		t.Fatalf("views\n got %+v\nwant %+v", views, wantViews)
	}
	contains(t, e.mustGst("map", "list", "--json"), `"listen": "127.0.0.1:15432"`, `"machine": "devcn"`, `"target": "localhost:5432"`, `"status": "listening"`, `"error": "address already in use"`)

	// Without a daemon, nothing listens.
	e.back.installed = false
	want = [][]string{
		{"LISTEN", "MACHINE", "TARGET", "STATUS"},
		{"127.0.0.1:15432", "devcn", "localhost:5432", "gst", "is", "not", "running;", "start", "it", "with", "gst", "up"},
		{"127.0.0.1:15433", "far", "localhost:5433", "gst", "is", "not", "running;", "start", "it", "with", "gst", "up"},
	}
	if got := rows(e.mustGst("map", "list")); !equalRows(got, want) {
		t.Fatalf("list without a daemon\n got %q\nwant %q", got, want)
	}
	for _, v := range decode[[]mapView](t, e.mustGst("map", "list", "--json")) {
		if v.Status != statusNotRun || v.Error != "" {
			t.Errorf("without a daemon: %+v", v)
		}
	}
}

func equalRows(a, b [][]string) bool {
	return slices.EqualFunc(a, b, slices.Equal[[]string])
}

// A daemon that does not report a map, as before it has reloaded, does not run it.
func TestListenerStatus(t *testing.T) {
	states := []wire.Listener{{Listen: "127.0.0.1:1", Error: "boom"}, {Listen: "127.0.0.1:2"}}
	for _, tc := range []struct {
		address string
		states  []wire.Listener
		status  string
		err     string
	}{
		{"127.0.0.1:1", states, statusFailed, "boom"},
		{"127.0.0.1:2", states, statusListening, ""},
		{"127.0.0.1:3", states, statusNotRun, ""},
		{"127.0.0.1:1", nil, statusNotRun, ""},
	} {
		if status, err := listenerStatus(tc.address, tc.states); status != tc.status || err != tc.err {
			t.Errorf("listenerStatus(%s, %v) = %q, %q; want %q, %q", tc.address, tc.states, status, err, tc.status, tc.err)
		}
	}
}

// --- route ---

func TestRouteAdd(t *testing.T) {
	e := newEnv(t)
	out := e.mustGst("route", "add", "Corp.Example.com.", "DevCN")
	if want := "Route corp.example.com goes through devcn.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	e.mustGst("route", "add", "10.20.1.5/16", "devcn")
	e.mustGst("route", "add", "192.0.2.7", "far.gst")
	e.mustGst("route", "add", "default", "devsg")
	want := config.Routes{
		{Match: "corp.example.com", Via: "devcn"},
		{Match: "10.20.0.0/16", Via: "devcn"},
		{Match: "192.0.2.7", Via: "far"},
		{Match: "default", Via: "devsg"},
	}
	if got := e.load().Routes; !slices.Equal(got, want) {
		t.Fatalf("routes\n got %+v\nwant %+v", got, want)
	}
	if !e.back.installed {
		t.Fatal("adding a route did not bring up the daemon")
	}
}

func TestRouteAddReplacesMatch(t *testing.T) {
	e := newEnv(t)
	e.mustGst("route", "add", "corp.example.com", "devcn")
	e.mustGst("route", "add", "default", "devcn")
	e.mustGst("route", "add", "CORP.example.com", "far")
	want := config.Routes{{Match: "default", Via: "devcn"}, {Match: "corp.example.com", Via: "far"}}
	if got := e.load().Routes; !slices.Equal(got, want) {
		t.Fatalf("routes\n got %+v\nwant %+v", got, want)
	}
}

func TestRouteAddNotesAboutTheMachine(t *testing.T) {
	e := newEnv(t)
	e.mustGst("up")
	e.back.topology = meshTopology() // devcn is an exit, far is not

	code, stdout, stderr := e.gst("route", "add", "corp.example.com", "devcn")
	if code != exitOK || stderr != "" || stdout == "" {
		t.Fatalf("an exit: code %d, stderr %q", code, stderr)
	}
	code, _, stderr = e.gst("route", "add", "10.0.0.0/8", "far")
	if code != exitOK || stderr != "note: far is not an exit yet; run `gst exit on` there\n" {
		t.Fatalf("not an exit: code %d, stderr %q", code, stderr)
	}
	code, _, stderr = e.gst("route", "add", "default", "ghost")
	if code != exitOK || stderr != "note: no machine \"ghost\" in the mesh; see gst nodes\n" {
		t.Fatalf("unknown: code %d, stderr %q", code, stderr)
	}
	// The route is kept in each case.
	if got := e.load().Routes; len(got) != 3 {
		t.Fatalf("routes %+v", got)
	}
	// A machine that is named twice is not guessed.
	_, _, stderr = e.gst("route", "add", "192.0.2.1", "dup")
	contains(t, stderr, "note: ", `"dup" names several machines`)
	// Names are resolved like for gst connect: by id prefix and by link name.
	_, _, stderr = e.gst("route", "add", "192.0.2.2", "bbbb")
	if stderr != "" {
		t.Fatalf("id prefix: stderr %q", stderr)
	}

	// Without an answer from the daemon there is nothing to say.
	e.back.installed = false
	e.back.syncErr = nil
	e.back.nodesErr = nil
	e.mustGst("down")
	_, _, stderr = e.gst("route", "add", "192.0.2.3", "far")
	if strings.Contains(stderr, "is not an exit") {
		t.Fatalf("stderr %q", stderr)
	}
}

func TestRouteAddRejects(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{
		{"route", "add", "default"},
		{"route", "add", "not a host", "devcn"},
		{"route", "add", "10.0.0.0/33", "devcn"},
		{"route", "add", "corp.example.com", "Bad_Name"},
		{"route", "add", "corp.example.com", "devcn", "extra"},
	} {
		if stderr := e.expectCode(exitUsage, args...); !strings.Contains(stderr, "usage: gst route add <match> <machine>") {
			t.Errorf("gst %v: %q", args, stderr)
		}
	}
	if len(e.load().Routes) != 0 || len(e.back.calls) != 0 {
		t.Fatalf("rejected routes changed state: %+v %v", e.load().Routes, e.back.calls)
	}
}

func TestRouteRemove(t *testing.T) {
	e := newEnv(t)
	e.mustGst("route", "add", "corp.example.com", "devcn")
	e.mustGst("route", "add", "10.0.0.0/8", "devcn")
	e.mustGst("route", "add", "default", "devsg")
	e.resetCalls()

	e.expectError(exitFailure, []string{"route", "remove", "default", "example.org"},
		"error: no route for example.org\n", "hint: see the routes with `gst route list`\n")
	if len(e.load().Routes) != 3 || len(e.back.calls) != 0 {
		t.Fatalf("a failed remove changed state: %+v %v", e.load().Routes, e.back.calls)
	}
	out := e.mustGst("route", "remove", "10.1.2.3/8", "Corp.Example.Com")
	if want := "Removed route 10.0.0.0/8.\nRemoved route corp.example.com.\n"; out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if got := e.load().Routes; !slices.Equal(got, config.Routes{{Match: "default", Via: "devsg"}}) {
		t.Fatalf("routes %+v", got)
	}
	if !slices.Equal(e.back.calls, []string{"sync"}) {
		t.Fatalf("calls %v", e.back.calls)
	}
	e.mustGst("route", "rm", "default")
	e.expectError(exitUsage, []string{"route", "remove"}, "error: missing argument <match>\n")
	e.expectCode(exitUsage, "route", "remove", "not a host")
}

func TestRouteList(t *testing.T) {
	e := newEnv(t)
	if out := e.mustGst("route", "list"); !strings.HasPrefix(out, "No routes; the proxy connects to hosts outside the mesh from this machine. Add one with: gst route add <match> <machine>\n") {
		t.Fatalf("output %q", out)
	}
	if out := e.mustGst("route", "list", "--json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty JSON %q", out)
	}
	e.mustGst("route", "add", "corp.example.com", "devcn")
	e.mustGst("route", "add", "default", "devsg")

	want := [][]string{{"MATCH", "VIA"}, {"corp.example.com", "devcn"}, {"default", "devsg"}}
	if got := rows(e.mustGst("route", "ls")); !equalRows(got, want) {
		t.Fatalf("list\n got %q\nwant %q", got, want)
	}
	got := decode[[]config.Route](t, e.mustGst("route", "list", "--json"))
	if !slices.Equal(got, []config.Route{{Match: "corp.example.com", Via: "devcn"}, {Match: "default", Via: "devsg"}}) {
		t.Fatalf("routes %+v", got)
	}
	contains(t, e.mustGst("route", "list", "--json"), `"match": "corp.example.com"`, `"via": "devcn"`)
}
