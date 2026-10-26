package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters used for new hashes. Verification reads the parameters
// from the stored string, so these can change without invalidating old hashes.
const (
	argonMemoryKiB = 64 * 1024
	argonTime      = 3
	argonThreads   = 2
	argonSaltLen   = 16
	argonKeyLen    = 32

	// maxConcurrentHashes bounds simultaneous argon2 computations (each uses
	// 64 MiB).
	maxConcurrentHashes = 2
	// hashWait is the longest an acquire waits for a free slot.
	hashWait = 5 * time.Second
)

// ErrBusy is returned when no hashing slot became free in time.
var ErrBusy = errors.New("auth: too many concurrent password hashes")

// ErrInvalidHash is returned when a stored hash is not a valid argon2id PHC string.
var ErrInvalidHash = errors.New("auth: invalid password hash")

var hashSem = make(chan struct{}, maxConcurrentHashes)

// acquire takes a hashing slot, waiting for ctx or hashWait, whichever is first.
func acquire(ctx context.Context, wait time.Duration) (release func(), err error) {
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case hashSem <- struct{}{}:
		return func() { <-hashSem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrBusy, ctx.Err())
	case <-t.C:
		return nil, ErrBusy
	}
}

var b64 = base64.RawStdEncoding

// HashPassword returns an argon2id PHC string for password. It returns ErrBusy
// if all hashing slots stay occupied until ctx ends or 5 s pass.
func HashPassword(ctx context.Context, password string) (string, error) {
	release, err := acquire(ctx, hashWait)
	if err != nil {
		return "", err
	}
	defer release()
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return encodePHC(argonMemoryKiB, argonTime, argonThreads, salt, key), nil
}

func encodePHC(m, t uint32, p uint8, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, m, t, p, b64.EncodeToString(salt), b64.EncodeToString(key))
}

type phc struct {
	m, t    uint32
	p       uint8
	salt, k []byte
}

func parsePHC(s string) (phc, error) {
	var h phc
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return h, ErrInvalidHash
	}
	var v int
	if n, err := fmt.Sscanf(parts[2], "v=%d", &v); n != 1 || err != nil || v != argon2.Version {
		return h, ErrInvalidHash
	}
	var p uint32
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &h.m, &h.t, &p); n != 3 || err != nil {
		return h, ErrInvalidHash
	}
	if p == 0 || p > 255 || h.t == 0 || h.m == 0 || h.m > 1<<22 || h.t > 100 {
		return h, ErrInvalidHash
	}
	h.p = uint8(p)
	var err error
	if h.salt, err = b64.DecodeString(parts[4]); err != nil || len(h.salt) == 0 {
		return h, ErrInvalidHash
	}
	if h.k, err = b64.DecodeString(parts[5]); err != nil || len(h.k) == 0 {
		return h, ErrInvalidHash
	}
	return h, nil
}

// VerifyPassword reports whether password matches the PHC string encoded. The
// comparison is constant-time. A malformed hash yields ErrInvalidHash; slot
// exhaustion yields ErrBusy.
func VerifyPassword(ctx context.Context, password, encoded string) (bool, error) {
	h, err := parsePHC(encoded)
	if err != nil {
		return false, err
	}
	release, err := acquire(ctx, hashWait)
	if err != nil {
		return false, err
	}
	defer release()
	got := argon2.IDKey([]byte(password), h.salt, h.t, h.m, h.p, uint32(len(h.k)))
	return subtle.ConstantTimeCompare(got, h.k) == 1, nil
}

var (
	dummyOnce sync.Once
	dummyHash string
)

// DummyVerify burns the same CPU time as VerifyPassword against a throwaway
// hash, to equalise timing when the account does not exist. It always reports
// false, and returns ErrBusy if no slot was available.
func DummyVerify(ctx context.Context, password string) (bool, error) {
	dummyOnce.Do(func() {
		salt := make([]byte, argonSaltLen)
		_, _ = rand.Read(salt)
		key := argon2.IDKey([]byte("dummy"), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
		dummyHash = encodePHC(argonMemoryKiB, argonTime, argonThreads, salt, key)
	})
	_, err := VerifyPassword(ctx, password, dummyHash)
	return false, err
}
