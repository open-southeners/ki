// Package store persists agent state in a SQLite database. It holds no
// business logic and performs no encryption: credential columns carry blobs
// already sealed by the caller.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

// ErrNotFound is returned when the requested row does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrExists is returned when a row violates a uniqueness constraint.
var ErrExists = errors.New("store: already exists")

// migrations are applied in order, one transaction each; the number applied is
// tracked in PRAGMA user_version. Never edit an entry once released: append.
var migrations = []string{
	`CREATE TABLE projects (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		slug               TEXT NOT NULL,
		repo_url           TEXT NOT NULL,
		branch             TEXT NOT NULL DEFAULT '',
		tag_pattern        TEXT NOT NULL DEFAULT '',
		default_branch     TEXT NOT NULL DEFAULT '',
		compose_file       TEXT NOT NULL DEFAULT '',
		auth_method        TEXT NOT NULL DEFAULT 'none',
		public_key         TEXT NOT NULL DEFAULT '',
		private_key_enc    BLOB NOT NULL DEFAULT x'',
		token_enc          BLOB NOT NULL DEFAULT x'',
		token_user         TEXT NOT NULL DEFAULT '',
		webhook_id         TEXT NOT NULL,
		webhook_secret_enc BLOB NOT NULL DEFAULT x'',
		status             TEXT NOT NULL DEFAULT '',
		status_detail      TEXT NOT NULL DEFAULT '',
		commit_sha         TEXT NOT NULL DEFAULT '',
		created_at         TEXT NOT NULL,
		updated_at         TEXT NOT NULL,
		UNIQUE (slug),
		UNIQUE (webhook_id)
	)`,
	`CREATE TABLE project_env (
		project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
		name       TEXT NOT NULL,
		value_enc  BLOB NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (project_id, name)
	)`,
	`CREATE TABLE deploys (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
		ref          TEXT NOT NULL DEFAULT '',
		commit_sha   TEXT NOT NULL DEFAULT '',
		source       TEXT NOT NULL DEFAULT '',
		status       TEXT NOT NULL DEFAULT '',
		stage        TEXT NOT NULL DEFAULT '',
		error_reason TEXT NOT NULL DEFAULT '',
		error_detail TEXT NOT NULL DEFAULT '',
		error_data   TEXT NOT NULL DEFAULT '',
		created_at   TEXT NOT NULL,
		started_at   TEXT,
		finished_at  TEXT
	);
	CREATE INDEX deploys_project_id_idx ON deploys (project_id, id DESC);
	CREATE TABLE deploy_logs (
		deploy_id INTEGER NOT NULL REFERENCES deploys(id) ON DELETE CASCADE,
		seq       INTEGER NOT NULL,
		stream    TEXT NOT NULL,
		text      TEXT NOT NULL,
		PRIMARY KEY (deploy_id, seq)
	)`,
}

// Project is a persisted project row.
type Project struct {
	ID                                                            int64
	Slug, RepoURL, Branch, TagPattern, DefaultBranch, ComposeFile string
	// AuthMethod is "none", "ssh_key" or "https_token".
	AuthMethod string
	// PublicKey is the authorized_keys line of the deploy key (ssh_key only).
	PublicKey string
	// PrivateKeyEnc and TokenEnc are sealed by internal/secret with
	// aad "<slug>/<field>".
	PrivateKeyEnc, TokenEnc []byte
	TokenUser               string
	WebhookID               string
	WebhookSecretEnc        []byte
	Status, StatusDetail    string
	Commit                  string
	// CreatedAt and UpdatedAt are UTC and set by the store.
	CreatedAt, UpdatedAt time.Time
}

// Store is a handle on the state database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens (creating with mode 0600 if needed) the database at path in WAL
// mode and applies pending migrations.
//
// Every transaction the store opens is a write, so transactions start
// IMMEDIATE (_txlock): a deferred read-then-write transaction cannot upgrade its
// lock under WAL while another writer is active and fails at once with
// SQLITE_BUSY, bypassing busy_timeout. Plain reads use no transaction and stay
// concurrent.
func Open(path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: create database file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("store: create database file: %w", err)
	}
	u := url.URL{Scheme: "file", Path: path}
	dsn := u.String() + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("store: database schema version %d is newer than supported %d", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		if err := s.applyMigration(ctx, i); err != nil {
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, i int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
		return err
	}
	// PRAGMA does not accept bound parameters; i is not user input.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
		return err
	}
	return tx.Commit()
}

const projectColumns = `id, slug, repo_url, branch, tag_pattern, default_branch, compose_file,
	auth_method, public_key, private_key_enc, token_enc, token_user, webhook_id,
	webhook_secret_enc, status, status_detail, commit_sha, created_at, updated_at`

type scanner interface {
	Scan(dest ...any) error
}

func scanProject(sc scanner) (*Project, error) {
	var p Project
	var created, updated string
	err := sc.Scan(&p.ID, &p.Slug, &p.RepoURL, &p.Branch, &p.TagPattern, &p.DefaultBranch,
		&p.ComposeFile, &p.AuthMethod, &p.PublicKey, &p.PrivateKeyEnc, &p.TokenEnc,
		&p.TokenUser, &p.WebhookID, &p.WebhookSecretEnc, &p.Status, &p.StatusDetail,
		&p.Commit, &created, &updated)
	if err != nil {
		return nil, err
	}
	if p.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return nil, fmt.Errorf("store: parse created_at: %w", err)
	}
	if p.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return nil, fmt.Errorf("store: parse updated_at: %w", err)
	}
	p.CreatedAt, p.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	return &p, nil
}

// blob maps nil to an empty slice so NOT NULL columns accept it.
func blob(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

func now() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

func stamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func mapErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrExists
	}
	return err
}

// CreateProject inserts p, setting its ID and timestamps. It returns ErrExists
// when the slug or webhook id is already taken.
func (s *Store) CreateProject(ctx context.Context, p *Project) error {
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO projects (
		slug, repo_url, branch, tag_pattern, default_branch, compose_file, auth_method,
		public_key, private_key_enc, token_enc, token_user, webhook_id, webhook_secret_enc,
		status, status_detail, commit_sha, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Slug, p.RepoURL, p.Branch, p.TagPattern, p.DefaultBranch, p.ComposeFile, p.AuthMethod,
		p.PublicKey, blob(p.PrivateKeyEnc), blob(p.TokenEnc), p.TokenUser, p.WebhookID,
		blob(p.WebhookSecretEnc), p.Status, p.StatusDetail, p.Commit, stamp(t), stamp(t))
	if err != nil {
		if err = mapErr(err); err == ErrExists {
			return err
		}
		return fmt.Errorf("store: create project: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: create project: %w", err)
	}
	p.ID, p.CreatedAt, p.UpdatedAt = id, t, t
	return nil
}

func (s *Store) getBy(ctx context.Context, column, value string) (*Project, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE `+column+` = ?`, value)
	p, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get project: %w", err)
	}
	return p, nil
}

// GetProject returns the project with the given slug, or ErrNotFound.
func (s *Store) GetProject(ctx context.Context, slug string) (*Project, error) {
	return s.getBy(ctx, "slug", slug)
}

// GetProjectByWebhookID returns the project with the given webhook id, or
// ErrNotFound.
func (s *Store) GetProjectByWebhookID(ctx context.Context, id string) (*Project, error) {
	return s.getBy(ctx, "webhook_id", id)
}

// ListProjects returns all projects sorted by slug.
func (s *Store) ListProjects(ctx context.Context) ([]*Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("store: list projects: %w", err)
	}
	defer rows.Close()
	var out []*Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list projects: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list projects: %w", err)
	}
	return out, nil
}

// UpdateProject overwrites the mutable columns of the project identified by
// p.Slug and bumps UpdatedAt (also on p). It returns ErrNotFound if there is no
// such project and ErrExists if the new webhook id is taken.
func (s *Store) UpdateProject(ctx context.Context, p *Project) error {
	t := now()
	res, err := s.db.ExecContext(ctx, `UPDATE projects SET
		repo_url = ?, branch = ?, tag_pattern = ?, default_branch = ?, compose_file = ?,
		auth_method = ?, public_key = ?, private_key_enc = ?, token_enc = ?, token_user = ?,
		webhook_id = ?, webhook_secret_enc = ?, status = ?, status_detail = ?, commit_sha = ?,
		updated_at = ?
		WHERE slug = ?`,
		p.RepoURL, p.Branch, p.TagPattern, p.DefaultBranch, p.ComposeFile,
		p.AuthMethod, p.PublicKey, blob(p.PrivateKeyEnc), blob(p.TokenEnc), p.TokenUser,
		p.WebhookID, blob(p.WebhookSecretEnc), p.Status, p.StatusDetail, p.Commit,
		stamp(t), p.Slug)
	if err != nil {
		if err = mapErr(err); err == ErrExists {
			return err
		}
		return fmt.Errorf("store: update project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update project: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	p.UpdatedAt = t
	return nil
}

// DeleteProject removes the project with the given slug, or returns
// ErrNotFound.
func (s *Store) DeleteProject(ctx context.Context, slug string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE slug = ?`, slug)
	if err != nil {
		return fmt.Errorf("store: delete project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete project: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
