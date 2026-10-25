package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Exposure sources.
const (
	ExposureSourceLabels  = "labels"
	ExposureSourceProject = "project"
)

// Exposure publishes one project service on a set of domains. Validation is the
// caller's job; the store only enforces domain uniqueness.
type Exposure struct {
	Service string
	Port    int
	Domains []string
	// Path is an optional path prefix ("" for none).
	Path string
	// Source is "labels" (from the compose file) or "project" (set through the
	// API). Set by the store; ignored on input.
	Source    string
	UpdatedAt time.Time
}

// ProjectExposure is an effective Exposure with its owning project slug.
type ProjectExposure struct {
	Project string
	Exposure
}

// DomainConflictError reports a domain already routed to another service (of
// this or another project). Use errors.As to retrieve it.
type DomainConflictError struct {
	Domain, Project, Service string
}

func (e *DomainConflictError) Error() string {
	return fmt.Sprintf("store: domain %q is already exposed by %s/%s", e.Domain, e.Project, e.Service)
}

// claimDomains registers the domains for (pid, service, source) inside tx. A
// domain held by anything else yields *DomainConflictError. With skipTaken,
// taken domains are silently left unregistered instead.
func claimDomains(ctx context.Context, tx *sql.Tx, pid int64, service, source string, domains []string, skipTaken bool) error {
	seen := map[string]bool{}
	for _, d := range domains {
		if seen[d] {
			continue
		}
		seen[d] = true
		var owner, svc string
		err := tx.QueryRowContext(ctx, `SELECT p.slug, e.service FROM exposure_domains e
			JOIN projects p ON p.id = e.project_id WHERE e.domain = ?`, d).Scan(&owner, &svc)
		if err == nil {
			if skipTaken {
				continue
			}
			return &DomainConflictError{Domain: d, Project: owner, Service: svc}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: check domain: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO exposure_domains (domain, project_id, service, source)
			VALUES (?, ?, ?, ?)`, d, pid, service, source); err != nil {
			return fmt.Errorf("store: register domain: %w", err)
		}
	}
	return nil
}

func insertExposure(ctx context.Context, tx *sql.Tx, pid int64, e Exposure, source string, t time.Time) error {
	domains := e.Domains
	if domains == nil {
		domains = []string{}
	}
	b, err := json.Marshal(domains)
	if err != nil {
		return fmt.Errorf("store: encode domains: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO project_exposures
		(project_id, service, port, domains, path, source, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		pid, e.Service, e.Port, string(b), e.Path, source, stamp(t)); err != nil {
		if mapErr(err) == ErrExists {
			return ErrExists
		}
		return fmt.Errorf("store: insert exposure: %w", err)
	}
	return nil
}

// SetProjectExposure stores e (source "project") as the project's entry for
// e.Service, replacing any previous project entry and its domains. It overrides
// a label entry for the same service: that entry's domains are released and
// stay unregistered while the project entry exists. A domain routed to any other
// service (even of the same project) returns *DomainConflictError and changes
// nothing. It returns ErrNotFound for an unknown slug.
func (s *Store) SetProjectExposure(ctx context.Context, slug string, e Exposure) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: set exposure: %w", err)
	}
	defer tx.Rollback()
	pid, err := projectID(ctx, tx, slug)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM exposure_domains WHERE project_id = ? AND service = ?`, pid, e.Service); err != nil {
		return fmt.Errorf("store: set exposure: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_exposures WHERE project_id = ? AND service = ? AND source = ?`,
		pid, e.Service, ExposureSourceProject); err != nil {
		return fmt.Errorf("store: set exposure: %w", err)
	}
	if err := claimDomains(ctx, tx, pid, e.Service, ExposureSourceProject, e.Domains, false); err != nil {
		return err
	}
	if err := insertExposure(ctx, tx, pid, e, ExposureSourceProject, now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: set exposure: %w", err)
	}
	return nil
}

// RemoveProjectExposure deletes the project entry of the service (a no-op when
// there is none). A label entry for the service, if any, becomes effective again
// and its domains are registered; domains meanwhile taken by another service are
// left unrouted. It returns ErrNotFound for an unknown slug.
func (s *Store) RemoveProjectExposure(ctx context.Context, slug, service string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: remove exposure: %w", err)
	}
	defer tx.Rollback()
	pid, err := projectID(ctx, tx, slug)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM project_exposures WHERE project_id = ? AND service = ? AND source = ?`,
		pid, service, ExposureSourceProject)
	if err != nil {
		return fmt.Errorf("store: remove exposure: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM exposure_domains WHERE project_id = ? AND service = ?`, pid, service); err != nil {
			return fmt.Errorf("store: remove exposure: %w", err)
		}
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT domains FROM project_exposures
			WHERE project_id = ? AND service = ? AND source = ?`, pid, service, ExposureSourceLabels).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: remove exposure: %w", err)
		}
		if err == nil {
			var domains []string
			if err := json.Unmarshal([]byte(raw), &domains); err != nil {
				return fmt.Errorf("store: decode domains: %w", err)
			}
			if err := claimDomains(ctx, tx, pid, service, ExposureSourceLabels, domains, true); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: remove exposure: %w", err)
	}
	return nil
}

// ReplaceLabelExposures atomically replaces all of the project's label entries
// with es. Domains of an entry whose service has a project entry are not
// registered (the project entry overrides), so they cannot conflict. Any other
// domain routed elsewhere returns *DomainConflictError and changes nothing. A
// repeated service returns ErrExists. It returns ErrNotFound for an unknown slug.
func (s *Store) ReplaceLabelExposures(ctx context.Context, slug string, es []Exposure) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: replace label exposures: %w", err)
	}
	defer tx.Rollback()
	pid, err := projectID(ctx, tx, slug)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM exposure_domains WHERE project_id = ? AND source = ?`, pid, ExposureSourceLabels); err != nil {
		return fmt.Errorf("store: replace label exposures: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_exposures WHERE project_id = ? AND source = ?`, pid, ExposureSourceLabels); err != nil {
		return fmt.Errorf("store: replace label exposures: %w", err)
	}
	t := now()
	for _, e := range es {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_exposures
			WHERE project_id = ? AND service = ? AND source = ?`, pid, e.Service, ExposureSourceProject).Scan(&n); err != nil {
			return fmt.Errorf("store: replace label exposures: %w", err)
		}
		if n == 0 {
			if err := claimDomains(ctx, tx, pid, e.Service, ExposureSourceLabels, e.Domains, false); err != nil {
				return err
			}
		}
		if err := insertExposure(ctx, tx, pid, e, ExposureSourceLabels, t); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: replace label exposures: %w", err)
	}
	return nil
}

// effectiveExposures is the shared query: project entries plus label entries of
// services without a project entry.
const effectiveExposures = `SELECT p.slug, x.service, x.port, x.domains, x.path, x.source, x.updated_at
	FROM project_exposures x JOIN projects p ON p.id = x.project_id
	WHERE (x.source = 'project' OR NOT EXISTS (SELECT 1 FROM project_exposures y
		WHERE y.project_id = x.project_id AND y.service = x.service AND y.source = 'project'))`

func queryExposures(ctx context.Context, db *sql.DB, extra string, args ...any) ([]ProjectExposure, error) {
	rows, err := db.QueryContext(ctx, effectiveExposures+extra+` ORDER BY p.slug, x.service`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list exposures: %w", err)
	}
	defer rows.Close()
	var out []ProjectExposure
	for rows.Next() {
		var pe ProjectExposure
		var raw, updated string
		if err := rows.Scan(&pe.Project, &pe.Service, &pe.Port, &raw, &pe.Path, &pe.Source, &updated); err != nil {
			return nil, fmt.Errorf("store: list exposures: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &pe.Domains); err != nil {
			return nil, fmt.Errorf("store: decode domains: %w", err)
		}
		if pe.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
			return nil, fmt.Errorf("store: parse updated_at: %w", err)
		}
		pe.UpdatedAt = pe.UpdatedAt.UTC()
		out = append(out, pe)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list exposures: %w", err)
	}
	return out, nil
}

// ListExposures returns the project's effective exposures (project entries plus
// label entries not overridden) sorted by service, or ErrNotFound for an unknown
// slug.
func (s *Store) ListExposures(ctx context.Context, slug string) ([]Exposure, error) {
	if _, err := projectID(ctx, s.db, slug); err != nil {
		return nil, err
	}
	pes, err := queryExposures(ctx, s.db, ` AND p.slug = ?`, slug)
	if err != nil {
		return nil, err
	}
	out := make([]Exposure, len(pes))
	for i, pe := range pes {
		out[i] = pe.Exposure
	}
	return out, nil
}

// AllExposures returns every project's effective exposures, sorted by project
// slug then service. It is the input for edge routing.
func (s *Store) AllExposures(ctx context.Context) ([]ProjectExposure, error) {
	return queryExposures(ctx, s.db, ``)
}

// DomainOwner returns the project slug and service routing the domain, or
// ErrNotFound when it is unregistered.
func (s *Store) DomainOwner(ctx context.Context, domain string) (project, service string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT p.slug, e.service FROM exposure_domains e
		JOIN projects p ON p.id = e.project_id WHERE e.domain = ?`, domain).Scan(&project, &service)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("store: domain owner: %w", err)
	}
	return project, service, nil
}
