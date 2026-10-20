// Package config loads and validates the tunnel configuration file.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"strings"
	"time"
	"unicode"
)

// Config is the tunnel configuration file. Values returned by Load and Parse
// are validated, and IdentityFile and KnownHostsFile have "~" expanded.
type Config struct {
	Server                   string    `json:"server"` // host:port of the SSH server
	User                     string    `json:"user"`
	IdentityFile             string    `json:"identity_file"`
	KnownHostsFile           string    `json:"known_hosts_file"`
	ConnectTimeoutSeconds    int64     `json:"connect_timeout_seconds"`
	KeepaliveIntervalSeconds int64     `json:"keepalive_interval_seconds"`
	KeepaliveCountMax        int64     `json:"keepalive_count_max"`
	CleanupListeners         bool      `json:"cleanup_listeners"`
	Forwards                 []Forward `json:"forwards"`
}

// Forward relays connections accepted on Listen to Target. A Remote
// forward listens on the server and dials Target from this machine, like
// "ssh -R"; a Local forward does the reverse, like "ssh -L".
type Forward struct {
	Direction string `json:"direction"` // Remote or Local
	Listen    string `json:"listen"`    // IP:port bound on the listening side
	Target    string `json:"target"`    // host:port dialed from the other side
}

// Forward directions.
const (
	Remote = "remote"
	Local  = "local"
)

// ConnectTimeout bounds TCP connects, the SSH handshake, and remote cleanup.
func (c Config) ConnectTimeout() time.Duration {
	return time.Duration(c.ConnectTimeoutSeconds) * time.Second
}

// KeepaliveInterval is the time between keepalive probes.
func (c Config) KeepaliveInterval() time.Duration {
	return time.Duration(c.KeepaliveIntervalSeconds) * time.Second
}

// KeepaliveTimeout is how long a single keepalive probe may stay unanswered.
func (c Config) KeepaliveTimeout() time.Duration {
	return c.KeepaliveInterval() * time.Duration(c.KeepaliveCountMax)
}

// ListenAddr returns the parsed Listen address; it is valid once the
// configuration has passed validation.
func (f Forward) ListenAddr() netip.AddrPort {
	addr, _ := parseListenAddress(f.Listen)
	return addr
}

// Errors deliberately omit configuration values so they are safe to log.
var (
	errRead       = errors.New("configuration read failed")
	errSchema     = errors.New("invalid configuration schema")
	errNoForwards = errors.New("configuration requires forwards")
	errServer     = errors.New("invalid server endpoint")
	errUser       = errors.New("invalid user")
	errTimeouts   = errors.New("invalid timeout or keepalive settings")
)

// Load reads and validates the configuration file at path.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, errRead
	}
	return Parse(data)
}

// Parse decodes and validates a configuration document. Unknown fields are
// rejected; a missing cleanup_listeners means false, and any other missing
// or null field fails validation.
func Parse(data []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, errSchema
	}
	if _, err := dec.Token(); err != io.EOF { // nothing may follow the object
		return Config{}, errSchema
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// validate checks every field and expands "~" in the credential paths.
func (c *Config) validate() error {
	if !isDialAddress(c.Server) {
		return errServer
	}
	if !isValidUser(c.User) {
		return errUser
	}
	if !isValidSeconds(c.ConnectTimeoutSeconds) || !isValidSeconds(c.KeepaliveIntervalSeconds) ||
		c.KeepaliveCountMax <= 0 || c.KeepaliveCountMax > maxSeconds/c.KeepaliveIntervalSeconds {
		return errTimeouts
	}

	var err error
	if c.IdentityFile, err = expandHome(c.IdentityFile); err != nil {
		return err
	}
	if c.KnownHostsFile, err = expandHome(c.KnownHostsFile); err != nil {
		return err
	}
	return validateForwards(c.Forwards)
}

func validateForwards(forwards []Forward) error {
	if len(forwards) == 0 {
		return errNoForwards
	}
	type listener struct {
		direction string
		addr      netip.AddrPort
	}
	seen := make(map[listener]bool, len(forwards))
	for i, fwd := range forwards {
		mapping := i + 1
		if fwd.Direction != Remote && fwd.Direction != Local {
			return fmt.Errorf("invalid direction for mapping %d", mapping)
		}
		listen, ok := parseListenAddress(fwd.Listen)
		if !ok {
			return fmt.Errorf("invalid listen endpoint for mapping %d", mapping)
		}
		// Unmap so that ::ffff:a.b.c.d and a.b.c.d count as the same listener.
		key := listener{fwd.Direction, netip.AddrPortFrom(listen.Addr().Unmap(), listen.Port())}
		if seen[key] {
			return fmt.Errorf("duplicate listener for mapping %d", mapping)
		}
		seen[key] = true
		if !isDialAddress(fwd.Target) {
			return fmt.Errorf("invalid target endpoint for mapping %d", mapping)
		}
	}
	return nil
}

// maxSeconds is the largest whole number of seconds a time.Duration can hold.
const maxSeconds = math.MaxInt64 / int64(time.Second)

func isValidSeconds(n int64) bool {
	return n > 0 && n <= maxSeconds
}

func isValidUser(user string) bool {
	return user != "" && !strings.ContainsFunc(user, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
}
