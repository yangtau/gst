package sshconn

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
)

// NewIdentity makes sure that path holds a private key: it writes a new
// one there as ReplaceIdentity does, unless the file exists. It is for
// machines without ssh-keygen.
func NewIdentity(path string) error {
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return ReplaceIdentity(path)
}

// ReplaceIdentity writes a new ED25519 key without a passphrase at path,
// for its owner only, in place of any key there.
func ReplaceIdentity(path string) error {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		return err
	}
	return replaceFile(path, pem.EncodeToMemory(block))
}

// AuthorizedKey returns the line that lets the identity at path log in,
// for a server's authorized_keys, ending in comment.
func AuthorizedKey(path, comment string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if comment != "" {
		line += " " + comment
	}
	return line, nil
}
