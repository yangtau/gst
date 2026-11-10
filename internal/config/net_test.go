package config

import "testing"

func TestParseSubnet(t *testing.T) {
	for _, tt := range []struct {
		spec, want string
	}{
		{"10.99.0.0/16", "10.99.0.0/16"},
		{"10.99.5.7/16", "10.99.0.0/16"}, // the host bits go
		{"172.16.0.0/12", "172.16.0.0/12"},
		{"192.168.40.0/24", "192.168.40.0/24"},
		{"100.64.0.0/10", "100.64.0.0/10"},
		{"10.0.0.0/8", "10.0.0.0/8"},
		{"10.1.2.0/28", "10.1.2.0/28"},
	} {
		got, err := ParseSubnet(tt.spec)
		if err != nil || got.String() != tt.want {
			t.Errorf("ParseSubnet(%q) = %v, %v, want %s", tt.spec, got, err, tt.want)
		}
	}
	for _, spec := range []string{"", "nonsense", "10.0.0.1", "8.8.8.0/24", "10.0.0.0/7", "10.1.2.3/30", "172.15.0.0/16", "fd00::/64"} {
		if got, err := ParseSubnet(spec); err == nil {
			t.Errorf("ParseSubnet(%q) = %v", spec, got)
		}
	}
}
