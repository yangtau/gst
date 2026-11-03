// Package config reads and writes the gst configuration file, which holds
// the named tunnels. The gst CLI is its only intended writer.
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// Version is the current configuration format. Version 2 added Stopped:
// before it, whether a tunnel ran was recorded by its own background
// service.
const Version = 2

// File is the configuration file: tunnels keyed by name. Version is 0 in
// files written before versions were recorded.
type File struct {
	Version int                `json:"version,omitempty"`
	Mesh    *Mesh              `json:"mesh,omitempty"`
	Tunnels map[string]*Tunnel `json:"tunnels"`
}

// Mesh is present when this machine takes part in the gst mesh: its daemon
// accepts peer links and keeps running without started tunnels.
type Mesh struct {
	ID   string `json:"id"`   // random and stable
	Name string `json:"name"` // shown to peers
}

// Tunnel is one SSH connection and the forwards it serves. Destination is
// resolved with "ssh -G" each time the tunnel starts, so the user's ssh
// configuration supplies host, port, user, identities, known_hosts files,
// and timeouts. The optional fields override what ssh resolves.
type Tunnel struct {
	Destination              string    `json:"destination"`
	IdentityFile             string    `json:"identity_file,omitempty"`
	KnownHostsFile           string    `json:"known_hosts_file,omitempty"`
	ConnectTimeoutSeconds    int64     `json:"connect_timeout_seconds,omitempty"`
	KeepaliveIntervalSeconds int64     `json:"keepalive_interval_seconds,omitempty"`
	KeepaliveCountMax        int64     `json:"keepalive_count_max,omitempty"`
	CleanupListeners         bool      `json:"cleanup_listeners,omitempty"`
	Forwards                 []Forward `json:"forwards"`
	// Stopped tunnels stay configured, but the daemon does not run them.
	Stopped bool `json:"stopped,omitempty"`
	// Mesh links this machine's gst with the server's over the tunnel.
	Mesh bool `json:"mesh,omitempty"`
	// RemoteGst is the server's gst command for the peer link; "gst" in
	// the login shell's PATH when empty.
	RemoteGst string `json:"remote_gst,omitempty"`
	// Via is ViaMesh for a tunnel through the gst mesh: Destination names
	// a machine of the mesh instead of an SSH server, and the forwards
	// connect through the mesh from that machine. Such a tunnel has only
	// local and dynamic forwards and no SSH settings.
	Via string `json:"via,omitempty"`
}

// ViaMesh is Tunnel.Via for a tunnel through the gst mesh.
const ViaMesh = "mesh"

// DefaultPath is $XDG_CONFIG_HOME/gst/config.json, or ~/.config/gst/config.json
// when XDG_CONFIG_HOME is unset, on every platform.
func DefaultPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("home directory lookup failed")
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "gst", "config.json"), nil
}

// Load reads and validates the configuration file. A missing file is an
// empty configuration of the current version.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &File{Version: Version, Tunnels: map[string]*Tunnel{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	f, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return f, nil
}

// parse decodes and validates a configuration document. Errors name the
// tunnel and forward but not configured values, because the tunnel process
// logs them.
func parse(data []byte) (*File, error) {
	var f File
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, errors.New("invalid JSON or unknown field")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after the JSON object")
	}
	if f.Tunnels == nil {
		f.Tunnels = map[string]*Tunnel{}
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

func (f *File) validate() error {
	if f.Version < 0 || f.Version > Version {
		return fmt.Errorf("unsupported config version %d; upgrade gst", f.Version)
	}
	if m := f.Mesh; m != nil && (!meshIDPattern.MatchString(m.ID) || ValidateNodeName(m.Name) != nil) {
		return errors.New("invalid mesh id or name")
	}
	for name, t := range f.Tunnels {
		if err := ValidateName(name); err != nil {
			return err
		}
		if t == nil {
			return fmt.Errorf("tunnel %s is null", name)
		}
		if err := t.Validate(); err != nil {
			return fmt.Errorf("tunnel %s: %w", name, err)
		}
	}
	return nil
}

// Update applies fn to the configuration file under an exclusive lock and
// writes the result atomically, so concurrent gst commands do not lose
// each other's changes. Nothing is written if fn fails.
func Update(path string, fn func(*File) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open config lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock config: %w", err)
	}

	f, err := Load(path)
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		return err
	}
	if err := f.validate(); err != nil {
		return err
	}
	for _, t := range f.Tunnels {
		if t.Forwards == nil {
			t.Forwards = []Forward{}
		}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

var meshIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidMeshID reports whether id is a mesh node id.
func ValidMeshID(id string) bool { return meshIDPattern.MatchString(id) }

// ValidateNodeName checks a mesh node name, which follows the rules of
// tunnel names.
func ValidateNodeName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid node name %q: use 1-32 lowercase letters, digits, or '-', starting with a letter or digit", name)
	}
	return nil
}

// NewMeshID returns a random mesh node id.
func NewMeshID() string {
	return hex.EncodeToString(randomBytes(16))
}

// ValidateName checks a tunnel name. Names become part of service names and
// log file names, so they are restricted to a portable character set.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid tunnel name %q: use 1-32 lowercase letters, digits, or '-', starting with a letter or digit", name)
	}
	return nil
}

// ValidateDestination checks an ssh destination such as "myhost",
// "user@host", or "ssh://user@host:port". It must not look like an ssh
// option, since it is passed to "ssh -G".
func ValidateDestination(destination string) error {
	if destination == "" || strings.HasPrefix(destination, "-") ||
		strings.ContainsFunc(destination, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return errors.New("invalid destination: use an ssh destination such as host, user@host, or ssh://user@host:port")
	}
	return nil
}

// Validate checks a tunnel. Paths must be absolute; durations and counts
// are either zero (unset) or positive and representable.
func (t *Tunnel) Validate() error {
	if err := ValidateDestination(t.Destination); err != nil {
		return err
	}
	for _, path := range []string{t.IdentityFile, t.KnownHostsFile} {
		if path != "" && (!filepath.IsAbs(path) || strings.ContainsRune(path, 0)) {
			return errors.New("identity and known_hosts paths must be absolute")
		}
	}
	if strings.ContainsFunc(t.RemoteGst, func(r rune) bool { return unicode.IsControl(r) }) {
		return errors.New("invalid remote gst command")
	}
	if err := t.validateVia(); err != nil {
		return err
	}
	if !isUnsetOrSeconds(t.ConnectTimeoutSeconds) || !isUnsetOrSeconds(t.KeepaliveIntervalSeconds) ||
		t.KeepaliveCountMax < 0 || t.KeepaliveCountMax > MaxKeepaliveCount {
		return errors.New("invalid timeout or keepalive settings")
	}
	return validateForwards(t.Forwards)
}

func (t *Tunnel) validateVia() error {
	switch t.Via {
	case "":
		return nil
	case ViaMesh:
	default:
		return fmt.Errorf("unsupported via %q", t.Via)
	}
	if t.Mesh || t.RemoteGst != "" || t.CleanupListeners {
		return errors.New("a tunnel through the mesh cannot have the SSH settings --mesh, --remote-gst, or --cleanup-listeners")
	}
	for i, f := range t.Forwards {
		if f.Direction == Remote {
			return fmt.Errorf("forward %d: a tunnel through the mesh cannot have -R forwards", i+1)
		}
	}
	return nil
}

// MaxKeepaliveCount bounds keepalive_count_max so that the keepalive
// timeout, interval × count, always fits in a time.Duration.
const MaxKeepaliveCount = 1000

// MaxSeconds is the largest number of seconds accepted for a duration
// setting; together with MaxKeepaliveCount it keeps every product in range.
const MaxSeconds = math.MaxInt64 / int64(time.Second) / MaxKeepaliveCount

func isUnsetOrSeconds(n int64) bool {
	return n >= 0 && n <= MaxSeconds
}

// AbsPath expands a leading "~" or "~/" to the home directory and makes
// path absolute. Other users' homes ("~name") are not supported.
func AbsPath(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return "", errors.New("invalid path")
	}
	if strings.HasPrefix(path, "~") {
		if path != "~" && !strings.HasPrefix(path, "~/") {
			return "", fmt.Errorf("unsupported path %q: only ~ and ~/ are expanded", path)
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("home directory lookup failed")
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	return filepath.Abs(path)
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}
