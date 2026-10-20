package tunnel

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// newClientConfig authenticates with every usable identity and verifies
// the server strictly against the known_hosts files.
func newClientConfig(opts Options) (*ssh.ClientConfig, error) {
	var signers []ssh.Signer
	for _, id := range loadIdentities(opts.IdentityFiles) {
		if id.signer != nil {
			signers = append(signers, id.signer)
		}
	}
	if len(signers) == 0 {
		return nil, errors.New("no usable identity")
	}
	hostKeyCallback, _, err := loadKnownHosts(opts.KnownHostsFiles)
	if err != nil {
		return nil, errors.New("known hosts load failed")
	}
	return &ssh.ClientConfig{
		User:              opts.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signers...)},
		HostKeyCallback:   hostKeyCallback,
		HostKeyAlgorithms: knownHostKeyAlgorithms(hostKeyCallback, opts.Server),
	}, nil
}

// identity is a candidate private key file. Exactly one of signer and
// problem is set.
type identity struct {
	path    string
	signer  ssh.Signer
	problem string
}

func loadIdentities(paths []string) []identity {
	ids := make([]identity, 0, len(paths))
	for _, path := range paths {
		id := identity{path: path}
		key, err := os.ReadFile(path)
		var missing *ssh.PassphraseMissingError
		switch {
		case errors.Is(err, fs.ErrNotExist):
			id.problem = "missing"
		case err != nil:
			id.problem = "unreadable"
		default:
			id.signer, err = ssh.ParsePrivateKey(key)
			switch {
			case errors.As(err, &missing):
				id.problem = "protected by a passphrase"
			case err != nil:
				id.problem = "not a supported private key"
			}
		}
		ids = append(ids, id)
	}
	return ids
}

// loadKnownHosts reads the existing files among paths and returns them.
func loadKnownHosts(paths []string) (ssh.HostKeyCallback, []string, error) {
	var existing []string
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			existing = append(existing, path)
		}
	}
	if len(existing) == 0 {
		return nil, nil, errors.New("no known_hosts file exists")
	}
	callback, err := knownhosts.New(existing...)
	if err != nil {
		return nil, existing, err
	}
	return callback, existing, nil
}

// knownHostKeyAlgorithms returns the algorithms of the host keys recorded for
// address, so that the server presents one of them. Otherwise x/crypto/ssh
// negotiates by its own preference, which starts with ECDSA, and a server
// recorded only by its ED25519 key fails verification as a key mismatch.
func knownHostKeyAlgorithms(callback ssh.HostKeyCallback, address string) []string {
	var keyErr *knownhosts.KeyError
	if !errors.As(callback(address, &net.TCPAddr{}, unknownKey{}), &keyErr) {
		return nil
	}
	var algorithms []string
	for _, known := range keyErr.Want {
		if keyType := known.Key.Type(); keyType == ssh.KeyAlgoRSA {
			algorithms = append(algorithms, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
		} else {
			algorithms = append(algorithms, keyType)
		}
	}
	return algorithms
}

// unknownKey matches no known_hosts entry, so checking it makes the callback
// report every key recorded for the host.
type unknownKey struct{}

func (unknownKey) Type() string                        { return "" }
func (unknownKey) Marshal() []byte                     { return nil }
func (unknownKey) Verify([]byte, *ssh.Signature) error { return errors.New("unknown key") }

// dial opens an SSH client connection. timeout bounds both the TCP connect
// and the SSH handshake; cancelling ctx aborts the handshake or, later,
// closes the established connection.
func dial(ctx context.Context, address string, clientConfig *ssh.ClientConfig, timeout time.Duration) (*ssh.Client, error) {
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := new(net.Dialer).DialContext(dialCtx, "tcp", address)
	if err != nil {
		return nil, errors.New("TCP connection failed")
	}
	stopClosing := closeOnDone(ctx, conn)

	deadline, _ := dialCtx.Deadline()
	conn.SetDeadline(deadline)
	sshConn, channels, requests, err := ssh.NewClientConn(conn, address, clientConfig)
	switch {
	case err != nil:
		err = errors.New("SSH handshake failed")
	case dialCtx.Err() != nil:
		err = errors.New("SSH handshake timed out")
	}
	if err != nil {
		stopClosing()
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})

	client := ssh.NewClient(sshConn, channels, requests)
	go func() {
		client.Wait()
		stopClosing()
	}()
	return client, nil
}
