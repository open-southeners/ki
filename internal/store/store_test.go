package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func sample(slug, webhook string) *Project {
	return &Project{
		Slug: slug, RepoURL: "git@github.com:acme/" + slug + ".git", Branch: "main",
		DefaultBranch: "main", ComposeFile: "compose.yaml", AuthMethod: "ssh_key",
		PublicKey: "ssh-ed25519 AAAA ki-agent:" + slug, WebhookID: webhook,
		Status: "pending_access",
	}
}

func TestOpenCreatesFile0600(t *testing.T) {
	_, path := openStore(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
}

func TestOpenPragmas(t *testing.T) {
	s, _ := openStore(t)
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode %q err %v", mode, err)
	}
	var fk, busy int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys %d err %v", fk, err)
	}
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil || busy != 5000 {
		t.Fatalf("busy_timeout %d err %v", busy, err)
	}
}

func TestMigrationIdempotent(t *testing.T) {
	ctx := context.Background()
	s, path := openStore(t)
	if err := s.CreateProject(ctx, sample("app", "w1")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var v int
	if err := s2.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("user_version %d err %v", v, err)
	}
	if _, err := s2.GetProject(ctx, "app"); err != nil {
		t.Fatalf("data lost across reopen: %v", err)
	}
}

func TestNewerSchemaRejected(t *testing.T) {
	s, path := openStore(t)
	if _, err := s.db.Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("expected error")
	}
}

func TestCRUD(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	p := sample("app", "w1")
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if p.ID == 0 || p.CreatedAt.IsZero() || !p.CreatedAt.Equal(p.UpdatedAt) || p.CreatedAt.Location() != time.UTC {
		t.Fatalf("create did not set id/timestamps: %+v", p)
	}
	got, err := s.GetProject(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != p.ID || got.RepoURL != p.RepoURL || got.AuthMethod != "ssh_key" ||
		got.PublicKey != p.PublicKey || got.Status != "pending_access" ||
		!got.CreatedAt.Equal(p.CreatedAt) || !got.UpdatedAt.Equal(p.UpdatedAt) {
		t.Fatalf("got %+v want %+v", got, p)
	}
	byHook, err := s.GetProjectByWebhookID(ctx, "w1")
	if err != nil || byHook.Slug != "app" {
		t.Fatalf("by webhook: %+v %v", byHook, err)
	}

	time.Sleep(2 * time.Millisecond)
	got.Status, got.Commit, got.StatusDetail = "ready", "abc123", "x"
	if err := s.UpdateProject(ctx, got); err != nil {
		t.Fatal(err)
	}
	if !got.UpdatedAt.After(p.UpdatedAt) {
		t.Fatalf("updated_at not bumped: %v vs %v", got.UpdatedAt, p.UpdatedAt)
	}
	again, err := s.GetProject(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != "ready" || again.Commit != "abc123" || again.StatusDetail != "x" ||
		!again.UpdatedAt.Equal(got.UpdatedAt) || !again.CreatedAt.Equal(p.CreatedAt) {
		t.Fatalf("update not persisted: %+v", again)
	}

	if err := s.DeleteProject(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProject(ctx, "app"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestErrExists(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	if err := s.CreateProject(ctx, sample("app", "w1")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		p    *Project
	}{
		{"slug", sample("app", "w2")},
		{"webhook id", sample("other", "w1")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := s.CreateProject(ctx, tt.p); !errors.Is(err, ErrExists) {
				t.Fatalf("got %v", err)
			}
		})
	}
	// Updating onto a taken webhook id also clashes.
	o := sample("other", "w2")
	if err := s.CreateProject(ctx, o); err != nil {
		t.Fatal(err)
	}
	o.WebhookID = "w1"
	if err := s.UpdateProject(ctx, o); !errors.Is(err, ErrExists) {
		t.Fatalf("got %v", err)
	}
}

func TestErrNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	if _, err := s.GetProject(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get: %v", err)
	}
	if _, err := s.GetProjectByWebhookID(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get by webhook: %v", err)
	}
	if err := s.UpdateProject(ctx, sample("nope", "w")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update: %v", err)
	}
	if err := s.DeleteProject(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete: %v", err)
	}
}

func TestListOrdering(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	got, err := s.ListProjects(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty list: %v %v", got, err)
	}
	for i, slug := range []string{"zeta", "alpha", "mid"} {
		if err := s.CreateProject(ctx, sample(slug, "w"+slug)); err != nil {
			t.Fatal(i, err)
		}
	}
	got, err = s.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var slugs []string
	for _, p := range got {
		slugs = append(slugs, p.Slug)
	}
	if len(slugs) != 3 || slugs[0] != "alpha" || slugs[1] != "mid" || slugs[2] != "zeta" {
		t.Fatalf("order %v", slugs)
	}
}

func TestBlobsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	p := sample("app", "w1")
	p.PrivateKeyEnc = []byte{0, 1, 2, 0, 255}
	p.TokenEnc = bytes.Repeat([]byte{0xab}, 1000)
	p.WebhookSecretEnc = []byte{9, 0, 9}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetProject(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.PrivateKeyEnc, p.PrivateKeyEnc) || !bytes.Equal(got.TokenEnc, p.TokenEnc) ||
		!bytes.Equal(got.WebhookSecretEnc, p.WebhookSecretEnc) {
		t.Fatalf("blobs differ: %+v", got)
	}

	// nil blobs are stored as empty.
	got.PrivateKeyEnc, got.TokenEnc = nil, nil
	if err := s.UpdateProject(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetProject(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PrivateKeyEnc) != 0 || len(got.TokenEnc) != 0 {
		t.Fatalf("expected empty blobs: %+v", got)
	}
}
