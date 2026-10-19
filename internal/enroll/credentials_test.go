package enroll

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleCreds() Credentials {
	return Credentials{
		URL:           "wss://cp.example.test/agent",
		Token:         testToken,
		EnrolledAt:    time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
		EnrollmentURL: "https://cp.example.test",
	}
}

func TestCredentialsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "control-plane.json")
	want := sampleCreds()
	if err := SaveCredentials(path, want); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	got, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if *got != want {
		t.Errorf("got %+v, want %+v", *got, want)
	}

	// Overwrite leaves exactly one file and keeps the mode.
	want.Token = "other"
	if err := SaveCredentials(path, want); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	if len(ents) != 1 {
		t.Errorf("directory has %d entries, want 1 (no temp files left)", len(ents))
	}
	got, err = LoadCredentials(path)
	if err != nil || got.Token != "other" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestSaveCredentialsAtomicOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control-plane.json")
	if err := SaveCredentials(path, sampleCreds()); err != nil {
		t.Fatal(err)
	}
	// A directory at the destination makes the rename fail; the old data and
	// the directory must be left clean of temp files.
	bad := filepath.Join(dir, "dest")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveCredentials(bad, sampleCreds()); err == nil {
		t.Fatal("expected error renaming over a directory")
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
	if _, err := LoadCredentials(path); err != nil {
		t.Errorf("original credentials damaged: %v", err)
	}
}

func TestLoadCredentialsErrors(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := `{"url":"wss://x.test/a","token":"` + testToken + `"}`
	tests := []struct {
		name string
		path string
		want string
	}{
		{"group readable", write("g", good, 0o640), "mode 0640"},
		{"world readable", write("w", good, 0o604), "mode 0604"},
		{"group writable", write("gw", good, 0o620), "mode 0620"},
		{"bad json", write("j", `{"token":"`+testToken, 0o600), "not valid credentials JSON"},
		{"missing token", write("t", `{"url":"wss://x.test/a"}`, 0o600), "missing url or token"},
		{"directory", dir, "not a regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadCredentials(tt.path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatalf("error leaks token: %v", err)
			}
		})
	}
}

func TestCredentialsNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")
	if _, err := LoadCredentials(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load err = %v, want ErrNotExist", err)
	}
	if err := RemoveCredentials(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Remove err = %v, want ErrNotExist", err)
	}
}

func TestRemoveCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	if err := SaveCredentials(path, sampleCreds()); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCredentials(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file still exists: %v", err)
	}
}
