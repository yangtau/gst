package config

import (
	"strings"
	"testing"
)

func TestParseExpose(t *testing.T) {
	for _, tc := range []struct {
		target            string
		port              int
		udp               bool
		protocol, address string
		wantPort          int
	}{
		{"5999", 0, false, "tcp", "127.0.0.1:5999", 5999},
		{"5353", 5999, true, "udp", "127.0.0.1:5353", 5999},
		{"udp://service.example.com:5353", 0, false, "udp", "service.example.com:5353", 5353},
		{"tcp://[::1]:8080", 5999, false, "tcp", "[::1]:8080", 5999},
		{"tcp://[fe80::1%25en0]:8080", 5999, false, "tcp", "[fe80::1%en0]:8080", 5999},
		{"unix:///tmp/service%23.sock", 5999, false, "unix", "/tmp/service#.sock", 5999},
	} {
		e, err := ParseExpose(tc.target, tc.port, tc.udp)
		if err != nil {
			t.Fatal(err)
		}
		network, address := e.Destination()
		if network != tc.protocol || address != tc.address || e.Port != tc.wantPort {
			t.Fatalf("%s: %+v, %s %s", tc.target, e, network, address)
		}
		if err := e.Validate(); err != nil {
			t.Fatalf("canonical rule rejected: %+v: %v", e, err)
		}
	}
	for _, target := range []string{"0", "65536", "-1", "+1", "service.example.com:80", "tcp://:80", "tcp://0.0.0.0:80", "udp://[::]:80", "tcp://x:80/path", "tcp://user@x:80", "tcp://x:80?x", "unix://relative/path", "unixgram:///tmp/socket", "unix:///tmp/%00socket"} {
		if _, err := ParseExpose(target, 5999, false); err == nil {
			t.Errorf("accepted %q", target)
		}
	}
	if _, err := ParseExpose("unix:///tmp/socket", 0, false); err == nil {
		t.Fatal("Unix without port accepted")
	}
	for _, target := range []string{"tcp://localhost:80", "unix:///tmp/socket"} {
		if _, err := ParseExpose(target, 5999, true); err == nil {
			t.Fatal("UDP mismatch accepted")
		}
	}
}

func TestExposeConfig(t *testing.T) {
	for _, body := range []string{
		`{"protocol":"invalid","port":5999,"target":"tcp://secret.example.com:80"}`,
		`{"protocol":"udp","port":5999,"target":"unix:///tmp/secret.sock"}`,
	} {
		_, err := parse([]byte(`{"version":4,"exposes":[` + body + `]}`))
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe validation error: %v", err)
		}
	}
	rule := `{"protocol":"tcp","port":5999,"target":"tcp://127.0.0.1:80"}`
	if _, err := parse([]byte(`{"version":4,"exposes":[` + rule + `,` + rule + `]}`)); err == nil {
		t.Fatal("duplicate accepted")
	}
}
