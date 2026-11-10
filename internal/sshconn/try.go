package sshconn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// ErrUnauthorized says that the server refused every identity.
var ErrUnauthorized = errors.New("the server does not accept this machine's key")

// HostKeyError is a server whose host key known_hosts does not record.
type HostKeyError struct {
	Key ssh.PublicKey // the key that the server showed
	// Changed says that known_hosts records another key for the server:
	// the server's key was replaced, or something stands in for the server.
	Changed bool
}

func (e *HostKeyError) Error() string {
	if e.Changed {
		return "the server's host key is not the one recorded for it"
	}
	return "the server's host key is not recorded yet"
}

// firstHostKeys is the order to ask an unknown server for its host key
// in: the kind that OpenSSH shows a person first, so that the key to check
// is the one they have seen.
var firstHostKeys = []string{
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256,
}

// Try logs in to the server once and says how that went, for a person
// who sets up a link where there is no ssh to try it with: nil when the
// login works, a *HostKeyError when known_hosts does not record the key
// that the server shows, which Trust records once the person has checked
// it, ErrUnauthorized when the server takes none of the identities, and
// otherwise what failed. Like Check's, and unlike Dial's, its errors may
// name the server.
func Try(ctx context.Context, opts Options) error {
	var signers []ssh.Signer
	for _, id := range loadIdentities(opts.IdentityFiles) {
		if id.signer != nil {
			signers = append(signers, id.signer)
		}
	}
	if len(signers) == 0 {
		return errors.New("this machine has no usable key")
	}
	algorithms := firstHostKeys
	known, _, _ := loadKnownHosts(opts.KnownHostsFiles)
	if known != nil {
		if recorded := knownHostKeyAlgorithms(known, opts.Server); len(recorded) > 0 {
			algorithms = recorded
		}
	}
	var shown *HostKeyError
	clientConfig := &ssh.ClientConfig{
		User: opts.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signers...)},
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			if known == nil {
				shown = &HostKeyError{Key: key}
				return shown
			}
			err := known(hostname, remote, key)
			var keyErr *knownhosts.KeyError
			if errors.As(err, &keyErr) {
				shown = &HostKeyError{Key: key, Changed: len(keyErr.Want) > 0}
				return shown
			}
			return err
		},
		HostKeyAlgorithms: algorithms,
	}
	dialCtx, cancel := context.WithTimeout(ctx, opts.ConnectTimeout)
	defer cancel()
	conn, err := new(net.Dialer).DialContext(dialCtx, "tcp", opts.Server)
	if err != nil {
		if dialCtx.Err() != nil && ctx.Err() == nil {
			return fmt.Errorf("%s did not answer in time", opts.Server)
		}
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Err != nil {
			err = opErr.Err // without the addresses, which the message names once
		}
		return fmt.Errorf("cannot connect to %s: %v", opts.Server, err)
	}
	defer conn.Close()
	defer closeOnDone(dialCtx, conn)()
	sshConn, _, _, err := ssh.NewClientConn(conn, opts.Server, clientConfig)
	switch {
	case err == nil:
		sshConn.Close()
		return nil
	case shown != nil:
		return shown
	case dialCtx.Err() != nil:
		return fmt.Errorf("%s did not finish the SSH handshake in time", opts.Server)
	case strings.Contains(err.Error(), "unable to authenticate"):
		return ErrUnauthorized
	}
	return fmt.Errorf("the SSH handshake with %s failed: %v", opts.Server, strings.TrimPrefix(err.Error(), "ssh: handshake failed: "))
}

// Trust records key as the host key of server in the known_hosts file at
// path, in place of the keys that the file recorded for that server by
// its address. It is for a person who has checked the key.
func Trust(path, server string, key ssh.PublicKey) error {
	host := knownhosts.Normalize(server)
	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		for line := range strings.Lines(string(data)) {
			line = strings.TrimRight(line, "\r\n")
			if hosts, _, _ := strings.Cut(line, " "); line != "" && hosts != host {
				lines = append(lines, line)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lines = append(lines, knownhosts.Line([]string{host}, key))
	return replaceFile(path, []byte(strings.Join(lines, "\n")+"\n"))
}

// replaceFile replaces the file at path with data, readable by its owner
// only, at once.
func replaceFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
