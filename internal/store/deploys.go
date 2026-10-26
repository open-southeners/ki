package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EnvVar is a persisted project environment variable. ValueEnc is sealed by
// internal/secret with aad "<slug>/env/<Name>"; the store never sees plaintext.
type EnvVar struct {
	Name      string
	ValueEnc  []byte
	UpdatedAt time.Time
}

// Deploy is a persisted deploy row.
type Deploy struct {
	ID int64
	// ProjectID and Project (the slug) identify the owning project. On
	// CreateDeploy only Project is read.
	ProjectID int64
	Project   string
	// Ref is the requested ref; Commit the resolved sha ("" until checked out).
	Ref, Commit string
	// Source is "webhook", "api" or "cli".
	Source string
	// Status is queued, running, succeeded, failed, superseded or interrupted.
	Status string
	// Stage is the last pipeline stage reached.
	Stage string
	// RollbackOf is the id of the deploy this one re-deploys, 0 if it is not
	// a rollback. Set on CreateDeploy only.
	RollbackOf int64
	// ErrorReason and ErrorDetail describe a failure; ErrorData is opaque JSON
	// ("" when none) owned by the caller.
	ErrorReason, ErrorDetail, ErrorData string
	// CreatedAt is UTC and set by the store. StartedAt and FinishedAt are nil
	// until set by the caller.
	CreatedAt             time.Time
	StartedAt, FinishedAt *time.Time
}

// DeployLog is one stored log line of a deploy.
type DeployLog struct {
	Seq int64
	// Stream is "stdout", "stderr" or "agent".
	Stream, Text string
}

// projectID resolves a slug inside q, returning ErrNotFound when unknown.
func projectID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, slug string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM projects WHERE slug = ?`, slug).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: resolve project: %w", err)
	}
	return id, nil
}

func isForeignKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

// SetEnv inserts or replaces the given variables (name to sealed value) of the
// project, leaving others untouched. It returns ErrNotFound for an unknown slug.
func (s *Store) SetEnv(ctx context.Context, slug string, vars map[string][]byte) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: set env: %w", err)
	}
	defer tx.Rollback()
	id, err := projectID(ctx, tx, slug)
	if err != nil {
		return err
	}
	t := stamp(now())
	for name, v := range vars {
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_env (project_id, name, value_enc, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (project_id, name) DO UPDATE SET value_enc = excluded.value_enc,
				updated_at = excluded.updated_at`, id, name, blob(v), t); err != nil {
			return fmt.Errorf("store: set env: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: set env: %w", err)
	}
	return nil
}

// UnsetEnv removes the named variables; names that are not set are ignored. It
// returns ErrNotFound for an unknown slug.
func (s *Store) UnsetEnv(ctx context.Context, slug string, names []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: unset env: %w", err)
	}
	defer tx.Rollback()
	id, err := projectID(ctx, tx, slug)
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, err := tx.ExecContext(ctx, `DELETE FROM project_env WHERE project_id = ? AND name = ?`, id, name); err != nil {
			return fmt.Errorf("store: unset env: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: unset env: %w", err)
	}
	return nil
}

// ListEnv returns the project's variables sorted by name, or ErrNotFound for an
// unknown slug.
func (s *Store) ListEnv(ctx context.Context, slug string) ([]EnvVar, error) {
	id, err := projectID(ctx, s.db, slug)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, value_enc, updated_at FROM project_env WHERE project_id = ? ORDER BY name`, id)
	if err != nil {
		return nil, fmt.Errorf("store: list env: %w", err)
	}
	defer rows.Close()
	var out []EnvVar
	for rows.Next() {
		var v EnvVar
		var updated string
		if err := rows.Scan(&v.Name, &v.ValueEnc, &updated); err != nil {
			return nil, fmt.Errorf("store: list env: %w", err)
		}
		if v.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
			return nil, fmt.Errorf("store: parse updated_at: %w", err)
		}
		v.UpdatedAt = v.UpdatedAt.UTC()
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list env: %w", err)
	}
	return out, nil
}

const deployColumns = `d.id, d.project_id, p.slug, d.ref, d.commit_sha, d.source, d.status, d.stage,
	d.error_reason, d.error_detail, d.error_data, d.created_at, d.started_at, d.finished_at,
	d.rollback_of`

const deployFrom = ` FROM deploys d JOIN projects p ON p.id = d.project_id`

func nullID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

func nullStamp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return stamp(*t)
}

func parseNullStamp(ns sql.NullString, what string) (*time.Time, error) {
	if !ns.Valid {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, ns.String)
	if err != nil {
		return nil, fmt.Errorf("store: parse %s: %w", what, err)
	}
	t = t.UTC()
	return &t, nil
}

func scanDeploy(sc scanner) (*Deploy, error) {
	var d Deploy
	var created string
	var started, finished sql.NullString
	var rollbackOf sql.NullInt64
	err := sc.Scan(&d.ID, &d.ProjectID, &d.Project, &d.Ref, &d.Commit, &d.Source, &d.Status,
		&d.Stage, &d.ErrorReason, &d.ErrorDetail, &d.ErrorData, &created, &started, &finished, &rollbackOf)
	if err != nil {
		return nil, err
	}
	d.RollbackOf = rollbackOf.Int64
	if d.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return nil, fmt.Errorf("store: parse created_at: %w", err)
	}
	d.CreatedAt = d.CreatedAt.UTC()
	if d.StartedAt, err = parseNullStamp(started, "started_at"); err != nil {
		return nil, err
	}
	if d.FinishedAt, err = parseNullStamp(finished, "finished_at"); err != nil {
		return nil, err
	}
	return &d, nil
}

// CreateDeploy inserts d for the project d.Project, setting its ID, ProjectID and
// CreatedAt. It returns ErrNotFound for an unknown project.
func (s *Store) CreateDeploy(ctx context.Context, d *Deploy) error {
	pid, err := projectID(ctx, s.db, d.Project)
	if err != nil {
		return err
	}
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO deploys (project_id, ref, commit_sha, source,
		status, stage, error_reason, error_detail, error_data, created_at, started_at, finished_at, rollback_of)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		pid, d.Ref, d.Commit, d.Source, d.Status, d.Stage, d.ErrorReason, d.ErrorDetail,
		d.ErrorData, stamp(t), nullStamp(d.StartedAt), nullStamp(d.FinishedAt), nullID(d.RollbackOf))
	if err != nil {
		if isForeignKey(err) {
			return ErrNotFound // project deleted since the lookup
		}
		return fmt.Errorf("store: create deploy: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: create deploy: %w", err)
	}
	d.ID, d.ProjectID, d.CreatedAt = id, pid, t
	return nil
}

// UpdateDeploy overwrites the mutable columns (everything but the id, project
// and creation time) of the deploy identified by d.ID. It returns ErrNotFound
// if there is no such deploy.
func (s *Store) UpdateDeploy(ctx context.Context, d *Deploy) error {
	res, err := s.db.ExecContext(ctx, `UPDATE deploys SET ref = ?, commit_sha = ?, source = ?,
		status = ?, stage = ?, error_reason = ?, error_detail = ?, error_data = ?,
		started_at = ?, finished_at = ?
		WHERE id = ?`,
		d.Ref, d.Commit, d.Source, d.Status, d.Stage, d.ErrorReason, d.ErrorDetail, d.ErrorData,
		nullStamp(d.StartedAt), nullStamp(d.FinishedAt), d.ID)
	if err != nil {
		return fmt.Errorf("store: update deploy: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update deploy: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetDeploy returns the deploy with the given id, or ErrNotFound.
func (s *Store) GetDeploy(ctx context.Context, id int64) (*Deploy, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+deployColumns+deployFrom+` WHERE d.id = ?`, id)
	d, err := scanDeploy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get deploy: %w", err)
	}
	return d, nil
}

// ListDeploys returns the project's deploys newest first. A positive beforeID
// restricts the result to ids below it and a positive limit caps its length. It
// returns ErrNotFound for an unknown slug.
func (s *Store) ListDeploys(ctx context.Context, slug string, limit int, beforeID int64) ([]*Deploy, error) {
	pid, err := projectID(ctx, s.db, slug)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+deployColumns+deployFrom+`
		WHERE d.project_id = ? AND (? <= 0 OR d.id < ?) ORDER BY d.id DESC LIMIT ?`,
		pid, beforeID, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list deploys: %w", err)
	}
	defer rows.Close()
	var out []*Deploy
	for rows.Next() {
		d, err := scanDeploy(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list deploys: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list deploys: %w", err)
	}
	return out, nil
}

// AppendDeployLog stores one log line. It returns ErrNotFound for an unknown
// deploy and ErrExists if seq is already used.
func (s *Store) AppendDeployLog(ctx context.Context, deployID, seq int64, stream, text string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO deploy_logs (deploy_id, seq, stream, text) VALUES (?, ?, ?, ?)`,
		deployID, seq, stream, text)
	if err != nil {
		if err = mapErr(err); err == ErrExists {
			return err
		}
		if isForeignKey(err) {
			return ErrNotFound
		}
		return fmt.Errorf("store: append deploy log: %w", err)
	}
	return nil
}

// DeployLogs returns the deploy's log lines with seq greater than sinceSeq, in
// seq order. An unknown deploy yields no lines.
func (s *Store) DeployLogs(ctx context.Context, deployID, sinceSeq int64) ([]DeployLog, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, stream, text FROM deploy_logs WHERE deploy_id = ? AND seq > ? ORDER BY seq`,
		deployID, sinceSeq)
	if err != nil {
		return nil, fmt.Errorf("store: deploy logs: %w", err)
	}
	defer rows.Close()
	var out []DeployLog
	for rows.Next() {
		var l DeployLog
		if err := rows.Scan(&l.Seq, &l.Stream, &l.Text); err != nil {
			return nil, fmt.Errorf("store: deploy logs: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: deploy logs: %w", err)
	}
	return out, nil
}

// PruneDeploys deletes the project's oldest deploys (and their logs), keeping
// the newest keep. Queued and running deploys are never deleted. It returns the
// number removed, or ErrNotFound for an unknown slug. keep < 1 is a no-op.
func (s *Store) PruneDeploys(ctx context.Context, slug string, keep int) (int, error) {
	pid, err := projectID(ctx, s.db, slug)
	if err != nil {
		return 0, err
	}
	if keep < 1 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM deploys WHERE project_id = ?
		AND status NOT IN ('queued', 'running')
		AND id NOT IN (SELECT id FROM deploys WHERE project_id = ? ORDER BY id DESC LIMIT ?)`,
		pid, pid, keep)
	if err != nil {
		return 0, fmt.Errorf("store: prune deploys: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune deploys: %w", err)
	}
	return int(n), nil
}

// InterruptUnfinishedDeploys marks every queued or running deploy interrupted
// (setting finished_at) and returns how many it changed. Call it at startup.
func (s *Store) InterruptUnfinishedDeploys(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE deploys SET status = 'interrupted', finished_at = ?
		WHERE status IN ('queued', 'running')`, stamp(now()))
	if err != nil {
		return 0, fmt.Errorf("store: interrupt deploys: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: interrupt deploys: %w", err)
	}
	return int(n), nil
}
