package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLogFileRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "myhost.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxLogSize-10), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := openLog(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, line := range []string{"fits\n", "rotates\n", "stays\n"} {
		if _, err := l.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	old, _ := os.ReadFile(path + ".1")
	current, _ := os.ReadFile(path)
	if len(old) != maxLogSize-5 || !bytes.HasSuffix(old, []byte("fits\n")) || string(current) != "rotates\nstays\n" {
		t.Fatalf("old log has %d bytes ending %q; current log %q", len(old), old[max(0, len(old)-8):], current)
	}

	// Rotating again replaces the older file.
	if _, err := l.Write(bytes.Repeat([]byte("y"), maxLogSize)); err != nil {
		t.Fatal(err)
	}
	if old, _ := os.ReadFile(path + ".1"); string(old) != "rotates\nstays\n" {
		t.Fatalf("second rotation left %q", old)
	}
}
