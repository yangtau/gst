// Package config reads and writes the gst configuration file: this
// machine's node identity, its links to other machines, and its exposed
// ports. The gst CLI is its only intended writer, and the daemon, for what
// it learns about the links and the identity.
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
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"unicode"
)

// Version is the current configuration format. Version 5 listed links
// without names. Version 6 adds ssh arguments, the machine a link reaches,
// and membership, and leaves the subnet to the network; Load reads
// version 5 too.
const Version = 6

// File is the configuration file.
type File struct {
	Version int   `json:"version"`
	Node    *Node `json:"node,omitempty"` // set by Update
	// Down keeps the daemon from running until gst up.
	Down bool `json:"down,omitempty"`
	// Links are this machine's links, one per destination.
	Links   []Link   `json:"links"`
	Exposes []Expose `json:"exposes,omitempty"`
}

// Node is this machine's identity in the mesh.
type Node struct {
	ID   string `json:"id"`   // random and stable
	Name string `json:"name"` // shown to others; not unique
	// Generation counts the times that a person added this machine back
	// after a removal from the network.
	Generation int64 `json:"generation,omitempty"`
}

// Link is an SSH connection to another machine's gst. Destination, with
// SSHArgs, is resolved with "ssh -G" each time the link connects, so the
// user's ssh configuration supplies host, port, user, identities,
// known_hosts files, and timeouts. Destination also identifies the link.
type Link struct {
	Destination string `json:"destination"`
	// SSHArgs are ssh options for this link, such as -p 2222 or -i key.
	SSHArgs []string `json:"ssh_args,omitempty"`
	// Target is the id of the machine that the link reached, once it ran.
	Target string `json:"target,omitempty"`
	// Admit says that a person added the link with gst add, and it has
	// not run yet: it brings a removed machine at either end back.
	Admit bool `json:"admit,omitempty"`
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
	if head.Version == 5 {
		return parseV5(data)
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

// parseV5 reads version 5, whose links may name the remote gst command
// and whose subnet the network now keeps: version 6 drops both.
func parseV5(data []byte) (*File, error) {
	var v5 struct {
		File
		Links []struct {
			Link
			RemoteGst string `json:"remote_gst"`
		} `json:"links"`
		Net json.RawMessage `json:"net"`
	}
	if err := decodeStrict(data, &v5); err != nil {
		return nil, err
	}
	f := &v5.File
	f.Version = Version
	f.Links = []Link{}
	for _, l := range v5.Links {
		f.Links = append(f.Links, l.Link)
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
	if n := f.Node; n != nil && (!ValidNodeID(n.ID) || ValidateName(n.Name) != nil || n.Generation < 0) {
		return errors.New("invalid node id, name, or generation")
	}
	links, targets := map[string]bool{}, map[string]bool{}
	for i, l := range f.Links {
		if err := l.Validate(); err != nil {
			return fmt.Errorf("link %d: %w", i+1, err)
		}
		if links[l.Destination] {
			return fmt.Errorf("link %d: another link has the same destination", i+1)
		}
		if targets[l.Target] {
			return fmt.Errorf("link %d: another link reaches the same machine", i+1)
		}
		links[l.Destination] = true
		if l.Target != "" {
			targets[l.Target] = true
		}
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
	if err := WriteFile(path, append(data, '\n')); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
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

// WriteFile replaces the file at path with data, readable by its owner
// only, at once: a reader sees the old file or the new one, never part.
func WriteFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
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
	for _, arg := range l.SSHArgs {
		if arg == "" || strings.ContainsFunc(arg, unicode.IsControl) {
			return errors.New("invalid ssh argument")
		}
	}
	if l.Target != "" && !ValidNodeID(l.Target) {
		return errors.New("invalid target")
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
