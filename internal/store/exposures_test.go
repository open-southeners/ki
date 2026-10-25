package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func exp(service string, port int, domains ...string) Exposure {
	return Exposure{Service: service, Port: port, Domains: domains}
}

func newExposureStore(t *testing.T, slugs ...string) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, slug := range slugs {
		if err := s.CreateProject(context.Background(), sample(slug, "w-"+slug)); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestExposureSetListRemove(t *testing.T) {
	ctx := context.Background()
	s := newExposureStore(t, "app")
	e := exp("web", 8080, "a.example.com", "b.example.com")
	e.Path = "/x"
	if err := s.SetProjectExposure(ctx, "app", e); err != nil {
		t.Fatal(err)
	}
	// Replacing releases the old domains.
	if err := s.SetProjectExposure(ctx, "app", exp("web", 9090, "c.example.com")); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListExposures(ctx, "app")
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	if got[0].Port != 9090 || got[0].Source != "project" || !reflect.DeepEqual(got[0].Domains, []string{"c.example.com"}) || got[0].UpdatedAt.IsZero() || got[0].UpdatedAt.Location().String() != "UTC" {
		t.Fatalf("%+v", got[0])
	}
	if _, _, err := s.DomainOwner(ctx, "a.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if p, svc, err := s.DomainOwner(ctx, "c.example.com"); err != nil || p != "app" || svc != "web" {
		t.Fatal(p, svc, err)
	}
	if err := s.RemoveProjectExposure(ctx, "app", "web"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveProjectExposure(ctx, "app", "web"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListExposures(ctx, "app"); len(got) != 0 {
		t.Fatal(got)
	}
	if _, _, err := s.DomainOwner(ctx, "c.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.SetProjectExposure(ctx, "nope", e); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.ListExposures(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestExposureConflicts(t *testing.T) {
	ctx := context.Background()
	s := newExposureStore(t, "a", "b")
	if err := s.SetProjectExposure(ctx, "a", exp("web", 80, "x.example.com")); err != nil {
		t.Fatal(err)
	}
	var ce *DomainConflictError
	err := s.SetProjectExposure(ctx, "b", exp("web", 80, "ok.example.com", "x.example.com"))
	if !errors.As(err, &ce) || ce.Domain != "x.example.com" || ce.Project != "a" || ce.Service != "web" {
		t.Fatalf("%v", err)
	}
	if _, _, err := s.DomainOwner(ctx, "ok.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatal("conflicting set must change nothing")
	}
	// Same project, other service also conflicts.
	if err := s.SetProjectExposure(ctx, "a", exp("api", 80, "x.example.com")); !errors.As(err, &ce) || ce.Service != "web" {
		t.Fatalf("%v", err)
	}
	// Label replace conflicts atomically: previous labels survive.
	if err := s.ReplaceLabelExposures(ctx, "b", []Exposure{exp("w", 1, "l.example.com")}); err != nil {
		t.Fatal(err)
	}
	err = s.ReplaceLabelExposures(ctx, "b", []Exposure{exp("w", 1, "n.example.com"), exp("z", 2, "x.example.com")})
	if !errors.As(err, &ce) {
		t.Fatalf("%v", err)
	}
	got, _ := s.ListExposures(ctx, "b")
	if len(got) != 1 || got[0].Domains[0] != "l.example.com" {
		t.Fatalf("%+v", got)
	}
	if p, _, err := s.DomainOwner(ctx, "l.example.com"); err != nil || p != "b" {
		t.Fatal(p, err)
	}
	if err := s.ReplaceLabelExposures(ctx, "b", []Exposure{exp("w", 1), exp("w", 2)}); !errors.Is(err, ErrExists) {
		t.Fatalf("%v", err)
	}
}

func TestExposureOverride(t *testing.T) {
	ctx := context.Background()
	s := newExposureStore(t, "a", "b")
	if err := s.ReplaceLabelExposures(ctx, "a", []Exposure{exp("web", 80, "x.example.com", "y.example.com"), exp("db", 5, "d.example.com")}); err != nil {
		t.Fatal(err)
	}
	// A project entry for the same service naming the same domain is allowed and wins.
	if err := s.SetProjectExposure(ctx, "a", exp("web", 81, "x.example.com")); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListExposures(ctx, "a")
	if len(got) != 2 || got[0].Service != "db" || got[1].Service != "web" || got[1].Port != 81 || got[1].Source != "project" {
		t.Fatalf("%+v", got)
	}
	// The overridden label's other domain is released.
	if err := s.SetProjectExposure(ctx, "b", exp("web", 80, "y.example.com")); err != nil {
		t.Fatal(err)
	}
	// A label replace while overridden registers nothing for web and cannot conflict.
	if err := s.ReplaceLabelExposures(ctx, "a", []Exposure{exp("web", 80, "x.example.com", "y.example.com")}); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := s.DomainOwner(ctx, "y.example.com"); p != "b" {
		t.Fatal(p)
	}
	all, err := s.AllExposures(ctx)
	if err != nil || len(all) != 2 || all[0].Project != "a" || all[0].Port != 81 || all[1].Project != "b" {
		t.Fatalf("%+v %v", all, err)
	}
	// Removing the project entry re-registers the label domains that are free.
	if err := s.RemoveProjectExposure(ctx, "a", "web"); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := s.DomainOwner(ctx, "x.example.com"); p != "a" {
		t.Fatal(p)
	}
	if p, _, _ := s.DomainOwner(ctx, "y.example.com"); p != "b" {
		t.Fatal(p)
	}
	got, _ = s.ListExposures(ctx, "a")
	if len(got) != 1 || got[0].Source != "labels" || got[0].Port != 80 {
		t.Fatalf("%+v", got)
	}
}

func TestExposureCascade(t *testing.T) {
	ctx := context.Background()
	s := newExposureStore(t, "a")
	if err := s.SetProjectExposure(ctx, "a", exp("web", 80, "x.example.com")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DomainOwner(ctx, "x.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if all, _ := s.AllExposures(ctx); len(all) != 0 {
		t.Fatal(all)
	}
}
