// Package secret provides at-rest encryption for credentials stored in the
// agent's state database.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// KeySize is the size in bytes of the key file (AES-256).
const KeySize = 32

// Box seals and opens small secrets with AES-256-GCM.
type Box struct {
	aead cipher.AEAD
}

// LoadOrCreate reads the key at path, creating it with 32 random bytes and
// mode 0600 when it does not exist. An existing key file of the wrong size or
// with permissions looser than 0600 is rejected.
func LoadOrCreate(path string) (*Box, error) {
	key, err := readKey(path)
	if errors.Is(err, fs.ErrNotExist) {
		key, err = createKey(path)
	}
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: init cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: init gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

func readKey(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("secret: open key file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("secret: stat key file: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("secret: key file %s has mode %04o, want 0600", path, perm)
	}
	if info.Size() != KeySize {
		return nil, fmt.Errorf("secret: key file %s is %d bytes, want %d", path, info.Size(), KeySize)
	}
	key := make([]byte, KeySize)
	if _, err := f.Read(key); err != nil {
		return nil, fmt.Errorf("secret: read key file: %w", err)
	}
	return key, nil
}

func createKey(path string) ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("secret: generate key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("secret: create key file: %w", err)
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("secret: write key file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("secret: write key file: %w", err)
	}
	// O_CREATE is subject to the umask; make the mode exact.
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("secret: chmod key file: %w", err)
	}
	return key, nil
}

// Seal encrypts plaintext, binding it to aad. The random nonce is prefixed to
// the returned ciphertext.
func (b *Box) Seal(plaintext, aad []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		// crypto/rand never fails on supported platforms.
		panic(fmt.Sprintf("secret: read random: %v", err))
	}
	return b.aead.Seal(nonce, nonce, plaintext, aad)
}

// Open decrypts a value produced by Seal. It fails if the ciphertext was
// altered or aad differs from the one used to seal.
func (b *Box) Open(ciphertext, aad []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(ciphertext) < n+b.aead.Overhead() {
		return nil, errors.New("secret: ciphertext too short")
	}
	plain, err := b.aead.Open(nil, ciphertext[:n], ciphertext[n:], aad)
	if err != nil {
		return nil, fmt.Errorf("secret: open: %w", err)
	}
	return plain, nil
}
