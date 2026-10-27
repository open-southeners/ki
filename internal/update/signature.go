// Package update implements the agent's self-update support: release
// signature verification against a compiled-in ed25519 key, a poller that
// learns about new GitHub releases, and the cached state shown in the panel
// and the CLI.
//
// A release carries checksums.txt.sig next to checksums.txt. The signature
// covers the release tag as well as the checksums so an old, validly signed
// release cannot be replayed as a newer one.
package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	sigHeader     = "ki-agent-release-signature-v1"
	sigKeyPrefix  = "key-id: "
	sigSigPrefix  = "sig: "
	messageHeader = "ki-agent release v1\n"
)

// SignedMessage returns the exact bytes that are signed for a release: a fixed
// header, the tag and the unmodified checksums.txt.
func SignedMessage(tag string, checksums []byte) []byte {
	msg := make([]byte, 0, len(messageHeader)+len("tag: ")+len(tag)+1+len(checksums))
	msg = append(msg, messageHeader...)
	msg = append(msg, "tag: "...)
	msg = append(msg, tag...)
	msg = append(msg, '\n')
	msg = append(msg, checksums...)
	return msg
}

// FormatSignature renders the three-line checksums.txt.sig file.
func FormatSignature(keyID string, sig []byte) []byte {
	return []byte(sigHeader + "\n" + sigKeyPrefix + keyID + "\n" + sigSigPrefix +
		base64.StdEncoding.EncodeToString(sig) + "\n")
}

// ParseSignature parses a checksums.txt.sig file. It accepts exactly three
// LF-terminated lines and rejects anything else.
func ParseSignature(b []byte) (keyID string, sig []byte, err error) {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return "", nil, errors.New("signature file must end with a newline")
	}
	lines := strings.Split(string(b[:len(b)-1]), "\n")
	if len(lines) != 3 {
		return "", nil, fmt.Errorf("signature file has %d lines, want 3", len(lines))
	}
	if lines[0] != sigHeader {
		return "", nil, errors.New("unrecognised signature file header")
	}
	keyID, ok := strings.CutPrefix(lines[1], sigKeyPrefix)
	if !ok || !validKeyID(keyID) {
		return "", nil, errors.New("malformed key id line")
	}
	enc, ok := strings.CutPrefix(lines[2], sigSigPrefix)
	if !ok {
		return "", nil, errors.New("malformed signature line")
	}
	sig, err = base64.StdEncoding.Strict().DecodeString(enc)
	if err != nil {
		return "", nil, fmt.Errorf("decode signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return "", nil, fmt.Errorf("signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	return keyID, sig, nil
}

// validKeyID reports whether s is 16 lowercase hex characters.
func validKeyID(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// KeyID returns the identifier of a public key: the first 8 bytes of its
// SHA-256, lowercase hex.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// Verify checks sigFile over (tag, checksums) against the trusted keys. It
// fails closed: an empty key set, an unknown key id or a bad signature are all
// errors.
func Verify(keys map[string]ed25519.PublicKey, tag string, checksums, sigFile []byte) error {
	if len(keys) == 0 {
		return errors.New("no trusted release keys are compiled into this build")
	}
	keyID, sig, err := ParseSignature(sigFile)
	if err != nil {
		return fmt.Errorf("parse signature: %w", err)
	}
	pub, ok := keys[keyID]
	if !ok || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("release signed with unknown key %s", keyID)
	}
	if !ed25519.Verify(pub, SignedMessage(tag, checksums), sig) {
		return errors.New("release signature does not match")
	}
	return nil
}
