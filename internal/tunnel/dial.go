package tunnel

import (
	"context"
	"errors"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"reverse-ssh-tunnel/internal/config"
)

// newClientConfig authenticates with the configured private key and
// verifies the server strictly against the known_hosts file.
func newClientConfig(cfg config.Config) (*ssh.ClientConfig, error) {
	key, err := os.ReadFile(cfg.IdentityFile)
	if err != nil {
		return nil, errors.New("identity read failed")
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, errors.New("identity parse failed")
	}
	hostKeyCallback, err := knownhosts.New(cfg.KnownHostsFile)
	if err != nil {
		return nil, errors.New("known hosts load failed")
	}
	return &ssh.ClientConfig{
		User:              cfg.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   hostKeyCallback,
		HostKeyAlgorithms: knownHostKeyAlgorithms(hostKeyCallback, cfg.Server),
	}, nil
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
