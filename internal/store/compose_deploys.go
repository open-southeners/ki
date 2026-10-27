package store

import (
	"context"
	"fmt"
)

// DeployedComposeVersions returns the compose versions that at least one
// succeeded deploy of the project ran. It returns ErrNotFound for an unknown
// slug.
func (s *Store) DeployedComposeVersions(ctx context.Context, slug string) (map[int]bool, error) {
	pid, err := projectID(ctx, s.db, slug)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT compose_version FROM deploys
		WHERE project_id = ? AND status = 'succeeded' AND compose_version IS NOT NULL`, pid)
	if err != nil {
		return nil, fmt.Errorf("store: deployed compose versions: %w", err)
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: deployed compose versions: %w", err)
		}
		out[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: deployed compose versions: %w", err)
	}
	return out, nil
}

// LatestDeployedComposeVersion returns the compose version of the project's
// most recent succeeded deploy, 0 when there is none. It returns ErrNotFound
// for an unknown slug.
func (s *Store) LatestDeployedComposeVersion(ctx context.Context, slug string) (int, error) {
	pid, err := projectID(ctx, s.db, slug)
	if err != nil {
		return 0, err
	}
	var v int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT compose_version FROM deploys
		WHERE project_id = ? AND status = 'succeeded' AND compose_version IS NOT NULL
		ORDER BY id DESC LIMIT 1), 0)`, pid).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: latest deployed compose version: %w", err)
	}
	return v, nil
}
