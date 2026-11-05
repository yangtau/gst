package config

import (
	"slices"
	"testing"
)

func TestParseMachine(t *testing.T) {
	for ref, want := range map[string]string{
		"devbox":         "devbox",
		"DevBox":         "devbox",
		"devbox.gst":     "devbox",
		"DEVBOX.GST":     "devbox",
		"0a1b":           "0a1b",
		"my-host":        "my-host",
		"":               "",
		".gst":           "",
		"a.b":            "",
		"a_b":            "",
		"-a":             "",
		"a b":            "",
		"devbox.gst.gst": "",
	} {
		got, err := ParseMachine(ref)
		if (want == "") != (err != nil) || got != want {
			t.Errorf("ParseMachine(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
}

func TestParseMachineTarget(t *testing.T) {
	tests := []struct {
		spec, machine, target string // machine is empty when the spec is invalid
	}{
		{"devbox:5432", "devbox", "localhost:5432"},
		{"DevBox.gst:5432", "devbox", "localhost:5432"},
		{"devbox:db.internal:5432", "devbox", "db.internal:5432"},
		{"devbox:[::1]:5432", "devbox", "[::1]:5432"},
		{"devbox:/run/app.sock", "devbox", "/run/app.sock"},
		{"devbox", "", ""},
		{"devbox:", "", ""},
		{":5432", "", ""},
		{"bad_name:5432", "", ""},
		{"devbox:db", "", ""},
		{"devbox:0", "", ""},
	}
	for _, tt := range tests {
		machine, target, err := ParseMachineTarget(tt.spec)
		if (tt.machine == "") != (err != nil) || machine != tt.machine || target != tt.target {
			t.Errorf("ParseMachineTarget(%q) = %q, %q, %v; want %q, %q", tt.spec, machine, target, err, tt.machine, tt.target)
		}
	}
}

func TestParseRouteMatch(t *testing.T) {
	tests := []struct {
		spec, want string // want is empty when the spec is invalid
	}{
		{"default", "default"},
		{"DEFAULT", "default"},
		{"example.com", "example.com"},
		{"Example.COM.", "example.com"},
		{"corp", "corp"},
		{"10.0.0.0/8", "10.0.0.0/8"},
		{"10.1.2.3/8", "10.0.0.0/8"}, // masked
		{"192.0.2.1/24", "192.0.2.0/24"},
		{"192.0.2.1", "192.0.2.1"},
		{"2001:db8::1/32", "2001:db8::/32"},
		{"2001:DB8::1", "2001:db8::1"},
		{"::1", "::1"},
		{"*.example.com", ""},
		{"*.", ""},
		{"*", ""},
		{".example.com", ""},
		{".", ""},
		{"", ""},
		{"a..b", ""},
		{"-a.com", ""},
		{"example.com/8", ""},
		{"10.0.0.0/33", ""},
		{"fe80::1%eth0", ""},
		{"a b", ""},
	}
	for _, tt := range tests {
		got, err := ParseRouteMatch(tt.spec)
		if (tt.want == "") != (err != nil) || got != tt.want {
			t.Errorf("ParseRouteMatch(%q) = %q, %v; want %q", tt.spec, got, err, tt.want)
		}
	}
}

func TestRoutesLookup(t *testing.T) {
	routes := Routes{
		{Match: "default", Via: "dflt"},
		{Match: "example.com", Via: "domain"},
		{Match: "corp.example.com", Via: "subdomain"},
		{Match: "10.0.0.0/8", Via: "net8"},
		{Match: "10.1.0.0/16", Via: "net16"},
		{Match: "192.0.2.7", Via: "ip"},
		{Match: "2001:db8::/32", Via: "net6"},
	}
	tests := []struct {
		host, via string
	}{
		{"other.org", "dflt"},
		{"example.com", "domain"},
		{"www.example.com", "domain"},
		{"notexample.com", "dflt"}, // a suffix must start at a label
		{"example.com.evil.org", "dflt"},
		{"corp.example.com", "subdomain"},
		{"a.b.corp.example.com", "subdomain"},
		{"EXAMPLE.com.", "domain"}, // case and the trailing dot do not matter
		{"10.2.3.4", "net8"},
		{"10.1.2.3", "net16"},
		{"192.0.2.7", "ip"},
		{"192.0.2.8", "dflt"},
		{"[2001:db8::1]", "net6"},
		{"2001:DB8::1", "net6"},
		{"2001:db9::1", "dflt"},
		{"::ffff:10.1.2.3", "net16"},
	}
	for _, order := range []string{"given", "reversed"} {
		rs := routes
		if order == "reversed" {
			rs = slices.Clone(routes)
			slices.Reverse(rs)
		}
		for _, tt := range tests {
			if via, ok := rs.Lookup(tt.host); !ok || via != tt.via {
				t.Errorf("%s: Lookup(%q) = %q, %v; want %q", order, tt.host, via, ok, tt.via)
			}
		}
	}
}

func TestRoutesLookupWithoutDefault(t *testing.T) {
	tests := []struct {
		name   string
		routes Routes
		host   string
		via    string // empty when nothing matches
	}{
		{"no routes", nil, "example.com", ""},
		{"no match", Routes{{Match: "example.com", Via: "a"}}, "example.org", ""},
		{"IP literal does not match a domain route", Routes{{Match: "example.com", Via: "a"}}, "192.0.2.1", ""},
		{"IP literal does not match a numeric suffix", Routes{{Match: "0.2.1", Via: "a"}}, "192.0.2.1", ""},
		{"domain does not match an IP route", Routes{{Match: "192.0.2.1", Via: "a"}}, "example.com", ""},
		{"domain does not match a CIDR route", Routes{{Match: "10.0.0.0/8", Via: "a"}}, "10.example.com", ""},
		{"IPv6 literal does not match an IPv4 route", Routes{{Match: "10.0.0.0/8", Via: "a"}}, "2001:db8::1", ""},
		{"/0 beats default for IPs", Routes{{Match: "default", Via: "d"}, {Match: "0.0.0.0/0", Via: "z"}}, "192.0.2.1", "z"},
		{"/0 does not apply to domains", Routes{{Match: "default", Via: "d"}, {Match: "0.0.0.0/0", Via: "z"}}, "example.com", "d"},
		{"host route beats prefix", Routes{{Match: "10.0.0.0/8", Via: "p"}, {Match: "10.0.0.1", Via: "h"}}, "10.0.0.1", "h"},
	}
	for _, tt := range tests {
		via, ok := tt.routes.Lookup(tt.host)
		if via != tt.via || ok != (tt.via != "") {
			t.Errorf("%s: Lookup(%q) = %q, %v; want %q", tt.name, tt.host, via, ok, tt.via)
		}
	}
}
