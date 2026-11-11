// Package mobile is gst on a phone or tablet, a machine whose system
// gives it no command line, no ssh, and no background service: an iPhone
// or iPad. An app does there what the gst command does elsewhere, and
// the system runs the daemon inside the process of a VPN, which it
// starts and stops. Both are this package, in two processes that share
// one directory.
//
// The app's part (the methods "app.…" of Call) keeps what the command
// line and ssh keep elsewhere: the node's identity, a key of its own to
// log in to servers with, the host keys that a person checked, and the
// links. The VPN's part (Start and Stop) runs the daemon, and Call
// answers the daemon's control methods there, as its socket does
// elsewhere. No other node can link to such a machine: nothing on it
// accepts an SSH login.
package mobile

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/yangtau/ssh-tunnel/internal/config"
	"github.com/yangtau/ssh-tunnel/internal/logfile"
	"github.com/yangtau/ssh-tunnel/internal/sshconfig"
	"github.com/yangtau/ssh-tunnel/internal/sshconn"
	"github.com/yangtau/ssh-tunnel/internal/version"
	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Methods of the app's part, which change what the daemon then reloads
// (wire.MethodReload).
const (
	// MethodInfo ("app.info") takes no params and returns Info.
	MethodInfo = "app.info"
	// MethodName ("app.name") takes NameParams, renames this node, and
	// returns Info.
	MethodName = "app.name"
	// MethodCheck ("app.check") takes LinkParams, logs in to the server
	// once, and returns Check: how that went.
	MethodCheck = "app.check"
	// MethodTrust ("app.trust") takes TrustParams and records the host
	// key of the server, which a person checked; it returns nothing.
	MethodTrust = "app.trust"
	// MethodAdd ("app.add") takes LinkParams, adds the link as gst add
	// does, and returns Info.
	MethodAdd = "app.add"
	// MethodUnlink ("app.unlink") takes LinkParams, removes the link as
	// gst unlink does, and returns Info.
	MethodUnlink = "app.unlink"
	// MethodNewKey ("app.new_key") takes no params, replaces this
	// machine's key with a new one, and returns Info. Servers accept the
	// new key once it is in their authorized_keys; links log in with it
	// from their next connection.
	MethodNewKey = "app.new_key"
	// MethodLog ("app.log") takes no params and returns Log: the end of
	// the daemon's log.
	MethodLog = "app.log"
)

// Info is this node and what its configuration holds.
type Info struct {
	Node     wire.NodeInfo `json:"node"`
	Protocol int           `json:"protocol"`
	// PublicKey is the line that lets this machine log in, for a
	// server's authorized_keys.
	PublicKey string `json:"public_key"`
	Links     []Link `json:"links"`
}

// Link is a link of the configuration.
type Link struct {
	Destination string `json:"destination"`
	Target      string `json:"target,omitempty"` // the id of the node it reached
}

// NameParams name this node.
type NameParams struct {
	Name string `json:"name"`
}

// LinkParams name a link by its destination: user@host, or
// ssh://user@host:port.
type LinkParams struct {
	Destination string `json:"destination"`
}

// States of a Check.
const (
	CheckReady        = "ready"        // the login works
	CheckUnknownHost  = "unknown_host" // no host key is recorded for the server; HostKey is the one it shows
	CheckChangedHost  = "changed_host" // the server shows HostKey, and another is recorded
	CheckUnauthorized = "unauthorized" // the server does not accept this machine's key
	CheckFailed       = "failed"       // Error says what
)

// Check is how a login to a server went.
type Check struct {
	Destination string   `json:"destination"`
	Server      string   `json:"server"` // its host:port
	User        string   `json:"user"`
	State       string   `json:"state"`
	HostKey     *HostKey `json:"host_key,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// HostKey is a server's host key, for a person to compare with the
// server's own.
type HostKey struct {
	Type        string `json:"type"`        // such as ssh-ed25519
	Fingerprint string `json:"fingerprint"` // as ssh-keygen -l prints it: SHA256:…
	Key         string `json:"key"`         // the key itself, for TrustParams
}

// TrustParams name the host key that a person checked.
type TrustParams struct {
	Destination string `json:"destination"`
	Key         string `json:"key"` // a HostKey's Key
}

// Log is the end of the daemon's log.
type Log struct {
	Text string `json:"text"`
}

// App is gst in one of the two processes of a phone.
type App struct {
	dir   string
	local wire.Server // the app's methods

	// connect opens the connection of a link, and device returns the
	// system's interface once it is set up; tests replace them.
	connect func(ctx context.Context, l *config.Link) (io.ReadWriteCloser, error)
	device  func() (Device, error)

	mu      sync.Mutex
	running *running // the daemon, between Start and Stop
}

// Open returns gst with its files in dir, which the two processes share.
// The first time, it gives the node an identity, called after name, the
// name that the phone goes by, and a key to log in to servers with.
func Open(dir, name string) (*App, error) {
	a := &App{dir: dir, device: systemDevice}
	a.connect = a.connectLink
	err := config.Update(a.configPath(), func(f *config.File) error {
		if f.Node == nil {
			f.Node = &config.Node{ID: config.NewNodeID(), Name: config.NodeName(name)}
		}
		// Under the configuration's lock, so that the two processes
		// make one key.
		return sshconn.NewIdentity(a.keyPath())
	})
	if err != nil {
		return nil, err
	}
	none := func(fn func() (any, error)) func(context.Context, struct{}) (any, error) {
		return func(context.Context, struct{}) (any, error) { return fn() }
	}
	wire.Handle(&a.local, MethodInfo, none(func() (any, error) { return a.info() }))
	wire.Handle(&a.local, MethodName, a.rename)
	wire.Handle(&a.local, MethodCheck, a.check)
	wire.Handle(&a.local, MethodTrust, a.trust)
	wire.Handle(&a.local, MethodAdd, a.add)
	wire.Handle(&a.local, MethodUnlink, a.unlink)
	wire.Handle(&a.local, MethodNewKey, none(a.newKey))
	wire.Handle(&a.local, MethodLog, none(func() (any, error) { return a.log() }))
	return a, nil
}

func (a *App) configPath() string     { return filepath.Join(a.dir, "config.json") }
func (a *App) keyPath() string        { return filepath.Join(a.dir, "id_ed25519") }
func (a *App) knownHostsPath() string { return filepath.Join(a.dir, "known_hosts") }
func (a *App) logPath() string        { return filepath.Join(a.dir, "gst.log") }

// Call answers a call: of the app's methods, or, while the daemon runs
// in this process, of the daemon's control methods.
func (a *App) Call(ctx context.Context, req wire.Request) wire.Response {
	if strings.HasPrefix(req.Method, "app.") {
		return a.local.Call(ctx, req)
	}
	a.mu.Lock()
	r := a.running
	a.mu.Unlock()
	if r == nil {
		return wire.Response{Error: wire.Errorf(wire.CodeFailed, "gst is not running on this machine")}
	}
	return r.daemon.Call(ctx, req)
}

func (a *App) info() (Info, error) {
	f, err := config.Load(a.configPath())
	if err != nil {
		return Info{}, err
	}
	if f.Node == nil {
		return Info{}, errors.New("this machine has no identity yet")
	}
	key, err := sshconn.AuthorizedKey(a.keyPath(), "gst@"+f.Node.Name)
	if err != nil {
		return Info{}, errors.New("this machine's key is unreadable")
	}
	info := Info{
		Node:      wire.NodeInfo{ID: f.Node.ID, Name: f.Node.Name, Software: version.Software},
		Protocol:  wire.Protocol,
		PublicKey: key,
		Links:     []Link{},
	}
	for _, l := range f.Links {
		info.Links = append(info.Links, Link{Destination: l.Destination, Target: l.Target})
	}
	return info, nil
}

// update changes the configuration and returns what it holds then.
func (a *App) update(fn func(*config.File) error) (any, error) {
	if err := config.Update(a.configPath(), fn); err != nil {
		return nil, err
	}
	return a.info()
}

func (a *App) rename(_ context.Context, p NameParams) (any, error) {
	if err := config.ValidateName(p.Name); err != nil {
		return nil, wire.Errorf(wire.CodeInvalidParams, "%v", err)
	}
	return a.update(func(f *config.File) error {
		f.Node.Name = p.Name
		return nil
	})
}

// options are the settings of the connection to destination. Where
// there is no ssh configuration, the destination says where and as whom,
// and the rest is this machine's own.
func (a *App) options(destination string) (sshconn.Options, error) {
	if err := config.ValidateDestination(destination); err != nil {
		return sshconn.Options{}, err
	}
	host, err := sshconfig.Literal(destination)
	if err != nil {
		return sshconn.Options{}, err
	}
	return sshconn.Options{
		Server:          host.Address(),
		User:            host.User,
		IdentityFiles:   []string{a.keyPath()},
		KnownHostsFiles: []string{a.knownHostsPath()},
		ConnectTimeout:  sshconn.DefaultConnectTimeout,
	}, nil
}

func (a *App) check(ctx context.Context, p LinkParams) (any, error) {
	opts, err := a.options(p.Destination)
	if err != nil {
		return nil, wire.Errorf(wire.CodeInvalidParams, "%v", err)
	}
	c := Check{Destination: p.Destination, Server: opts.Server, User: opts.User}
	err = sshconn.Try(ctx, opts)
	var shown *sshconn.HostKeyError
	switch {
	case err == nil:
		c.State = CheckReady
	case errors.As(err, &shown):
		c.State = CheckUnknownHost
		if shown.Changed {
			c.State = CheckChangedHost
		}
		c.HostKey = &HostKey{
			Type:        shown.Key.Type(),
			Fingerprint: ssh.FingerprintSHA256(shown.Key),
			Key:         strings.TrimSpace(string(ssh.MarshalAuthorizedKey(shown.Key))),
		}
	case errors.Is(err, sshconn.ErrUnauthorized):
		c.State = CheckUnauthorized
	default:
		c.State, c.Error = CheckFailed, err.Error()
	}
	return c, nil
}

func (a *App) trust(_ context.Context, p TrustParams) (any, error) {
	opts, err := a.options(p.Destination)
	if err != nil {
		return nil, wire.Errorf(wire.CodeInvalidParams, "%v", err)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(p.Key))
	if err != nil {
		return nil, wire.Errorf(wire.CodeInvalidParams, "invalid host key")
	}
	if err := sshconn.Trust(a.knownHostsPath(), opts.Server, key); err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

// add adds a link as gst add does: one that a person added, which
// brings a removed node back, in place of the one to the destination.
func (a *App) add(_ context.Context, p LinkParams) (any, error) {
	opts, err := a.options(p.Destination)
	if err != nil {
		return nil, wire.Errorf(wire.CodeInvalidParams, "%v", err)
	}
	if err := sshconn.Check(opts); err != nil {
		return nil, wire.Errorf(wire.CodeFailed, "the server's host key is not recorded yet")
	}
	l := config.Link{Destination: p.Destination, Admit: true}
	return a.update(func(f *config.File) error {
		if old := f.Link(l.Destination); old != nil {
			*old = l
		} else {
			f.Links = append(f.Links, l)
		}
		return nil
	})
}

func (a *App) unlink(_ context.Context, p LinkParams) (any, error) {
	return a.update(func(f *config.File) error {
		if f.Link(p.Destination) == nil {
			return wire.Errorf(wire.CodeNotFound, "this machine has no link to %s", p.Destination)
		}
		f.Links = slices.DeleteFunc(f.Links, func(l config.Link) bool { return l.Destination == p.Destination })
		return nil
	})
}

// newKey replaces the key under the configuration's lock, as Open makes
// it, so that the other process never reads half a key.
func (a *App) newKey() (any, error) {
	return a.update(func(*config.File) error {
		return sshconn.ReplaceIdentity(a.keyPath())
	})
}

// logTail is how much of the log the app shows.
const logTail = 64 << 10

func (a *App) log() (Log, error) {
	var text []byte
	// The file before the latest rotation comes first.
	for _, path := range []string{a.logPath() + ".1", a.logPath()} {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Log{}, err
		}
		text = append(text, data...)
	}
	if len(text) > logTail {
		text = text[len(text)-logTail:]
		if i := slices.Index(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	return Log{Text: string(text)}, nil
}

// connectLink connects to the daemon on the machine of link l over SSH,
// for the daemon. Its errors omit addresses, users, and paths.
func (a *App) connectLink(ctx context.Context, l *config.Link) (io.ReadWriteCloser, error) {
	opts, err := a.options(l.Destination)
	if err != nil || len(l.SSHArgs) > 0 {
		return nil, errors.New("the destination is not one that this machine can use")
	}
	return sshconn.Link(ctx, opts)
}

// running is the daemon while it runs.
type running struct {
	daemon interface {
		Call(context.Context, wire.Request) wire.Response
	}
	stop context.CancelFunc
	done chan struct{}
	log  *logfile.File
}
