package store

import (
	"context"
	"errors"
	"testing"
)

func TestDeployedComposeVersions(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	p := sample("app", "w1")
	p.Source = "compose"
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got, err := s.DeployedComposeVersions(ctx, "app"); err != nil || len(got) != 0 {
		t.Fatalf("empty = %v %v", got, err)
	}
	if v, err := s.LatestDeployedComposeVersion(ctx, "app"); err != nil || v != 0 {
		t.Fatalf("latest empty = %d %v", v, err)
	}
	for _, d := range []*Deploy{
		{Project: "app", Status: "succeeded", ComposeVersion: 2},
		{Project: "app", Status: "failed", ComposeVersion: 3},
		{Project: "app", Status: "succeeded", ComposeVersion: 1},
		{Project: "app", Status: "succeeded"},
	} {
		if err := s.CreateDeploy(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.DeployedComposeVersions(ctx, "app")
	if err != nil || len(got) != 2 || !got[1] || !got[2] || got[3] {
		t.Fatalf("deployed = %v %v", got, err)
	}
	if v, err := s.LatestDeployedComposeVersion(ctx, "app"); err != nil || v != 1 {
		t.Fatalf("latest = %d %v", v, err)
	}
	if _, err := s.DeployedComposeVersions(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := s.LatestDeployedComposeVersion(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}
