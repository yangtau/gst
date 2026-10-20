package sshconfig

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	out := `user tunnel
hostname ssh.example.com
port 2200
identityfile ~/.ssh/id_rsa
identityfile /keys/id_ed25519
globalknownhostsfile /etc/ssh/ssh_known_hosts /etc/ssh/ssh_known_hosts2
userknownhostsfile ~/.ssh/known_hosts ~/.ssh/known_hosts2
connecttimeout none
serveraliveinterval 20
serveralivecountmax 4
proxycommand none
`
	got, err := parse([]byte(out), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	want := Host{
		Hostname:            "ssh.example.com",
		Port:                "2200",
		User:                "tunnel",
		IdentityFiles:       []string{"/home/u/.ssh/id_rsa", "/keys/id_ed25519"},
		KnownHostsFiles:     []string{"/home/u/.ssh/known_hosts", "/home/u/.ssh/known_hosts2", "/etc/ssh/ssh_known_hosts", "/etc/ssh/ssh_known_hosts2"},
		ServerAliveInterval: 20 * time.Second,
		ServerAliveCountMax: 4,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if got.Address() != "ssh.example.com:2200" {
		t.Fatal(got.Address())
	}
}

func TestParseRejects(t *testing.T) {
	base := "user u\nhostname h\nport 22\n"
	for name, out := range map[string]string{
		"proxyjump":    base + "proxyjump jump.example.com\n",
		"proxycommand": base + "proxycommand nc %h %p\n",
		"hostkeyalias": base + "hostkeyalias alias\n",
		"bad timeout":  base + "connecttimeout soon\n",
		"no hostname":  "user u\nport 22\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parse([]byte(out), "/home/u"); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestResolveWithSSH(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not installed")
	}
	file := filepath.Join(t.TempDir(), "config")
	config := "Host myhost\n  HostName ssh.example.com\n  User tunnel\n  Port 2200\n  IdentityFile /keys/gst\n  ServerAliveInterval 7\n"
	if err := os.WriteFile(file, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ConfigFileEnv, file)

	h, err := Resolve(context.Background(), "myhost")
	if err != nil {
		t.Fatal(err)
	}
	if h.Address() != "ssh.example.com:2200" || h.User != "tunnel" || h.IdentityFiles[0] != "/keys/gst" || h.ServerAliveInterval != 7*time.Second {
		t.Fatalf("unexpected host %+v", h)
	}

	h, err = Resolve(context.Background(), "ssh://other@192.0.2.1:2022")
	if err != nil {
		t.Fatal(err)
	}
	if h.Address() != "192.0.2.1:2022" || h.User != "other" {
		t.Fatalf("unexpected host %+v", h)
	}

	if _, err := Resolve(context.Background(), "bad host"); err == nil || !strings.Contains(err.Error(), "ssh -G") {
		t.Fatalf("unexpected error %v", err)
	}
}
