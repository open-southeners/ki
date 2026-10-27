package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestComposeVersions(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	p := sample("app", "w1")
	p.Source = "compose"
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetProject(ctx, "app")
	if got.Source != "compose" {
		t.Fatalf("source %q", got.Source)
	}
	if v, err := s.LatestComposeVersion(ctx, "app"); err != nil || v != 0 {
		t.Fatalf("latest %d %v", v, err)
	}
	if _, err := s.GetComposeVersion(ctx, "app", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty get: %v", err)
	}
	for i := 1; i <= 5; i++ {
		v, err := s.AddComposeVersion(ctx, "app", "c"+string(rune('0'+i)), "alice", "n")
		if err != nil || v != i {
			t.Fatalf("add %d: %d %v", i, v, err)
		}
	}
	latest, err := s.GetComposeVersion(ctx, "app", 0)
	if err != nil || latest.Version != 5 || latest.Content != "c5" || latest.CreatedBy != "alice" || latest.Note != "n" || latest.CreatedAt.IsZero() {
		t.Fatalf("latest %+v %v", latest, err)
	}
	if v, _ := s.GetComposeVersion(ctx, "app", 2); v == nil || v.Content != "c2" {
		t.Fatalf("v2 %+v", v)
	}
	if _, err := s.GetComposeVersion(ctx, "app", 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	l, err := s.ListComposeVersions(ctx, "app", 3)
	if err != nil || len(l) != 3 || l[0].Version != 5 || l[2].Version != 3 || l[0].Content != "" {
		t.Fatalf("list %+v %v", l, err)
	}
	n, err := s.PruneComposeVersions(ctx, "app", 2, 1)
	if err != nil || n != 2 {
		t.Fatalf("prune %d %v", n, err)
	}
	l, _ = s.ListComposeVersions(ctx, "app", 0)
	var vs []int
	for _, v := range l {
		vs = append(vs, v.Version)
	}
	if len(vs) != 3 || vs[0] != 5 || vs[1] != 4 || vs[2] != 1 {
		t.Fatalf("after prune %v", vs)
	}
	if v, _ := s.AddComposeVersion(ctx, "app", "c6", "", ""); v != 6 {
		t.Fatalf("next %d", v)
	}
	for _, f := range []func() error{
		func() error { _, err := s.AddComposeVersion(ctx, "nope", "x", "", ""); return err },
		func() error { _, err := s.LatestComposeVersion(ctx, "nope"); return err },
		func() error { _, err := s.GetComposeVersion(ctx, "nope", 0); return err },
		func() error { _, err := s.ListComposeVersions(ctx, "nope", 0); return err },
		func() error { _, err := s.PruneComposeVersions(ctx, "nope", 1, 0); return err },
	} {
		if err := f(); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown slug: %v", err)
		}
	}
}

func TestComposeVersionsConcurrent(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	if err := s.CreateProject(ctx, sample("app", "w1")); err != nil {
		t.Fatal(err)
	}
	const n = 30
	var mu sync.Mutex
	seen := map[int]bool{}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.AddComposeVersion(ctx, "app", "x", "", "")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[v] {
				t.Errorf("duplicate version %d", v)
			}
			seen[v] = true
		}()
	}
	wg.Wait()
	if v, _ := s.LatestComposeVersion(ctx, "app"); v != n || len(seen) != n {
		t.Fatalf("latest %d seen %d", v, len(seen))
	}
}

func TestComposeCascadeAndDeployVersion(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	if err := s.CreateProject(ctx, sample("app", "w1")); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetProject(ctx, "app"); p.Source != "git" {
		t.Fatalf("default source %q", p.Source)
	}
	if _, err := s.AddComposeVersion(ctx, "app", "x", "", ""); err != nil {
		t.Fatal(err)
	}
	d := &Deploy{Project: "app", Status: "queued", Ref: "compose@v1", ComposeVersion: 1}
	plain := &Deploy{Project: "app", Status: "queued"}
	for _, x := range []*Deploy{d, plain} {
		if err := s.CreateDeploy(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	if g, _ := s.GetDeploy(ctx, d.ID); g.ComposeVersion != 1 {
		t.Fatalf("compose version %d", g.ComposeVersion)
	}
	if g, _ := s.GetDeploy(ctx, plain.ID); g.ComposeVersion != 0 {
		t.Fatalf("plain compose version %d", g.ComposeVersion)
	}
	if err := s.DeleteProject(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM project_compose`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows left %d %v", n, err)
	}
}
