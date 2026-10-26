package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 default; required by authenticator apps.
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	totpStep   = 30
	totpDigits = 6
	totpSkew   = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret returns a fresh TOTP secret: 20 random bytes, base32 without padding.
func NewSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return b32.EncodeToString(b), nil
}

// URI returns the otpauth:// provisioning URI for secret.
func URI(issuer, account, secret string) string {
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", strconv.Itoa(totpDigits))
	q.Set("period", strconv.Itoa(totpStep))
	u := url.URL{Scheme: "otpauth", Host: "totp", Path: "/" + issuer + ":" + account, RawQuery: q.Encode()}
	return u.String()
}

// hotp computes the RFC 4226 HOTP value for key and counter with the given
// number of digits (dynamic truncation, HMAC-SHA-1).
func hotp(key []byte, counter uint64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, bin%mod)
}

// Verify checks a 6-digit code against secret at time now, accepting the
// current 30 s step and one step either side. A step whose counter is not
// greater than lastCounter is rejected (replay protection). On success it
// returns the matched counter, which the caller must persist.
func Verify(secret, code string, now time.Time, lastCounter uint64) (uint64, bool) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimRight(secret, "=")))
	if err != nil || len(code) != totpDigits || now.Unix() < 0 {
		return 0, false
	}
	cur := uint64(now.Unix()) / totpStep
	var matched uint64
	found := false
	for d := -totpSkew; d <= totpSkew; d++ {
		if d < 0 && cur < uint64(-d) {
			continue
		}
		c := cur + uint64(int64(d))
		eq := subtle.ConstantTimeCompare([]byte(hotp(key, c, totpDigits)), []byte(code)) == 1
		if eq && c > lastCounter {
			matched, found = c, true
		}
	}
	return matched, found
}
