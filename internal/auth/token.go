package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// TokenPrefix starts every API token.
const TokenPrefix = "ki_"

func randomB64() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewToken returns a new API token ("ki_" + base64url of 32 random bytes) and
// its SHA-256 hash, the only form that should be stored.
func NewToken() (plain string, hash []byte, err error) {
	s, err := randomB64()
	if err != nil {
		return "", nil, err
	}
	plain = TokenPrefix + s
	return plain, HashToken(plain), nil
}

// HashToken returns the SHA-256 of an API token.
func HashToken(plain string) []byte {
	h := sha256.Sum256([]byte(plain))
	return h[:]
}

// NewSessionID returns a new session identifier (base64url of 32 random bytes)
// and its SHA-256 hash, the only form that should be stored.
func NewSessionID() (id string, hash []byte, err error) {
	id, err = randomB64()
	if err != nil {
		return "", nil, err
	}
	return id, HashSessionID(id), nil
}

// HashSessionID returns the SHA-256 of a session identifier.
func HashSessionID(id string) []byte {
	h := sha256.Sum256([]byte(id))
	return h[:]
}
