// Package config reads and writes the gst configuration file: this
// machine's node identity, its links to other machines, and the subnet
// that the user chose. The gst CLI is its only intended writer.
package config

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"unicode"
)

// Version is the current configuration format. Version 4 left out the
// proxy, maps, and routes of version 3, as the virtual network replaced them.
// Version 5 lists links without names; Load reads version 4 too.
const Version = 5

// File is the configuration file.
type File struct {
	Version int   `json:"version"`
	Node    *Node `json:"node,omitempty"` // set by Update
	// Down keeps the daemon from running until gst up.
	Down bool `json:"down,omitempty"`
	// Links are this machine's links, one per destination.
	Links []Link `json:"links"`
	// Net is what the user chose for the virtual network; nil leaves it
	// to the machines.
	Net     *Net     `json:"net,omitempty"`
	Exposes []Expose `json:"exposes,omitempty"`
}

// Net is what the user chose for the virtual network.
type Net struct {
	// Subnet is the subnet of the whole network, and Since when the user
	// chose it, in Unix nanoseconds. The network's machines agree on the
	// latest choice.
	Subnet string `json:"subnet"`
	Since  int64  `json:"since"`
}

// Node is this machine's identity in the mesh.
type Node struct {
	ID   string `json:"id"`   // random and stable
	Name string `json:"name"` // shown to others; not unique
}

// Link is an SSH connection to another machine's gst. Destination is
// resolved with "ssh -G" each time the link connects, so the user's ssh
// configuration supplies host, port, user, identities, known_hosts files,
// and timeouts. It also identifies the link.
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
	return &File{Version: Version, Links: []Link{}}
}

// Load reads and validates the configuration file. A missing file is an
// empty configuration.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return empty(), nil
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
// link but not configured values, because the daemon logs them.
func parse(data []byte) (*File, error) {
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, errors.New("invalid JSON")
	}
	if head.Version == 4 {
		return parseV4(data)
	}
	if head.Version < Version {
		return nil, fmt.Errorf("config version %d is too old: remove the file and add the links again", head.Version)
	}
	f := empty()
	if err := decodeStrict(data, f); err != nil {
		return nil, err
	}
	if err := f.normalize(); err != nil {
		return nil, err
	}
	return f, nil
}

// parseV4 reads version 4, whose links have names, which version 5
// drops.
func parseV4(data []byte) (*File, error) {
	var v4 struct {
		File
		Links map[string]*Link `json:"links"`
	}
	if err := decodeStrict(data, &v4); err != nil {
		return nil, err
	}
	f := &v4.File
	f.Version = Version
	f.Links = []Link{}
	for _, name := range slices.Sorted(maps.Keys(v4.Links)) {
		if l := v4.Links[name]; l != nil && f.Link(l.Destination) == nil {
			f.Links = append(f.Links, *l)
		}
	}
	if err := f.normalize(); err != nil {
		return nil, err
	}
	return f, nil
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
		f.Links = []Link{}
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
	links := map[string]bool{}
	for i, l := range f.Links {
		if err := l.Validate(); err != nil {
			return fmt.Errorf("link %d: %w", i+1, err)
		}
		if links[l.Destination] {
			return fmt.Errorf("link %d: another link has the same destination", i+1)
		}
		links[l.Destination] = true
	}
	seen := map[string]bool{}
	for _, e := range f.Exposes {
		if err := e.Validate(); err != nil {
			return err
		}
		if seen[e.Key()] {
			return errors.New("duplicate expose port and protocol")
		}
		seen[e.Key()] = true
	}
	if f.Net != nil {
		if _, err := ParseSubnet(f.Net.Subnet); err != nil {
			return errors.New("invalid subnet")
		}
	}
	return nil
}

// Update applies fn to the configuration file under an exclusive lock and
// writes the result atomically, so concurrent gst commands do not lose
// each other's changes. Nothing is written if fn fails. The file always
// gets a node identity.
func Update(path string, fn func(*File) error) error {
	unlock, err := lock(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	f, err := Load(path)
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		return err
	}
	if f.Node == nil {
		f.Node = &Node{ID: NewNodeID(), Name: DefaultNodeName()}
	}
	if err := f.normalize(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
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

// ValidateName checks a machine's name. Names become parts of file
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

// Link returns the link to destination, or nil.
func (f *File) Link(destination string) *Link {
	i := slices.IndexFunc(f.Links, func(l Link) bool { return l.Destination == destination })
	if i < 0 {
		return nil
	}
	return &f.Links[i]
}

// LinkID identifies the link to destination to the machines of the
// network, without telling them the destination.
func LinkID(destination string) string {
	sum := sha256.Sum256([]byte(destination))
	return hex.EncodeToString(sum[:6])
}

var linkIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// ValidLinkID reports whether id is a link id.
func ValidLinkID(id string) bool { return linkIDPattern.MatchString(id) }

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
