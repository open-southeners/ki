package update

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRequestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Request{ID: "abc", Version: "v1.2.3-rc.1", RequestedBy: "admin", RequestedAt: time.Now().UTC().Truncate(time.Second)}
	if err := WriteRequest(dir, want); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, RequestFile))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	got, err := ReadRequest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if _, err := os.Lstat(filepath.Join(dir, RequestFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("request not deleted: %v", err)
	}
	if _, err := ReadRequest(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("second read err = %v", err)
	}
}

func TestWriteRequestRejectsBadVersion(t *testing.T) {
	if err := WriteRequest(t.TempDir(), Request{ID: "x", Version: "latest"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestReadRequestRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	mustWrite(t, secret, `{"id":"a","version":"v1.0.0","requested_by":"x","requested_at":"2026-01-01T00:00:00Z"}`, 0o600)
	if err := os.Symlink(secret, filepath.Join(dir, RequestFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRequest(dir); err == nil {
		t.Fatal("expected error")
	}
	if mustRead(t, secret) == "" {
		t.Error("symlink target was damaged")
	}
	if _, err := os.Lstat(filepath.Join(dir, RequestFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("symlink should be removed: %v", err)
	}
}

func TestReadRequestRejectsOversized(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, RequestFile), `{"id":"`+strings.Repeat("a", 5000)+`"}`, 0o640)
	if _, err := ReadRequest(dir); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, RequestFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("request not deleted: %v", err)
	}
}

func TestReadRequestRejectsBadContent(t *testing.T) {
	cases := map[string]string{
		"unknown field":  `{"id":"a","version":"v1.0.0","requested_by":"x","requested_at":"2026-01-01T00:00:00Z","url":"http://evil"}`,
		"bad version":    `{"id":"a","version":"v1.0","requested_by":"x","requested_at":"2026-01-01T00:00:00Z"}`,
		"path traversal": `{"id":"a","version":"v1.0.0/../x","requested_by":"x","requested_at":"2026-01-01T00:00:00Z"}`,
		"not json":       `nope`,
		"trailing data":  `{"id":"a","version":"v1.0.0","requested_by":"x","requested_at":"2026-01-01T00:00:00Z"} {}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			mustWrite(t, filepath.Join(dir, RequestFile), body, 0o640)
			if _, err := ReadRequest(dir); err == nil {
				t.Fatal("expected error")
			}
			if _, err := os.Lstat(filepath.Join(dir, RequestFile)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("request not deleted: %v", err)
			}
		})
	}
}

func TestResultRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Result{
		ID: "abc", Status: ResultRolledBack, From: "v1.0.0", To: "v1.1.0", Error: "boom",
		StartedAt:  time.Now().UTC().Truncate(time.Second),
		FinishedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := WriteResult(dir, want, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, ResultFile))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	got, err := ReadResult(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

func TestReadResultMissing(t *testing.T) {
	if _, err := ReadResult(t.TempDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteResultDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	mustWrite(t, victim, "precious", 0o600)
	if err := os.Symlink(victim, filepath.Join(dir, ResultFile)); err != nil {
		t.Fatal(err)
	}
	if err := WriteResult(dir, Result{ID: "a", Status: ResultSucceeded}, -1, -1); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, victim); got != "precious" {
		t.Errorf("symlink target overwritten: %q", got)
	}
	st, err := os.Lstat(filepath.Join(dir, ResultFile))
	if err != nil || !st.Mode().IsRegular() {
		t.Fatalf("result should be a regular file now: %v %v", st, err)
	}
}

func TestReadResultRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "t")
	mustWrite(t, target, `{"id":"a"}`, 0o600)
	if err := os.Symlink(target, filepath.Join(dir, ResultFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadResult(dir); err == nil {
		t.Fatal("expected error")
	}
}
