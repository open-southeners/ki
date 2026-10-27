package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-southeners/ki/internal/update"
)

func TestKeygenModeAndNoOverwrite(t *testing.T) {
	out := filepath.Join(t.TempDir(), "key")
	var buf bytes.Buffer
	if err := keygen(out, &buf); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(out)
	seed := strings.TrimSpace(string(raw))
	if strings.Contains(buf.String(), seed) {
		t.Fatal("keygen printed the private seed")
	}
	if !strings.Contains(buf.String(), "ed25519.PublicKey{0x") {
		t.Fatalf("missing map entry in output: %s", buf.String())
	}
	if err := keygen(out, &buf); err == nil {
		t.Fatal("keygen overwrote an existing file")
	}
	if after, _ := os.ReadFile(out); !bytes.Equal(raw, after) {
		t.Fatal("existing key file was modified")
	}
}

func TestSignRoundTrip(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := keygen(keyFile, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(keyFile)
	seedB64 := strings.TrimSpace(string(raw))
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil {
		t.Fatal(err)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)

	sums := []byte("abc  ki-agent_linux_amd64.tar.gz\n")
	in, sigPath := filepath.Join(dir, "checksums.txt"), filepath.Join(dir, "checksums.txt.sig")
	if err := os.WriteFile(in, sums, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sign("v1.2.3", in, sigPath, seedB64); err != nil {
		t.Fatal(err)
	}
	sig, _ := os.ReadFile(sigPath)
	keys := map[string]ed25519.PublicKey{update.KeyID(pub): pub}
	if err := update.Verify(keys, "v1.2.3", sums, sig); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := update.Verify(keys, "v1.2.4", sums, sig); err == nil {
		t.Fatal("signature verified for a different tag")
	}
}

func TestSignBadKey(t *testing.T) {
	in := filepath.Join(t.TempDir(), "c")
	_ = os.WriteFile(in, []byte("x\n"), 0o644)
	for _, k := range []string{"", "not base64!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		err := sign("v1.0.0", in, in+".sig", k)
		if err == nil {
			t.Fatalf("key %q accepted", k)
		}
		if k != "" && strings.Contains(err.Error(), k) {
			t.Fatalf("error echoes the key: %v", err)
		}
	}
}
