package config

import (
	"reflect"
	"testing"
)

func TestParseLegacy(t *testing.T) {
	const data = `{
  "version": 2,
  "mesh": {"id": "` + nodeID + `", "name": "laptop"},
  "tunnels": {
    "myhost": {
      "destination": "user@ssh.example.com",
      "identity_file": "/home/me/.ssh/id",
      "keepalive_interval_seconds": 5,
      "remote_gst": "/opt/gst",
      "forwards": [
        {"direction": "local", "listen": "127.0.0.1:5432", "target": "db.internal:5432"},
        {"direction": "local", "listen": "/tmp/app.sock", "target": "/run/app.sock"},
        {"direction": "remote", "listen": "0.0.0.0:2222", "target": "127.0.0.1:22"},
        {"direction": "dynamic", "listen": "127.0.0.1:1080"},
        {"direction": "local", "listen": "127.0.0.1:5432", "target": "other:1"}
      ]
    },
    "viamesh": {
      "destination": "mesh:devbox",
      "forwards": [
        {"direction": "local", "listen": "127.0.0.1:8080", "target": "localhost:80"},
        {"direction": "remote", "listen": "127.0.0.1:9000", "target": "127.0.0.1:9001"}
      ]
    },
    "bare": {"destination": "bare-host"}
  }
}`
	f, notes, err := parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	want := &File{
		Version: Version,
		Node:    &Node{ID: nodeID, Name: "laptop"},
		Links: map[string]*Link{
			"myhost": {Destination: "user@ssh.example.com", RemoteGst: "/opt/gst"},
			"bare":   {Destination: "bare-host"},
		},
		// Tunnels are processed in name order: bare, myhost, viamesh.
		Maps: []Map{
			{Listen: "127.0.0.1:5432", Machine: "myhost", Target: "db.internal:5432"},
			{Listen: "/tmp/app.sock", Machine: "myhost", Target: "/run/app.sock"},
			{Listen: "127.0.0.1:8080", Machine: "devbox", Target: "localhost:80"},
		},
		Routes: Routes{},
	}
	if !reflect.DeepEqual(f, want) {
		t.Errorf("file = %+v\nwant %+v", f, want)
	}
	wantNotes := []string{
		"tunnel myhost: -R 0.0.0.0:2222:127.0.0.1:22",
		"tunnel myhost: -D 127.0.0.1:1080",
		"tunnel myhost: -L 127.0.0.1:5432:other:1", // its listen address is taken
		"tunnel viamesh: -R 127.0.0.1:9000:127.0.0.1:9001",
	}
	if !reflect.DeepEqual(notes, wantNotes) {
		t.Errorf("notes = %q\nwant %q", notes, wantNotes)
	}
}

func TestParseLegacyEdges(t *testing.T) {
	t.Run("no mesh identity", func(t *testing.T) {
		f, notes, err := parse([]byte(`{"version": 2, "tunnels": {}}`))
		if err != nil || f.Node != nil || notes == nil || len(notes) != 0 {
			t.Fatalf("got %+v, %v, %v", f, notes, err)
		}
	})
	t.Run("missing version is legacy", func(t *testing.T) {
		f, notes, err := parse([]byte(`{"tunnels": {"myhost": {"destination": "myhost"}}}`))
		if err != nil || f.Version != Version || len(f.Links) != 1 || notes == nil {
			t.Fatalf("got %+v, %v, %v", f, notes, err)
		}
	})
	t.Run("null tunnel", func(t *testing.T) {
		f, _, err := parse([]byte(`{"version": 2, "tunnels": {"myhost": null}}`))
		if err != nil || len(f.Links) != 0 {
			t.Fatalf("got %+v, %v", f, err)
		}
	})
	t.Run("invalid forward is a note", func(t *testing.T) {
		_, notes, err := parse([]byte(`{"version": 2, "tunnels": {"myhost": {"destination": "myhost", "forwards": [
			{"direction": "local", "listen": "localhost:1", "target": "db:1"}]}}}`))
		if err != nil || !reflect.DeepEqual(notes, []string{"tunnel myhost: -L localhost:1:db:1"}) {
			t.Fatalf("notes = %q, %v", notes, err)
		}
	})
	t.Run("invalid mesh machine is a note", func(t *testing.T) {
		f, notes, err := parse([]byte(`{"version": 2, "tunnels": {"viamesh": {"destination": "mesh:Dev.Box", "forwards": [
			{"direction": "local", "listen": "127.0.0.1:1", "target": "db:1"}]}}}`))
		if err != nil || len(f.Maps) != 0 || len(notes) != 1 {
			t.Fatalf("got %+v, %q, %v", f, notes, err)
		}
	})
	t.Run("invalid tunnel fails", func(t *testing.T) {
		if _, _, err := parse([]byte(`{"version": 2, "tunnels": {"Bad Name": {"destination": "h"}}}`)); err == nil {
			t.Fatal("invalid link name accepted")
		}
		if _, _, err := parse([]byte(`{"version": 2, "mesh": {"id": "x", "name": "n"}}`)); err == nil {
			t.Fatal("invalid mesh id accepted")
		}
	})
}
