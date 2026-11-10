package sshconfig

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
proxycommand none
`
	got, err := parse([]byte(out), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	want := Host{
		Hostname:        "ssh.example.com",
		Port:            "2200",
		User:            "tunnel",
		IdentityFiles:   []string{"/home/u/.ssh/id_rsa", "/keys/id_ed25519"},
		KnownHostsFiles: []string{"/home/u/.ssh/known_hosts", "/home/u/.ssh/known_hosts2", "/etc/ssh/ssh_known_hosts", "/etc/ssh/ssh_known_hosts2"},
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
	config := "Host myhost\n  HostName ssh.example.com\n  User tunnel\n  Port 2200\n  IdentityFile /keys/gst\n"
	if err := os.WriteFile(file, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ConfigFileEnv, file)

	h, err := Resolve(context.Background(), "myhost", nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.Address() != "ssh.example.com:2200" || h.User != "tunnel" || h.IdentityFiles[0] != "/keys/gst" {
		t.Fatalf("unexpected host %+v", h)
	}

	// ssh options of the link beat the configuration.
	h, err = Resolve(context.Background(), "myhost", []string{"-p", "2222", "-l", "other"})
	if err != nil {
		t.Fatal(err)
	}
	if h.Address() != "ssh.example.com:2222" || h.User != "other" {
		t.Fatalf("unexpected host %+v", h)
	}

	h, err = Resolve(context.Background(), "ssh://other@192.0.2.1:2022", nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.Address() != "192.0.2.1:2022" || h.User != "other" {
		t.Fatalf("unexpected host %+v", h)
	}

	if _, err := Resolve(context.Background(), "bad host", nil); err == nil || !strings.Contains(err.Error(), "ssh -G") {
		t.Fatalf("unexpected error %v", err)
	}
}
