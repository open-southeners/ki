package secret

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func newBox(t *testing.T) *Box {
	t.Helper()
	b, err := LoadOrCreate(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	b := newBox(t)
	for _, pt := range [][]byte{[]byte("hunter2"), {}, bytes.Repeat([]byte{0xff}, 4096)} {
		ct := b.Seal(pt, []byte("app/token"))
		got, err := b.Open(ct, []byte("app/token"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("got %q want %q", got, pt)
		}
	}
}

func TestSealRandomNonce(t *testing.T) {
	b := newBox(t)
	if bytes.Equal(b.Seal([]byte("x"), nil), b.Seal([]byte("x"), nil)) {
		t.Fatal("two seals produced identical output")
	}
}

func TestOpenFailures(t *testing.T) {
	b := newBox(t)
	ct := b.Seal([]byte("secret"), []byte("app/token"))
	flipped := bytes.Clone(ct)
	flipped[len(flipped)-1] ^= 1
	flippedNonce := bytes.Clone(ct)
	flippedNonce[0] ^= 1
	tests := []struct {
		name string
		ct   []byte
		aad  []byte
	}{
		{"wrong aad", ct, []byte("other/token")},
		{"nil aad", ct, nil},
		{"tampered body", flipped, []byte("app/token")},
		{"tampered nonce", flippedNonce, []byte("app/token")},
		{"truncated", ct[:5], []byte("app/token")},
		{"empty", nil, []byte("app/token")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := b.Open(tt.ct, tt.aad); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestOtherKeyCannotOpen(t *testing.T) {
	a, b := newBox(t), newBox(t)
	if _, err := b.Open(a.Seal([]byte("x"), nil), nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestKeyFileCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	if _, err := LoadOrCreate(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != KeySize {
		t.Fatalf("mode %v size %d", info.Mode().Perm(), info.Size())
	}
}

func TestKeyReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	a, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Open(a.Seal([]byte("x"), nil), nil)
	if err != nil || string(got) != "x" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestInvalidKeyFile(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{"too short", make([]byte, 16), 0o600},
		{"too long", make([]byte, 64), 0o600},
		{"empty", nil, 0o600},
		{"group readable", make([]byte, KeySize), 0o640},
		{"world readable", make([]byte, KeySize), 0o604},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secret.key")
			if err := os.WriteFile(path, tt.data, tt.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadOrCreate(path); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestStricterPermsAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(path, make([]byte, KeySize), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(path); err != nil {
		t.Fatal(err)
	}
}

func TestMissingDir(t *testing.T) {
	if _, err := LoadOrCreate(filepath.Join(t.TempDir(), "nope", "secret.key")); err == nil {
		t.Fatal("expected error")
	}
}
