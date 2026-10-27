package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrationFromV1(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx, sample("app", "w1")); err != nil {
		t.Fatal(err)
	}
	// Roll the database back to a v1 schema.
	for _, q := range []string{`DROP TABLE project_compose`, `ALTER TABLE projects DROP COLUMN source`, `DROP TABLE audit_log`, `DROP TABLE api_tokens`, `DROP TABLE sessions`, `DROP TABLE setup_tokens`, `DROP TABLE users`, `DROP TABLE exposure_domains`, `DROP TABLE project_exposures`, `DROP TABLE deploy_logs`, `DROP TABLE deploys`, `DROP TABLE project_env`, `PRAGMA user_version = 1`} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != len(migrations) || v < 3 {
		t.Fatalf("user_version %d err %v", v, err)
	}
	if p, err := s.GetProject(ctx, "app"); err != nil || p.Source != "git" {
		t.Fatalf("project %+v err %v", p, err)
	}
	if err := s.CreateDeploy(ctx, &Deploy{Project: "app", Status: "queued"}); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentWritesNeverBusy(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	if err := s.CreateProject(ctx, sample("app", "w1")); err != nil {
		t.Fatal(err)
	}
	d := &Deploy{Project: "app", Status: "running"}
	if err := s.CreateDeploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	const iters = 200
	workers := []func(i int) error{
		func(i int) error { return s.SetEnv(ctx, "app", map[string][]byte{"A": {byte(i)}, "B": {1}}) },
		func(i int) error { return s.UnsetEnv(ctx, "app", []string{"B"}) },
		func(i int) error { return s.AppendDeployLog(ctx, d.ID, int64(i+1), "agent", "x") },
		func(i int) error { c := *d; c.Stage = "up"; return s.UpdateDeploy(ctx, &c) },
		func(i int) error { _, err := s.ListEnv(ctx, "app"); return err },
		func(i int) error { _, err := s.DeployLogs(ctx, d.ID, 0); return err },
		func(i int) error { _, err := s.GetDeploy(ctx, d.ID); return err },
	}
	errc := make(chan error, len(workers))
	for _, fn := range workers {
		go func() {
			for i := 0; i < iters; i++ {
				if err := fn(i); err != nil {
					errc <- err
					return
				}
			}
			errc <- nil
		}()
	}
	for range workers {
		if err := <-errc; err != nil {
			t.Error(err)
		}
	}
}

func TestEnv(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	if err := s.CreateProject(ctx, sample("app", "w1")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListEnv(ctx, "app"); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	if err := s.SetEnv(ctx, "app", map[string][]byte{"B": {1, 0, 2}, "A": []byte("a"), "E": nil}); err != nil {
		t.Fatal(err)
	}
	first, err := s.ListEnv(ctx, "app")
	if err != nil || len(first) != 3 || first[0].Name != "A" || first[1].Name != "B" || first[2].Name != "E" {
		t.Fatalf("list: %+v %v", first, err)
	}
	if !bytes.Equal(first[1].ValueEnc, []byte{1, 0, 2}) || len(first[2].ValueEnc) != 0 ||
		first[0].UpdatedAt.IsZero() || first[0].UpdatedAt.Location() != time.UTC {
		t.Fatalf("values: %+v", first)
	}

	// Merge: overwrite one, add one, keep the rest.
	time.Sleep(2 * time.Millisecond)
	if err := s.SetEnv(ctx, "app", map[string][]byte{"A": []byte("new"), "C": []byte("c")}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListEnv(ctx, "app")
	if err != nil || len(got) != 4 {
		t.Fatalf("merge: %+v %v", got, err)
	}
	if string(got[0].ValueEnc) != "new" || !got[0].UpdatedAt.After(first[0].UpdatedAt) {
		t.Fatalf("A not updated: %+v", got[0])
	}
	if !got[1].UpdatedAt.Equal(first[1].UpdatedAt) {
		t.Fatalf("B touched: %+v", got[1])
	}

	if err := s.UnsetEnv(ctx, "app", []string{"A", "missing", "C"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListEnv(ctx, "app")
	if len(got) != 2 || got[0].Name != "B" || got[1].Name != "E" {
		t.Fatalf("unset: %+v", got)
	}

	// Env is per project.
	if err := s.CreateProject(ctx, sample("other", "w2")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListEnv(ctx, "other"); len(got) != 0 {
		t.Fatalf("leak: %+v", got)
	}

	if err := s.SetEnv(ctx, "nope", map[string][]byte{"A": nil}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set: %v", err)
	}
	if err := s.UnsetEnv(ctx, "nope", []string{"A"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unset: %v", err)
	}
	if _, err := s.ListEnv(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("list: %v", err)
	}
}

func TestDeployLifecycle(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	if err := s.CreateProject(ctx, sample("app", "w1")); err != nil {
		t.Fatal(err)
	}
	d := &Deploy{Project: "app", Ref: "refs/heads/main", Source: "api", Status: "queued"}
	if err := s.CreateDeploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	if d.ID == 0 || d.ProjectID == 0 || d.CreatedAt.IsZero() || d.CreatedAt.Location() != time.UTC {
		t.Fatalf("create: %+v", d)
	}
	got, err := s.GetDeploy(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Project != "app" || got.Ref != d.Ref || got.Status != "queued" || got.StartedAt != nil ||
		got.FinishedAt != nil || !got.CreatedAt.Equal(d.CreatedAt) {
		t.Fatalf("got %+v", got)
	}

	start := now()
	end := start.Add(time.Second)
	got.Status, got.Stage, got.Commit, got.StartedAt, got.FinishedAt = "failed", "policy", "abc", &start, &end
	got.ErrorReason, got.ErrorDetail, got.ErrorData = "policy_violation", "bad", `[{"rule":"x"}]`
	if err := s.UpdateDeploy(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, err := s.GetDeploy(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != "failed" || again.Stage != "policy" || again.Commit != "abc" ||
		again.ErrorReason != "policy_violation" || again.ErrorDetail != "bad" || again.ErrorData != `[{"rule":"x"}]` ||
		again.StartedAt == nil || !again.StartedAt.Equal(start) || again.StartedAt.Location() != time.UTC ||
		again.FinishedAt == nil || !again.FinishedAt.Equal(end) || !again.CreatedAt.Equal(d.CreatedAt) {
		t.Fatalf("update not persisted: %+v", again)
	}

	if err := s.CreateDeploy(ctx, &Deploy{Project: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("create unknown: %v", err)
	}
	if err := s.UpdateDeploy(ctx, &Deploy{ID: 999}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update unknown: %v", err)
	}
	if _, err := s.GetDeploy(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get unknown: %v", err)
	}
}

func TestListDeploysPagination(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	for _, slug := range []string{"app", "other"} {
		if err := s.CreateProject(ctx, sample(slug, "w"+slug)); err != nil {
			t.Fatal(err)
		}
	}
	var ids []int64
	for i := 0; i < 5; i++ {
		d := &Deploy{Project: "app", Status: "succeeded"}
		if err := s.CreateDeploy(ctx, d); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, d.ID)
		if err := s.CreateDeploy(ctx, &Deploy{Project: "other", Status: "succeeded"}); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name     string
		limit    int
		beforeID int64
		want     []int64
	}{
		{"all", 0, 0, []int64{ids[4], ids[3], ids[2], ids[1], ids[0]}},
		{"limit", 2, 0, []int64{ids[4], ids[3]}},
		{"page 2", 2, ids[3], []int64{ids[2], ids[1]}},
		{"last page", 2, ids[1], []int64{ids[0]}},
		{"past end", 2, ids[0], nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ListDeploys(ctx, "app", tt.limit, tt.beforeID)
			if err != nil {
				t.Fatal(err)
			}
			var gotIDs []int64
			for _, d := range got {
				if d.Project != "app" {
					t.Fatalf("foreign deploy %+v", d)
				}
				gotIDs = append(gotIDs, d.ID)
			}
			if len(gotIDs) != len(tt.want) {
				t.Fatalf("got %v want %v", gotIDs, tt.want)
			}
			for i := range gotIDs {
				if gotIDs[i] != tt.want[i] {
					t.Fatalf("got %v want %v", gotIDs, tt.want)
				}
			}
		})
	}
	if _, err := s.ListDeploys(ctx, "nope", 10, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown slug: %v", err)
	}
}

func TestDeployLogs(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	if err := s.CreateProject(ctx, sample("app", "w1")); err != nil {
		t.Fatal(err)
	}
	d := &Deploy{Project: "app", Status: "running"}
	if err := s.CreateDeploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	for i, l := range []struct{ stream, text string }{{"agent", "one"}, {"stdout", "two"}, {"stderr", "three"}} {
		if err := s.AppendDeployLog(ctx, d.ID, int64(i+1), l.stream, l.text); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		since int64
		want  []string
	}{
		{0, []string{"one", "two", "three"}},
		{1, []string{"two", "three"}},
		{3, nil},
	}
	for _, tt := range tests {
		got, err := s.DeployLogs(ctx, d.ID, tt.since)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(tt.want) {
			t.Fatalf("since %d: %+v", tt.since, got)
		}
		for i, l := range got {
			if l.Text != tt.want[i] || l.Seq != tt.since+int64(i)+1 {
				t.Fatalf("since %d: %+v", tt.since, got)
			}
		}
	}
	if got, _ := s.DeployLogs(ctx, d.ID, 0); got[1].Stream != "stdout" {
		t.Fatalf("stream: %+v", got)
	}
	if err := s.AppendDeployLog(ctx, d.ID, 2, "agent", "dup"); !errors.Is(err, ErrExists) {
		t.Fatalf("dup seq: %v", err)
	}
	if err := s.AppendDeployLog(ctx, 999, 1, "agent", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown deploy: %v", err)
	}
}

func TestPruneDeploys(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	for _, slug := range []string{"app", "other"} {
		if err := s.CreateProject(ctx, sample(slug, "w"+slug)); err != nil {
			t.Fatal(err)
		}
	}
	var ids []int64
	for _, st := range []string{"succeeded", "running", "failed", "succeeded", "failed"} {
		d := &Deploy{Project: "app", Status: st}
		if err := s.CreateDeploy(ctx, d); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, d.ID)
	}
	other := &Deploy{Project: "other", Status: "succeeded"}
	if err := s.CreateDeploy(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendDeployLog(ctx, ids[0], 1, "agent", "x"); err != nil {
		t.Fatal(err)
	}

	if n, err := s.PruneDeploys(ctx, "app", 2); err != nil || n != 2 {
		t.Fatalf("prune: %d %v", n, err)
	}
	got, _ := s.ListDeploys(ctx, "app", 0, 0)
	// Newest two kept, plus the running one that must survive.
	if len(got) != 3 || got[0].ID != ids[4] || got[1].ID != ids[3] || got[2].ID != ids[1] {
		t.Fatalf("remaining: %+v", got)
	}
	if logs, _ := s.DeployLogs(ctx, ids[0], 0); len(logs) != 0 {
		t.Fatalf("logs not cascaded: %+v", logs)
	}
	if _, err := s.GetDeploy(ctx, other.ID); err != nil {
		t.Fatalf("other project pruned: %v", err)
	}
	if n, err := s.PruneDeploys(ctx, "app", 0); err != nil || n != 0 {
		t.Fatalf("keep 0: %d %v", n, err)
	}
	if _, err := s.PruneDeploys(ctx, "nope", 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown slug: %v", err)
	}
}

func TestInterruptUnfinishedDeploys(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	if err := s.CreateProject(ctx, sample("app", "w1")); err != nil {
		t.Fatal(err)
	}
	byStatus := map[string]*Deploy{}
	for _, st := range []string{"queued", "running", "succeeded", "failed", "superseded"} {
		d := &Deploy{Project: "app", Status: st}
		if err := s.CreateDeploy(ctx, d); err != nil {
			t.Fatal(err)
		}
		byStatus[st] = d
	}
	n, err := s.InterruptUnfinishedDeploys(ctx)
	if err != nil || n != 2 {
		t.Fatalf("interrupt: %d %v", n, err)
	}
	for st, want := range map[string]string{"queued": "interrupted", "running": "interrupted",
		"succeeded": "succeeded", "failed": "failed", "superseded": "superseded"} {
		d, err := s.GetDeploy(ctx, byStatus[st].ID)
		if err != nil || d.Status != want {
			t.Fatalf("%s: %+v %v", st, d, err)
		}
		if (want == "interrupted") != (d.FinishedAt != nil) {
			t.Fatalf("%s finished_at: %+v", st, d.FinishedAt)
		}
	}
	if n, err := s.InterruptUnfinishedDeploys(ctx); err != nil || n != 0 {
		t.Fatalf("second run: %d %v", n, err)
	}
}

func TestProjectDeleteCascades(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	if err := s.CreateProject(ctx, sample("app", "w1")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnv(ctx, "app", map[string][]byte{"A": []byte("a")}); err != nil {
		t.Fatal(err)
	}
	d := &Deploy{Project: "app", Status: "succeeded"}
	if err := s.CreateDeploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendDeployLog(ctx, d.ID, 1, "agent", "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"project_env", "deploys", "deploy_logs"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: %d rows err %v", table, n, err)
		}
	}
}

func TestDeployRollbackOf(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	if err := s.CreateProject(ctx, sample("app", "w1")); err != nil {
		t.Fatal(err)
	}
	a := &Deploy{Project: "app", Status: "succeeded"}
	if err := s.CreateDeploy(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := &Deploy{Project: "app", Status: "queued", RollbackOf: a.ID}
	if err := s.CreateDeploy(ctx, b); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ id, want int64 }{{a.ID, 0}, {b.ID, a.ID}} {
		d, err := s.GetDeploy(ctx, c.id)
		if err != nil || d.RollbackOf != c.want {
			t.Fatalf("deploy %d = %+v, %v; want rollback_of %d", c.id, d, err, c.want)
		}
	}
	b.Status = "running"
	if err := s.UpdateDeploy(ctx, b); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDeploy(ctx, b.ID); d.RollbackOf != a.ID {
		t.Fatalf("rollback_of lost on update: %+v", d)
	}
}
