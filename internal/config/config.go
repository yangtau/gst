// Package config reads and writes the gst configuration file: this
// machine's node identity, its links to other machines, and how it uses
// the mesh (proxy, maps, and routes). The gst CLI is its only intended
// writer.
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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode"
)

// Version is the current configuration format. Version 3 replaced the
// tunnels and their forwards with links, maps, and routes.
const Version = 3

// File is the configuration file.
type File struct {
	Version int   `json:"version"`
	Node    *Node `json:"node,omitempty"` // set by Update
	// Down keeps the daemon from running until gst up.
	Down   bool             `json:"down,omitempty"`
	Proxy  string           `json:"proxy,omitempty"` // listen address; off when empty
	Links  map[string]*Link `json:"links"`
	Maps   []Map            `json:"maps"`
	Routes Routes           `json:"routes"`
	// Net turns on the virtual network; it is off when nil.
	Net *Net `json:"net,omitempty"`
}

// Net is this machine's part in the virtual network.
type Net struct {
	// Subnet is the subnet that the user chose for the whole network, and
	// Since when, in Unix nanoseconds. The network's machines agree on the
	// latest choice. Empty leaves it to them.
	Subnet string `json:"subnet,omitempty"`
	Since  int64  `json:"since,omitempty"`
}

// Node is this machine's identity in the mesh.
type Node struct {
	ID   string `json:"id"`   // random and stable
	Name string `json:"name"` // shown to others; not unique
	// Exit lets other machines connect through this one to addresses
	// that are not on this machine.
	Exit bool `json:"exit,omitempty"`
}

// Link is an SSH connection to another machine's gst. Destination is
// resolved with "ssh -G" each time the link connects, so the user's ssh
// configuration supplies host, port, user, identities, known_hosts files,
// and timeouts.
type Link struct {
	Destination string `json:"destination"`
	// RemoteGst is the server's gst command; "gst" in the login shell's
	// PATH when empty.
	RemoteGst string `json:"remote_gst,omitempty"`
}

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

func empty() *File {
	return &File{Version: Version, Links: map[string]*Link{}, Maps: []Map{}, Routes: Routes{}}
}

// Load reads and validates the configuration file. A missing file is an
// empty configuration. A file of an earlier version is converted, as
// Upgrade would, without writing it.
func Load(path string) (*File, error) {
	f, _, err := load(path)
	return f, err
}

// load is Load that also returns, for a converted file, the notes of
// Upgrade, and nil notes otherwise.
func load(path string) (*File, []string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return empty(), nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read config %s: %w", path, err)
	}
	f, notes, err := parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("config %s: %w", path, err)
	}
	return f, notes, nil
}

// parse decodes and validates a configuration document. Errors name the
// link, map, or route but not configured values, because the daemon logs
// them.
func parse(data []byte) (*File, []string, error) {
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, nil, errors.New("invalid JSON")
	}
	if head.Version < Version {
		return parseLegacy(data)
	}
	f := empty()
	if err := decodeStrict(data, f); err != nil {
		return nil, nil, err
	}
	if err := f.normalize(); err != nil {
		return nil, nil, err
	}
	return f, nil, nil
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON or unknown field")
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}

// normalize fills in empty collections and validates f.
func (f *File) normalize() error {
	if f.Links == nil {
		f.Links = map[string]*Link{}
	}
	if f.Maps == nil {
		f.Maps = []Map{}
	}
	if f.Routes == nil {
		f.Routes = Routes{}
	}
	return f.validate()
}

func (f *File) validate() error {
	if f.Version != Version {
		return fmt.Errorf("unsupported config version %d; upgrade gst", f.Version)
	}
	if n := f.Node; n != nil && (!ValidNodeID(n.ID) || ValidateName(n.Name) != nil) {
		return errors.New("invalid node id or name")
	}
	if _, ok := parseListenAddress(f.Proxy); !ok && f.Proxy != "" {
		return errors.New("invalid proxy address")
	}
	for name, l := range f.Links {
		if err := ValidateName(name); err != nil {
			return err
		}
		if l == nil {
			return fmt.Errorf("link %s is null", name)
		}
		if err := l.Validate(); err != nil {
			return fmt.Errorf("link %s: %w", name, err)
		}
	}
	for i, m := range f.Maps {
		if err := m.validate(); err != nil {
			return fmt.Errorf("map %d: %w", i+1, err)
		}
		if m.Listen == f.Proxy {
			return fmt.Errorf("map %d listens on the proxy address", i+1)
		}
		for _, earlier := range f.Maps[:i] {
			if earlier.Listen == m.Listen {
				return fmt.Errorf("map %d: duplicate listen address", i+1)
			}
		}
	}
	if f.Net != nil && f.Net.Subnet != "" {
		if _, err := ParseSubnet(f.Net.Subnet); err != nil {
			return errors.New("invalid subnet of the virtual network")
		}
	}
	for i, r := range f.Routes {
		if err := r.validate(); err != nil {
			return fmt.Errorf("route %d: %w", i+1, err)
		}
		for _, earlier := range f.Routes[:i] {
			if earlier.Match == r.Match {
				return fmt.Errorf("route %d: duplicate match", i+1)
			}
		}
	}
	return nil
}

// Update applies fn to the configuration file under an exclusive lock and
// writes the result atomically, so concurrent gst commands do not lose
// each other's changes. Nothing is written if fn fails. The file always
// gets a node identity.
func Update(path string, fn func(*File) error) error {
	_, err := update(path, fn)
	return err
}

// Upgrade writes a configuration file of an earlier version in the
// current format. It returns whether it did and notes on the forwards
// that the current format has no place for. Other files stay as they are.
func Upgrade(path string) (upgraded bool, notes []string, err error) {
	if _, notes, err := load(path); err != nil || notes == nil {
		return false, nil, err
	}
	notes, err = update(path, func(*File) error { return nil })
	// Another gst may have upgraded the file meanwhile.
	return err == nil && notes != nil, notes, err
}

func update(path string, fn func(*File) error) ([]string, error) {
	unlock, err := lock(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	f, notes, err := load(path)
	if err != nil {
		return nil, err
	}
	if err := fn(f); err != nil {
		return nil, err
	}
	if f.Node == nil {
		f.Node = &Node{ID: NewNodeID(), Name: DefaultNodeName()}
	}
	if err := f.normalize(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return notes, writeFileAtomic(path, append(data, '\n'))
}

// Apply runs fn on the configuration file under a lock of its own, so
// that gst commands that apply the configuration in the background, each
// after its own change, do so one at a time with the latest configuration;
// otherwise one could act on a configuration that another has changed
// since. fn may read the configuration but not update it.
func Apply(path string, fn func(*File) error) error {
	unlock, err := lock(path + ".apply.lock")
	if err != nil {
		return err
	}
	defer unlock()
	f, err := Load(path)
	if err != nil {
		return err
	}
	return fn(f)
}

// lock takes an exclusive lock on the file at path, creating it and its
// directory.
func lock(path string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open config lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock config: %w", err)
	}
	return func() { f.Close() }, nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

var nodeIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidNodeID reports whether id is a node id.
func ValidNodeID(id string) bool { return nodeIDPattern.MatchString(id) }

// NewNodeID returns a random node id.
func NewNodeID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ValidateName checks a link or node name. Names become parts of file
// names and of host names under .gst, so they are restricted to a
// portable character set.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid name %q: use 1-32 lowercase letters, digits, or '-', starting with a letter or digit", name)
	}
	return nil
}

// DefaultNodeName derives a valid node name from the host name.
func DefaultNodeName() string {
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(strings.ToLower(host), ".")
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 32 {
		name = strings.Trim(name[:32], "-")
	}
	if ValidateName(name) != nil {
		return "node"
	}
	return name
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

// Validate checks a link.
func (l *Link) Validate() error {
	if err := ValidateDestination(l.Destination); err != nil {
		return err
	}
	if strings.ContainsFunc(l.RemoteGst, unicode.IsControl) {
		return errors.New("invalid remote gst command")
	}
	return nil
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
