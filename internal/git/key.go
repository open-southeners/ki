package git

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// GenerateDeployKey creates an Ed25519 key pair. privatePEM is the OpenSSH
// private key; authorizedKey is the one-line public key with comment appended.
func GenerateDeployKey(comment string) (privatePEM []byte, authorizedKey string, err error) {
	if strings.ContainsAny(comment, "\r\n\x00") {
		return nil, "", fmt.Errorf("deploy key comment must be a single line")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate deploy key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, "", fmt.Errorf("marshal private key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", fmt.Errorf("marshal public key: %w", err)
	}
	authorizedKey = strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(sshPub)), "\n")
	if comment != "" {
		authorizedKey += " " + comment
	}
	return pem.EncodeToMemory(block), authorizedKey, nil
}
