package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func signFile(priv ed25519.PrivateKey, tag string, checksums []byte) []byte {
	pub := priv.Public().(ed25519.PublicKey)
	return FormatSignature(KeyID(pub), ed25519.Sign(priv, SignedMessage(tag, checksums)))
}

func TestSignedMessage(t *testing.T) {
	got := string(SignedMessage("v1.2.3", []byte("abc  file\n")))
	want := "ki-agent release v1\ntag: v1.2.3\nabc  file\n"
	if got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestKeyIDFormat(t *testing.T) {
	pub, _ := testKey(t)
	id := KeyID(pub)
	if !validKeyID(id) {
		t.Fatalf("key id %q is not 16 lowercase hex chars", id)
	}
}

func TestSignatureRoundTrip(t *testing.T) {
	pub, priv := testKey(t)
	checksums := []byte("deadbeef  ki-agent_linux_amd64.tar.gz\n")
	sigFile := signFile(priv, "v0.4.0", checksums)

	id, sig, err := ParseSignature(sigFile)
	if err != nil {
		t.Fatal(err)
	}
	if id != KeyID(pub) || len(sig) != ed25519.SignatureSize {
		t.Fatalf("parsed id=%q len=%d", id, len(sig))
	}
	if !bytes.Equal(FormatSignature(id, sig), sigFile) {
		t.Fatal("format/parse is not a round trip")
	}
	keys := map[string]ed25519.PublicKey{KeyID(pub): pub}
	if err := Verify(keys, "v0.4.0", checksums, sigFile); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyFailures(t *testing.T) {
	pub, priv := testKey(t)
	otherPub, _ := testKey(t)
	checksums := []byte("deadbeef  ki-agent_linux_amd64.tar.gz\n")
	sigFile := signFile(priv, "v0.4.0", checksums)
	keys := map[string]ed25519.PublicKey{KeyID(pub): pub}

	tests := []struct {
		name      string
		keys      map[string]ed25519.PublicKey
		tag       string
		checksums []byte
		sigFile   []byte
	}{
		{"tampered checksums", keys, "v0.4.0", append([]byte("x"), checksums...), sigFile},
		{"wrong tag", keys, "v0.3.0", checksums, sigFile},
		{"unknown key", map[string]ed25519.PublicKey{KeyID(otherPub): otherPub}, "v0.4.0", checksums, sigFile},
		{"empty key set", map[string]ed25519.PublicKey{}, "v0.4.0", checksums, sigFile},
		{"nil key set", nil, "v0.4.0", checksums, sigFile},
		{"garbage signature file", keys, "v0.4.0", checksums, []byte("nope\n")},
		{"key id maps to other key", map[string]ed25519.PublicKey{KeyID(pub): otherPub}, "v0.4.0", checksums, sigFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Verify(tt.keys, tt.tag, tt.checksums, tt.sigFile); err == nil {
				t.Fatal("Verify succeeded, want error")
			}
		})
	}
}

func TestParseSignatureRejects(t *testing.T) {
	_, priv := testKey(t)
	good := string(signFile(priv, "v1.0.0", []byte("x")))
	lines := strings.Split(strings.TrimSuffix(good, "\n"), "\n")

	tests := map[string]string{
		"empty":            "",
		"no trailing LF":   strings.TrimSuffix(good, "\n"),
		"extra line":       good + "extra\n",
		"missing line":     lines[0] + "\n" + lines[1] + "\n",
		"blank extra":      good + "\n",
		"wrong header":     "other\n" + lines[1] + "\n" + lines[2] + "\n",
		"CRLF":             strings.ReplaceAll(good, "\n", "\r\n"),
		"upper-case key":   lines[0] + "\n" + strings.ToUpper(lines[1]) + "\n" + lines[2] + "\n",
		"short key id":     lines[0] + "\nkey-id: abcd\n" + lines[2] + "\n",
		"bad base64":       lines[0] + "\n" + lines[1] + "\nsig: !!!\n",
		"short signature":  lines[0] + "\n" + lines[1] + "\nsig: AAAA\n",
		"swapped lines":    lines[0] + "\n" + lines[2] + "\n" + lines[1] + "\n",
		"missing sig mark": lines[0] + "\n" + lines[1] + "\nsignature: AAAA\n",
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseSignature([]byte(in)); err == nil {
				t.Fatal("ParseSignature succeeded, want error")
			}
		})
	}
}

func TestReleaseKeysIsCopy(t *testing.T) {
	keys := ReleaseKeys()
	keys["x"] = ed25519.PublicKey{1}
	if len(ReleaseKeys()) != len(releaseKeys) {
		t.Fatal("ReleaseKeys leaked its internal map")
	}
}
