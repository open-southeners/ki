package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-southeners/ki-agent/internal/compose"
	"github.com/open-southeners/ki-agent/internal/policy"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/store"
)

const goodCompose = "services:\n  web:\n    image: registry.example/app:1\n"

// fakeValidator rejects content containing "build:" (a build violation) and
// "boom" (a plain compose error), and records every call.
type fakeValidator struct {
	calls []string
	err   error
}

func (f *fakeValidator) validate(_ context.Context, project, content string) (*compose.ValidationError, error) {
	f.calls = append(f.calls, project)
	if f.err != nil {
		return nil, f.err
	}
	if strings.Contains(content, "build:") {
		return &compose.ValidationError{Reason: "1 policy violation(s)", Violations: []policy.Violation{
			{Service: "web", Rule: policy.RuleBuildNotAllowed, Detail: "compose projects cannot build"},
		}}, nil
	}
	if strings.Contains(content, "boom") {
		return &compose.ValidationError{Reason: "services.web must be a mapping"}, nil
	}
	return nil, nil
}

func newComposeEnv(t testing.TB) (*env, *fakeValidator) {
	t.Helper()
	e := newEnv(t)
	fv := &fakeValidator{}
	e.svc.validateCompose = fv.validate
	return e, fv
}

func createCompose(t testing.TB, e *env, slug string) {
	t.Helper()
	if _, err := e.svc.Create(context.Background(), CreateParams{Slug: slug, Compose: goodCompose, CreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateCompose(t *testing.T) {
	ctx := context.Background()
	e, fv := newComposeEnv(t)
	res, err := e.svc.Create(ctx, CreateParams{Slug: "api", Compose: goodCompose, CreatedBy: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Project
	if res.WebhookSecret != "" || p.Source != SourceCompose || p.ComposeVersion != 1 || p.Status != StatusReady ||
		p.RepoURL != "" || p.Webhook.Path != "" || p.Auth.Method != "" {
		t.Errorf("result = %+v", res)
	}
	if len(fv.calls) != 1 || fv.calls[0] != "api" {
		t.Errorf("validator calls = %v", fv.calls)
	}
	fi, err := os.Stat(filepath.Join(e.data, "projects", "api"))
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o750 {
		t.Errorf("project dir = %v, %v", fi, err)
	}
	row, _ := e.st.GetProject(ctx, "api")
	if row.Source != SourceCompose || row.RepoURL != "" || row.WebhookID == "" {
		t.Errorf("row = %+v", row)
	}
	cv, err := e.svc.ComposeGet(ctx, "api", 0)
	if err != nil || cv.Version != 1 || cv.Content != goodCompose || cv.CreatedBy != "alice" {
		t.Errorf("v1 = %+v, %v", cv, err)
	}
	got, err := e.svc.Get(ctx, "api")
	if err != nil || got.Source != SourceCompose || got.ComposeVersion != 1 {
		t.Errorf("get = %+v, %v", got, err)
	}
	list, _ := e.svc.List(ctx)
	if len(list) != 1 || list[0].ComposeVersion != 1 {
		t.Errorf("list = %+v", list)
	}
	// Two compose projects both get their own webhook id.
	createCompose(t, e, "api2")
	if _, err := e.svc.Create(ctx, CreateParams{Slug: "api", Compose: goodCompose}); !errors.Is(err, store.ErrExists) {
		t.Errorf("duplicate err = %v", err)
	}
}

func TestCreateComposeGitProjectView(t *testing.T) {
	e := newEnv(t)
	e.seedProject(t, "app")
	got, err := e.svc.Get(context.Background(), "app")
	if err != nil || got.Source != SourceGit || got.ComposeVersion != 0 {
		t.Fatalf("git project = %+v, %v", got, err)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "compose_version") {
		t.Errorf("git view has compose_version: %s", b)
	}
}

func TestCreateComposeInvalid(t *testing.T) {
	ctx := context.Background()
	e, _ := newComposeEnv(t)
	tests := []struct {
		name string
		in   CreateParams
		want string
	}{
		{"both", CreateParams{Slug: "a", RepoURL: "https://example.com/a.git", Compose: goodCompose}, "mutually exclusive"},
		{"neither", CreateParams{Slug: "a"}, "required"},
		{"no slug", CreateParams{Compose: goodCompose}, "slug is required"},
		{"bad slug", CreateParams{Slug: "Bad_Slug", Compose: goodCompose}, "slug must match"},
		{"git field", CreateParams{Slug: "a", Compose: goodCompose, Branch: "main"}, "only to git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.svc.Create(ctx, tt.in)
			var inv *InvalidError
			if !errors.As(err, &inv) || !strings.Contains(inv.Msg, tt.want) {
				t.Fatalf("err = %v, want invalid containing %q", err, tt.want)
			}
		})
	}
	// Rejected content stores nothing.
	_, err := e.svc.Create(ctx, CreateParams{Slug: "a", Compose: "services:\n  web:\n    build: .\n"})
	var ve *compose.ValidationError
	if !errors.As(err, &ve) || len(ve.Violations) != 1 || ve.Violations[0].Rule != policy.RuleBuildNotAllowed {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.st.GetProject(ctx, "a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("project stored after rejection: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.data, "projects", "a")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("project dir left behind: %v", err)
	}
}

func TestComposeValidatorMissing(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Create(context.Background(), CreateParams{Slug: "a", Compose: goodCompose}); err == nil {
		t.Fatal("create without a validator must fail closed")
	}
}

func TestComposeSetGetHistory(t *testing.T) {
	ctx := context.Background()
	e, _ := newComposeEnv(t)
	createCompose(t, e, "api")

	v2 := strings.Replace(goodCompose, ":1", ":2", 1)
	v, err := e.svc.ComposeSet(ctx, "api", v2, "bump", "bob")
	if err != nil || v != 2 {
		t.Fatalf("set = %d, %v", v, err)
	}
	if _, err := e.svc.ComposeSet(ctx, "api", "services:\n  web:\n    build: .\n", "", "bob"); err == nil {
		t.Fatal("build content accepted")
	}
	if n, _ := e.st.LatestComposeVersion(ctx, "api"); n != 2 {
		t.Errorf("rejected set stored a version: latest %d", n)
	}
	cv, err := e.svc.ComposeGet(ctx, "api", 0)
	if err != nil || cv.Version != 2 || cv.Content != v2 || cv.Note != "bump" || cv.CreatedBy != "bob" || cv.CreatedAt == "" {
		t.Errorf("latest = %+v, %v", cv, err)
	}
	if cv, err := e.svc.ComposeGet(ctx, "api", 1); err != nil || cv.Content != goodCompose {
		t.Errorf("v1 = %+v, %v", cv, err)
	}
	if _, err := e.svc.ComposeGet(ctx, "api", 9); !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("v9 err = %v", err)
	}
	if _, err := e.svc.ComposeGet(ctx, "nope", 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown err = %v", err)
	}

	// Mark v1 as deployed.
	if err := e.st.CreateDeploy(ctx, &store.Deploy{Project: "api", Status: "succeeded", ComposeVersion: 1}); err != nil {
		t.Fatal(err)
	}
	hist, err := e.svc.ComposeHistory(ctx, "api", 0)
	if err != nil || len(hist) != 2 {
		t.Fatalf("history = %+v, %v", hist, err)
	}
	if hist[0].Version != 2 || hist[0].Deployed || hist[0].CreatedBy != "bob" || hist[0].Note != "bump" ||
		hist[1].Version != 1 || !hist[1].Deployed {
		t.Errorf("history = %+v", hist)
	}
	if hist, _ := e.svc.ComposeHistory(ctx, "api", 1); len(hist) != 1 || hist[0].Version != 2 {
		t.Errorf("limited history = %+v", hist)
	}
}

func TestComposeSetPrunesButProtectsDeployed(t *testing.T) {
	ctx := context.Background()
	e, _ := newComposeEnv(t)
	createCompose(t, e, "api")
	if err := e.st.CreateDeploy(ctx, &store.Deploy{Project: "api", Status: "succeeded", ComposeVersion: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= KeepComposeVersions+5; i++ {
		if _, err := e.svc.ComposeSet(ctx, "api", fmt.Sprintf("%s# %d\n", goodCompose, i), "", "bob"); err != nil {
			t.Fatal(err)
		}
	}
	hist, err := e.svc.ComposeHistory(ctx, "api", 0)
	if err != nil {
		t.Fatal(err)
	}
	// The newest 50 plus the deployed v1.
	if len(hist) != KeepComposeVersions+1 || hist[0].Version != KeepComposeVersions+5 || hist[len(hist)-1].Version != 1 {
		t.Fatalf("kept %d versions, newest %d, oldest %d", len(hist), hist[0].Version, hist[len(hist)-1].Version)
	}
}

func TestComposeOnGitProject(t *testing.T) {
	ctx := context.Background()
	e, _ := newComposeEnv(t)
	e.seedProject(t, "app")
	var inv *InvalidError
	if _, err := e.svc.ComposeGet(ctx, "app", 0); !errors.As(err, &inv) {
		t.Errorf("get err = %v", err)
	}
	if _, err := e.svc.ComposeSet(ctx, "app", goodCompose, "", "x"); !errors.As(err, &inv) {
		t.Errorf("set err = %v", err)
	}
	if _, err := e.svc.ComposeHistory(ctx, "app", 0); !errors.As(err, &inv) {
		t.Errorf("history err = %v", err)
	}
}

func TestComposeProjectGuards(t *testing.T) {
	ctx := context.Background()
	e, _ := newComposeEnv(t)
	createCompose(t, e, "api")
	var inv *InvalidError
	if _, err := e.svc.Update(ctx, UpdateParams{Slug: "api", RotateWebhookSecret: true}); !errors.As(err, &inv) || inv.Msg != "not a git project" {
		t.Errorf("update err = %v", err)
	}
	if _, err := e.svc.Verify(ctx, "api"); !errors.As(err, &inv) || inv.Msg != "not a git project" {
		t.Errorf("verify err = %v", err)
	}
	row, _ := e.st.GetProject(ctx, "api")
	if _, err := e.svc.WebhookHook(ctx, row.WebhookID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("webhook err = %v", err)
	}
	// Delete works like for git projects and removes the project dir.
	if _, err := e.svc.Delete(ctx, "api", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.data, "projects", "api")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("project dir remains: %v", err)
	}
}

func TestComposeHandlers(t *testing.T) {
	ctx := context.Background()
	e, _ := newComposeEnv(t)
	h := NewHandlers(e.svc)
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	marshal := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	var re *rpc.Error

	res, err := h.create(ctx, nil, raw(map[string]any{"slug": "api", "compose": goodCompose}))
	if err != nil || strings.Contains(marshal(res), "webhook_secret") {
		t.Fatalf("create = %s, %v", marshal(res), err)
	}
	if _, err := h.create(ctx, nil, raw(map[string]any{})); !errors.As(err, &re) || re.Code != rpc.CodeInvalidParams {
		t.Errorf("empty create err = %v", err)
	}
	if _, err := h.create(ctx, nil, raw(map[string]any{"slug": "x", "repo_url": "https://e.com/x.git", "compose": goodCompose})); !errors.As(err, &re) || re.Code != rpc.CodeInvalidParams {
		t.Errorf("both err = %v", err)
	}

	// A rejected file is invalid params carrying the violations.
	_, err = h.composeSet(ctx, nil, raw(map[string]any{"slug": "api", "compose": "services:\n  web:\n    build: .\n"}))
	if !errors.As(err, &re) || re.Code != rpc.CodeInvalidParams {
		t.Fatalf("build err = %v", err)
	}
	var data struct {
		Reason     string             `json:"reason"`
		Violations []policy.Violation `json:"violations"`
	}
	if json.Unmarshal(re.Data, &data) != nil || data.Reason != "policy_violation" ||
		len(data.Violations) != 1 || data.Violations[0].Rule != "build_not_allowed" {
		t.Errorf("data = %s", re.Data)
	}
	_, err = h.composeSet(ctx, nil, raw(map[string]any{"slug": "api", "compose": "boom"}))
	if !errors.As(err, &re) || !strings.Contains(string(re.Data), "compose_invalid") {
		t.Errorf("boom err = %v data %s", err, re.Data)
	}

	got, err := h.composeSet(ctx, nil, raw(map[string]any{"slug": "api", "compose": goodCompose + "# v2\n", "note": "n"}))
	if err != nil || marshal(got) != `{"version":2}` {
		t.Fatalf("set = %s, %v", marshal(got), err)
	}
	if _, err := h.composeSet(ctx, nil, raw(map[string]any{"slug": "api"})); !errors.As(err, &re) || re.Code != rpc.CodeInvalidParams {
		t.Errorf("missing compose err = %v", err)
	}
	g, err := h.composeGet(ctx, nil, raw(map[string]any{"slug": "api", "version": 1}))
	if err != nil || !strings.Contains(marshal(g), `"version":1`) {
		t.Errorf("get = %s, %v", marshal(g), err)
	}
	if _, err := h.composeGet(ctx, nil, raw(map[string]any{"slug": "api", "version": 7})); !errors.As(err, &re) || re.Code != rpc.CodeNotFound {
		t.Errorf("missing version err = %v", err)
	}
	if _, err := h.composeGet(ctx, nil, raw(map[string]any{"slug": "nope"})); !errors.As(err, &re) || re.Code != rpc.CodeNotFound {
		t.Errorf("unknown err = %v", err)
	}
	hs, err := h.composeHistory(ctx, nil, raw(map[string]any{"slug": "api"}))
	if err != nil || !strings.Contains(marshal(hs), `"deployed":false`) {
		t.Errorf("history = %s, %v", marshal(hs), err)
	}
}

func TestComposeSetDeployHook(t *testing.T) {
	ctx := context.Background()
	e, _ := newComposeEnv(t)
	h := NewHandlers(e.svc)
	createCompose(t, e, "api")
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

	// deploy:true without a hook is an error, and nothing is stored.
	if _, err := h.composeSet(ctx, nil, raw(map[string]any{"slug": "api", "compose": goodCompose + "# 2\n", "deploy": true})); err == nil {
		t.Fatal("deploy without hook succeeded")
	}
	if n, _ := e.st.LatestComposeVersion(ctx, "api"); n != 1 {
		t.Fatalf("latest = %d, want 1", n)
	}

	type call struct {
		slug    string
		version int
	}
	var calls []call
	e.svc.SetOnComposeSet(func(_ context.Context, slug string, version int) (any, error) {
		calls = append(calls, call{slug, version})
		return map[string]any{"id": 7}, nil
	})
	got, err := h.composeSet(ctx, nil, raw(map[string]any{"slug": "api", "compose": goodCompose + "# 2\n", "deploy": true}))
	b, _ := json.Marshal(got)
	if err != nil || string(b) != `{"version":2,"deploy":{"id":7}}` {
		t.Fatalf("set = %s, %v", b, err)
	}
	if len(calls) != 1 || calls[0] != (call{"api", 2}) {
		t.Errorf("hook calls = %v", calls)
	}
	// Without deploy the hook is not called.
	if _, err := h.composeSet(ctx, nil, raw(map[string]any{"slug": "api", "compose": goodCompose + "# 3\n"})); err != nil || len(calls) != 1 {
		t.Errorf("hook called without deploy: %v %v", calls, err)
	}
	// A hook failure is returned; the version stays stored.
	e.svc.SetOnComposeSet(func(context.Context, string, int) (any, error) { return nil, errors.New("busy") })
	if _, err := h.composeSet(ctx, nil, raw(map[string]any{"slug": "api", "compose": goodCompose + "# 4\n", "deploy": true})); err == nil || err.Error() != "busy" {
		t.Errorf("hook err = %v", err)
	}
	if n, _ := e.st.LatestComposeVersion(ctx, "api"); n != 4 {
		t.Errorf("latest = %d, want 4", n)
	}
}
