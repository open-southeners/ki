package projects

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/open-southeners/ki-agent/internal/store"
)

// seedProject inserts a project row without touching git.
func (e *env) seedProject(t testing.TB, slug string) {
	t.Helper()
	p := &store.Project{
		Slug: slug, RepoURL: "https://example.com/" + slug + ".git", Branch: "main",
		AuthMethod: AuthNone, WebhookID: "wh-" + slug, Status: StatusReady, ComposeFile: "compose.yaml",
		WebhookSecretEnc: e.box.Seal([]byte("topsecret"), aad(slug, "webhook_secret")),
	}
	if err := e.st.CreateProject(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func TestSetEnvValidation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedProject(t, "app")
	tests := []struct {
		name string
		vars map[string]string
	}{
		{"empty", map[string]string{}},
		{"bad name digit", map[string]string{"1A": "x"}},
		{"bad name dash", map[string]string{"A-B": "x"}},
		{"bad name empty", map[string]string{"": "x"}},
		{"nul", map[string]string{"A": "a\x00b"}},
		{"too large", map[string]string{"A": strings.Repeat("x", MaxEnvValue+1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.svc.SetEnv(ctx, "app", tt.vars)
			wantInvalid(t, err)
		})
	}
	if _, err := e.svc.SetEnv(ctx, "app", map[string]string{"OK": strings.Repeat("x", MaxEnvValue)}); err != nil {
		t.Errorf("max-size value rejected: %v", err)
	}
	if _, err := e.svc.SetEnv(ctx, "nope", map[string]string{"A": "1"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown slug err = %v", err)
	}
}

func TestEnvRoundTripAndMerge(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedProject(t, "app")
	names, err := e.svc.SetEnv(ctx, "app", map[string]string{"B": "2", "A": "1"})
	if err != nil || !reflect.DeepEqual(names, []string{"A", "B"}) {
		t.Fatalf("names = %v, err = %v", names, err)
	}
	if names, err = e.svc.SetEnv(ctx, "app", map[string]string{"B": "two", "C": "3"}); err != nil ||
		!reflect.DeepEqual(names, []string{"A", "B", "C"}) {
		t.Fatalf("names = %v, err = %v", names, err)
	}
	got, err := e.svc.Env(ctx, "app")
	if err != nil || !reflect.DeepEqual(got, map[string]string{"A": "1", "B": "two", "C": "3"}) {
		t.Fatalf("env = %v, err = %v", got, err)
	}

	// Sealed with the per-name aad.
	raw, _ := e.st.ListEnv(ctx, "app")
	if string(raw[0].ValueEnc) == "1" {
		t.Error("value stored in clear")
	}
	if _, err := e.box.Open(raw[0].ValueEnc, aad("app", "env/A")); err != nil {
		t.Errorf("aad scheme: %v", err)
	}

	list, err := e.svc.ListEnv(ctx, "app")
	if err != nil || len(list) != 3 || list[0].Name != "A" || list[0].UpdatedAt == "" {
		t.Fatalf("list = %+v, err = %v", list, err)
	}

	names, err = e.svc.UnsetEnv(ctx, "app", []string{"A", "ZZ"})
	if err != nil || !reflect.DeepEqual(names, []string{"B", "C"}) {
		t.Fatalf("unset names = %v, err = %v", names, err)
	}
	_, err = e.svc.UnsetEnv(ctx, "app", nil)
	wantInvalid(t, err)
	if _, err := e.svc.ListEnv(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("list unknown err = %v", err)
	}
}

func TestSetEnvLimit(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedProject(t, "app")
	all := map[string]string{}
	for i := 0; i < MaxEnvVars; i++ {
		all["V"+strconv.Itoa(i)] = "x"
	}
	if _, err := e.svc.SetEnv(ctx, "app", all); err != nil {
		t.Fatal(err)
	}
	// Overwriting an existing name stays within the limit; a new one does not.
	if _, err := e.svc.SetEnv(ctx, "app", map[string]string{"V0": "y"}); err != nil {
		t.Errorf("overwrite at limit: %v", err)
	}
	_, err := e.svc.SetEnv(ctx, "app", map[string]string{"EXTRA": "y"})
	wantInvalid(t, err)
	if l, _ := e.svc.ListEnv(ctx, "app"); len(l) != MaxEnvVars {
		t.Errorf("len = %d", len(l))
	}
}

func TestWithProjectAndSetCommit(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedProject(t, "app")
	err := e.svc.WithProject(ctx, "app", func(p *store.Project, dir string) error {
		if p.Slug != "app" || dir != e.svc.checkoutDir("app") {
			t.Errorf("p = %v dir = %s", p.Slug, dir)
		}
		if _, err := e.svc.Env(ctx, "app"); err != nil { // must not deadlock
			t.Error(err)
		}
		return e.svc.SetCommitLocked(ctx, p, "abc")
	})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := e.st.GetProject(ctx, "app"); p.Commit != "abc" {
		t.Errorf("commit = %q", p.Commit)
	}
	if err := e.svc.SetCommit(ctx, "app", "def"); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.st.GetProject(ctx, "app"); p.Commit != "def" {
		t.Errorf("commit = %q", p.Commit)
	}
	if err := e.svc.WithProject(ctx, "nope", func(*store.Project, string) error { return nil }); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	if _, err := e.svc.Auth(&store.Project{AuthMethod: AuthNone}); err != nil {
		t.Error(err)
	}
}

func TestWebhookHook(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedProject(t, "app")
	h, err := e.svc.WebhookHook(ctx, "wh-app")
	if err != nil {
		t.Fatal(err)
	}
	if h.Slug != "app" || h.Branch != "main" || h.Status != StatusReady || string(h.Secret) != "topsecret" {
		t.Errorf("hook = %+v", h)
	}
	if _, err := e.svc.WebhookHook(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestOnDeleteRunsBeforeLock(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedProject(t, "app")
	var calls atomic.Int32
	e.svc.OnDelete(func(ctx context.Context, slug string) {
		calls.Add(1)
		// The lock must be free: taking it here would deadlock otherwise.
		if err := e.svc.WithProject(ctx, slug, func(*store.Project, string) error { return nil }); err != nil {
			t.Error(err)
		}
	})
	e.svc.OnDelete(func(context.Context, string) { calls.Add(1) })
	if _, err := e.svc.Delete(ctx, "nope", false); !errors.Is(err, store.ErrNotFound) || calls.Load() != 0 {
		t.Fatalf("unknown: err = %v calls = %d", err, calls.Load())
	}
	if _, err := e.svc.Delete(ctx, "app", false); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("hook calls = %d", calls.Load())
	}
}

func TestOnDeletedRunsAfterDelete(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedProject(t, "app")
	var calls atomic.Int32
	e.svc.OnDeleted(func(ctx context.Context, slug string) {
		calls.Add(1)
		if _, err := e.svc.Get(ctx, slug); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("project still present in OnDeleted: %v", err)
		}
	})
	if _, err := e.svc.Delete(ctx, "nope", false); err == nil || calls.Load() != 0 {
		t.Fatalf("unknown: err = %v calls = %d", err, calls.Load())
	}
	if _, err := e.svc.Delete(ctx, "app", false); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Errorf("hook calls = %d", calls.Load())
	}
}
