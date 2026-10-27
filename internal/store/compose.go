package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ComposeVersion is one stored version of a compose project's compose file.
type ComposeVersion struct {
	// Version numbers start at 1 and only grow, per project.
	Version int
	// Content is empty in ListComposeVersions results.
	Content   string
	CreatedAt time.Time
	CreatedBy string
	Note      string
}

// AddComposeVersion stores content as the project's next version (max+1, or 1)
// and returns the number. It returns ErrNotFound for an unknown slug.
func (s *Store) AddComposeVersion(ctx context.Context, slug, content, createdBy, note string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: add compose version: %w", err)
	}
	defer tx.Rollback()
	id, err := projectID(ctx, tx, slug)
	if err != nil {
		return 0, err
	}
	var version int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM project_compose WHERE project_id = ?`, id).Scan(&version); err != nil {
		return 0, fmt.Errorf("store: add compose version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO project_compose (project_id, version, content, created_at, created_by, note)
		VALUES (?, ?, ?, ?, ?, ?)`, id, version, content, stamp(now()), createdBy, note); err != nil {
		return 0, fmt.Errorf("store: add compose version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: add compose version: %w", err)
	}
	return version, nil
}

// LatestComposeVersion returns the project's highest version number, 0 when it
// has none. It returns ErrNotFound for an unknown slug.
func (s *Store) LatestComposeVersion(ctx context.Context, slug string) (int, error) {
	id, err := projectID(ctx, s.db, slug)
	if err != nil {
		return 0, err
	}
	var v int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM project_compose WHERE project_id = ?`, id).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: latest compose version: %w", err)
	}
	return v, nil
}

// GetComposeVersion returns the given version of the project's compose file
// (0 = latest). It returns ErrNotFound for an unknown slug or version.
func (s *Store) GetComposeVersion(ctx context.Context, slug string, version int) (*ComposeVersion, error) {
	id, err := projectID(ctx, s.db, slug)
	if err != nil {
		return nil, err
	}
	q := `SELECT version, content, created_at, created_by, note FROM project_compose WHERE project_id = ?`
	args := []any{id}
	if version > 0 {
		q += ` AND version = ?`
		args = append(args, version)
	} else {
		q += ` ORDER BY version DESC LIMIT 1`
	}
	var v ComposeVersion
	var created string
	err = s.db.QueryRowContext(ctx, q, args...).Scan(&v.Version, &v.Content, &created, &v.CreatedBy, &v.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get compose version: %w", err)
	}
	if v.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return nil, fmt.Errorf("store: parse created_at: %w", err)
	}
	v.CreatedAt = v.CreatedAt.UTC()
	return &v, nil
}

// ListComposeVersions returns the project's versions newest first, without
// content; a positive limit caps the length. It returns ErrNotFound for an
// unknown slug.
func (s *Store) ListComposeVersions(ctx context.Context, slug string, limit int) ([]ComposeVersion, error) {
	id, err := projectID(ctx, s.db, slug)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	rows, err := s.db.QueryContext(ctx, `SELECT version, created_at, created_by, note
		FROM project_compose WHERE project_id = ? ORDER BY version DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list compose versions: %w", err)
	}
	defer rows.Close()
	var out []ComposeVersion
	for rows.Next() {
		var v ComposeVersion
		var created string
		if err := rows.Scan(&v.Version, &created, &v.CreatedBy, &v.Note); err != nil {
			return nil, fmt.Errorf("store: list compose versions: %w", err)
		}
		if v.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("store: parse created_at: %w", err)
		}
		v.CreatedAt = v.CreatedAt.UTC()
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list compose versions: %w", err)
	}
	return out, nil
}

// PruneComposeVersions deletes the project's oldest versions, keeping the
// newest keep and never deleting version protect (0 protects nothing). It
// returns the number removed, or ErrNotFound for an unknown slug. keep < 1 is a
// no-op.
func (s *Store) PruneComposeVersions(ctx context.Context, slug string, keep, protect int) (int, error) {
	id, err := projectID(ctx, s.db, slug)
	if err != nil {
		return 0, err
	}
	if keep < 1 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM project_compose WHERE project_id = ? AND version != ?
		AND id NOT IN (SELECT id FROM project_compose WHERE project_id = ? ORDER BY version DESC LIMIT ?)`,
		id, protect, id, keep)
	if err != nil {
		return 0, fmt.Errorf("store: prune compose versions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune compose versions: %w", err)
	}
	return int(n), nil
}
