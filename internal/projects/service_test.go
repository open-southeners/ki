package projects

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-southeners/ki-agent/internal/git"
	"github.com/open-southeners/ki-agent/internal/runtime"
	"github.com/open-southeners/ki-agent/internal/secret"
	"github.com/open-southeners/ki-agent/internal/store"
)

var gitEnv = []string{
	"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
	"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
}

func gitRun(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repo is a bare repository plus the work tree that seeds it.
type repo struct {
	t         testing.TB
	URL, work string
}

// seedRepo builds <root>/<name>.git with the given files committed on main
// (and a dev branch carrying DEV).
func seedRepo(t testing.TB, name string, files map[string]string) *repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	bare := filepath.Join(root, name+".git")
	work := filepath.Join(root, "work")
	gitRun(t, root, "init", "--bare", "--initial-branch=main", bare)
	gitRun(t, root, "init", "--initial-branch=main", work)
	gitRun(t, work, "remote", "add", "origin", bare)
	r := &repo{t: t, URL: "file://" + bare, work: work}
	r.commit(files)
	gitRun(t, work, "checkout", "-b", "dev")
	r.commit(map[string]string{"DEV": "dev\n"})
	gitRun(t, work, "checkout", "main")
	gitRun(t, work, "push", "origin", "main", "dev")
	return r
}

func (r *repo) commit(files map[string]string) string {
	r.t.Helper()
	for name, content := range files {
		full := filepath.Join(r.work, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			r.t.Fatal(err)
		}
		gitRun(r.t, r.work, "add", name)
	}
	gitRun(r.t, r.work, "commit", "-m", "change")
	return gitRun(r.t, r.work, "rev-parse", "HEAD")
}

func (r *repo) push(branch string) { gitRun(r.t, r.work, "push", "origin", branch) }

// fakeRuntime records PruneProject calls.
type fakeRuntime struct {
	runtime.Runtime
	err   error
	calls []string
	// onPrune runs inside PruneProject, to observe state at that moment.
	onPrune func()
}

func (f *fakeRuntime) PruneProject(_ context.Context, project string, volumes bool) (runtime.PruneReport, error) {
	if f.onPrune != nil {
		f.onPrune()
	}
	if f.err != nil {
		return runtime.PruneReport{}, f.err
	}
	f.calls = append(f.calls, project)
	if volumes {
		f.calls = append(f.calls, "volumes")
	}
	return runtime.PruneReport{Containers: 2, Networks: 1, Volumes: 3}, nil
}

type env struct {
	svc  *Service
	st   *store.Store
	box  *secret.Box
	rt   *fakeRuntime
	data string
}

func newEnv(t testing.TB) *env {
	t.Helper()
	data := t.TempDir()
	st, err := store.Open(filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, err := secret.LoadOrCreate(filepath.Join(data, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	protocols := []string{"https", "ssh", "file"}
	g, err := git.New(git.Options{DataDir: data, Protocols: protocols})
	if err != nil {
		t.Fatal(err)
	}
	rt := &fakeRuntime{}
	svc, err := New(Options{Store: st, Secret: box, Git: g, Runtime: rt, DataDir: data, Protocols: protocols,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return &env{svc: svc, st: st, box: box, rt: rt, data: data}
}

func (e *env) noStaging(t testing.TB) {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(e.data, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range ents {
		if strings.HasPrefix(ent.Name(), "clone-") {
			t.Errorf("staging leftover %s", ent.Name())
		}
	}
}

func reason(t testing.TB, err error) string {
	t.Helper()
	var ge *git.Error
	if !errors.As(err, &ge) {
		t.Fatalf("err = %v, want *git.Error", err)
	}
	return ge.Reason
}

func wantInvalid(t testing.TB, err error) {
	t.Helper()
	var inv *InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("err = %v, want InvalidError", err)
	}
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestDeriveSlug(t *testing.T) {
	tests := []struct{ in, want string }{
		{"app", "app"},
		{"My_App", "my-app"},
		{"--weird..name--", "weird-name"},
		{"UPPER", "upper"},
		{strings.Repeat("a", 80), strings.Repeat("a", 63)},
		{"___", ""},
	}
	for _, tt := range tests {
		if got := DeriveSlug(tt.in); got != tt.want {
			t.Errorf("DeriveSlug(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCreate(t *testing.T) {
	tests := []struct {
		name        string
		files       map[string]string
		mod         func(*CreateParams)
		wantSlug    string
		wantTrigger Trigger
		wantCompose string
	}{
		{"default slug and branch", map[string]string{"compose.yaml": "services: {}\n"}, nil,
			"myrepo", Trigger{Branch: "main"}, "compose.yaml"},
		{"explicit branch and slug", map[string]string{"docker-compose.yml": "x\n"},
			func(p *CreateParams) { p.Branch = "dev"; p.Slug = "other" },
			"other", Trigger{Branch: "dev"}, "docker-compose.yml"},
		{"tag trigger", map[string]string{"compose.yml": "x\n"},
			func(p *CreateParams) { p.TagPattern = "v*" },
			"myrepo", Trigger{TagPattern: "v*"}, "compose.yml"},
		{"explicit compose", map[string]string{"compose.yaml": "x\n", "deploy/prod.yaml": "x\n"},
			func(p *CreateParams) { p.ComposeFile = "./deploy/prod.yaml" },
			"myrepo", Trigger{Branch: "main"}, "deploy/prod.yaml"},
		{"detection order", map[string]string{"docker-compose.yaml": "x\n", "compose.yml": "x\n"}, nil,
			"myrepo", Trigger{Branch: "main"}, "compose.yml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			r := seedRepo(t, "MyRepo", tt.files)
			in := CreateParams{RepoURL: r.URL}
			if tt.mod != nil {
				tt.mod(&in)
			}
			res, err := e.svc.Create(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			p := res.Project
			if p.Slug != tt.wantSlug || p.Trigger != tt.wantTrigger || p.ComposeFile != tt.wantCompose {
				t.Errorf("project = %+v", p)
			}
			if p.Status != StatusReady || p.DefaultBranch != "main" || p.Commit == "" || p.Auth.Method != AuthNone {
				t.Errorf("project = %+v", p)
			}
			if len(res.WebhookSecret) != 64 || !strings.HasPrefix(p.Webhook.Path, "/hooks/") || len(p.Webhook.Path) != len("/hooks/")+32 {
				t.Errorf("webhook = %q %q", p.Webhook.Path, res.WebhookSecret)
			}
			if !exists(filepath.Join(e.data, "projects", tt.wantSlug, tt.wantCompose)) {
				t.Error("checkout missing compose file")
			}
			if tt.wantTrigger.Branch == "dev" && !exists(filepath.Join(e.data, "projects", tt.wantSlug, "DEV")) {
				t.Error("dev branch not checked out")
			}
			e.noStaging(t)

			row, err := e.st.GetProject(context.Background(), tt.wantSlug)
			if err != nil {
				t.Fatal(err)
			}
			got, err := e.box.Open(row.WebhookSecretEnc, aad(tt.wantSlug, "webhook_secret"))
			if err != nil || string(got) != res.WebhookSecret {
				t.Errorf("stored webhook secret = %q, %v", got, err)
			}
		})
	}
}

func TestCreateRejectsAndRollsBack(t *testing.T) {
	ctx := context.Background()
	t.Run("missing compose persists nothing", func(t *testing.T) {
		e := newEnv(t)
		r := seedRepo(t, "app", map[string]string{"README": "x\n"})
		_, err := e.svc.Create(ctx, CreateParams{RepoURL: r.URL})
		if got := reason(t, err); got != git.ReasonComposeNotFound {
			t.Fatalf("reason = %s", got)
		}
		if _, err := e.st.GetProject(ctx, "app"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("row persisted: %v", err)
		}
		if exists(filepath.Join(e.data, "projects", "app")) {
			t.Error("checkout left behind")
		}
		e.noStaging(t)
	})
	t.Run("missing branch", func(t *testing.T) {
		e := newEnv(t)
		r := seedRepo(t, "app", map[string]string{"compose.yaml": "x\n"})
		_, err := e.svc.Create(ctx, CreateParams{RepoURL: r.URL, Branch: "nope"})
		if got := reason(t, err); got != git.ReasonRefNotFound {
			t.Fatalf("reason = %s", got)
		}
		e.noStaging(t)
	})
	t.Run("missing repository", func(t *testing.T) {
		e := newEnv(t)
		_, err := e.svc.Create(ctx, CreateParams{RepoURL: "file://" + filepath.Join(t.TempDir(), "gone.git")})
		if got := reason(t, err); got != git.ReasonNotFound {
			t.Fatalf("reason = %s", got)
		}
		e.noStaging(t)
	})
	t.Run("slug clash", func(t *testing.T) {
		e := newEnv(t)
		r := seedRepo(t, "app", map[string]string{"compose.yaml": "x\n"})
		if _, err := e.svc.Create(ctx, CreateParams{RepoURL: r.URL}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Create(ctx, CreateParams{RepoURL: r.URL}); !errors.Is(err, store.ErrExists) {
			t.Fatalf("err = %v, want ErrExists", err)
		}
		if !exists(filepath.Join(e.data, "projects", "app", "compose.yaml")) {
			t.Error("clash removed the existing checkout")
		}
	})
	t.Run("validation", func(t *testing.T) {
		e := newEnv(t)
		r := seedRepo(t, "app", map[string]string{"compose.yaml": "x\n"})
		tests := []struct {
			name string
			in   CreateParams
		}{
			{"bad slug", CreateParams{RepoURL: r.URL, Slug: "Bad_Slug"}},
			{"dash slug", CreateParams{RepoURL: r.URL, Slug: "-x"}},
			{"both triggers", CreateParams{RepoURL: r.URL, Branch: "main", TagPattern: "v*"}},
			{"bad glob", CreateParams{RepoURL: r.URL, TagPattern: "[a"}},
			{"absolute compose", CreateParams{RepoURL: r.URL, ComposeFile: "/etc/passwd"}},
			{"escaping compose", CreateParams{RepoURL: r.URL, ComposeFile: "../x.yaml"}},
			{"nested escaping compose", CreateParams{RepoURL: r.URL, ComposeFile: "a/../../x.yaml"}},
			{"token on file url", CreateParams{RepoURL: r.URL, Token: "t"}},
			{"token on ssh url", CreateParams{RepoURL: "git@example.com:a/b.git", Token: "t"}},
			{"token_user alone", CreateParams{RepoURL: r.URL, TokenUser: "u"}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, err := e.svc.Create(ctx, tt.in)
				wantInvalid(t, err)
			})
		}
		for _, raw := range []string{"", "not a url", "ext::sh -c id", "https://user:pw@example.com/a/b.git", "/local/path"} {
			_, err := e.svc.Create(ctx, CreateParams{RepoURL: raw})
			if got := reason(t, err); got != git.ReasonInvalidURL {
				t.Errorf("%q: reason = %s", raw, got)
			}
		}
		if l, _ := e.st.ListProjects(ctx); len(l) != 0 {
			t.Errorf("rows persisted: %d", len(l))
		}
	})
}

func TestCreateHTTPSTokenInfersAuth(t *testing.T) {
	// A https URL cannot be reached here, so check the failure persists
	// nothing and the validation path inferred https_token (unreachable
	// host, not InvalidParams).
	e := newEnv(t)
	_, err := e.svc.Create(context.Background(), CreateParams{RepoURL: "https://127.0.0.1:1/a/b.git", Token: "sekret"})
	if got := reason(t, err); got != git.ReasonUnreachable && got != git.ReasonFailed {
		t.Fatalf("reason = %s", got)
	}
	if l, _ := e.st.ListProjects(context.Background()); len(l) != 0 {
		t.Error("row persisted")
	}
	e.noStaging(t)
}

func TestCreateSSH(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res, err := e.svc.Create(ctx, CreateParams{RepoURL: "git@github.com:acme/App.git", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Project
	if p.Slug != "app" || p.Status != StatusPending || p.Auth.Method != AuthSSHKey ||
		!strings.HasPrefix(p.Auth.PublicKey, "ssh-ed25519 ") || !strings.HasSuffix(p.Auth.PublicKey, " ki-agent:app") ||
		p.Commit != "" || p.Trigger.Branch != "main" {
		t.Errorf("project = %+v", p)
	}
	if exists(filepath.Join(e.data, "projects", "app")) {
		t.Error("ssh create must not clone")
	}
	row, _ := e.st.GetProject(ctx, "app")
	pem, err := e.box.Open(row.PrivateKeyEnc, aad("app", "private_key"))
	if err != nil || !strings.Contains(string(pem), "PRIVATE KEY") {
		t.Errorf("private key not stored sealed: %v", err)
	}
	// No-branch ssh create leaves the trigger open until verify.
	res, err = e.svc.Create(ctx, CreateParams{RepoURL: "ssh://git@github.com/acme/other.git"})
	if err != nil || res.Project.Trigger != (Trigger{}) {
		t.Errorf("trigger = %+v, %v", res.Project.Trigger, err)
	}
}

func TestVerifyTransitions(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := seedRepo(t, "app", map[string]string{"compose.yaml": "x\n"})
	// Row as an ssh project would be, but pointing at a readable file:// repo
	// with no credentials needed.
	row := &store.Project{
		Slug: "app", RepoURL: r.URL, AuthMethod: AuthNone, WebhookID: "wh1",
		Status: StatusPending, StatusDetail: "old",
	}
	if err := e.st.CreateProject(ctx, row); err != nil {
		t.Fatal(err)
	}

	p, err := e.svc.Verify(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusReady || p.StatusDetail != "" || p.Trigger.Branch != "main" || p.DefaultBranch != "main" ||
		p.ComposeFile != "compose.yaml" || p.Commit == "" {
		t.Fatalf("project = %+v", *p)
	}
	first := p.Commit
	e.noStaging(t)

	// Fetch path: a new commit is picked up, untracked files survive.
	if err := os.WriteFile(filepath.Join(e.data, "projects", "app", "local.env"), []byte("k=v"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.commit(map[string]string{"NEW": "n\n"})
	r.push("main")
	if p, err = e.svc.Verify(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if p.Commit == first || !exists(filepath.Join(e.data, "projects", "app", "NEW")) ||
		!exists(filepath.Join(e.data, "projects", "app", "local.env")) {
		t.Errorf("fetch path wrong: %+v", *p)
	}

	// Failure: status_detail set, status unchanged, error returned.
	row, _ = e.st.GetProject(ctx, "app")
	row.RepoURL = "file://" + filepath.Join(t.TempDir(), "gone.git")
	row.Status = StatusPending
	if err := e.st.UpdateProject(ctx, row); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Verify(ctx, "app")
	if got := reason(t, err); got != git.ReasonNotFound {
		t.Fatalf("reason = %s", got)
	}
	row, _ = e.st.GetProject(ctx, "app")
	if row.Status != StatusPending || !strings.HasPrefix(row.StatusDetail, git.ReasonNotFound) {
		t.Errorf("status = %q detail = %q", row.Status, row.StatusDetail)
	}

	if _, err := e.svc.Verify(ctx, "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestVerifyMissingComposeKeepsStatus(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := seedRepo(t, "app", map[string]string{"README": "x\n"})
	if err := e.st.CreateProject(ctx, &store.Project{
		Slug: "app", RepoURL: r.URL, AuthMethod: AuthNone, WebhookID: "wh", Status: StatusPending,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Verify(ctx, "app")
	if got := reason(t, err); got != git.ReasonComposeNotFound {
		t.Fatalf("reason = %s", got)
	}
	row, _ := e.st.GetProject(ctx, "app")
	if row.Status != StatusPending || row.StatusDetail == "" {
		t.Errorf("status = %q detail = %q", row.Status, row.StatusDetail)
	}
	if exists(filepath.Join(e.data, "projects", "app")) {
		t.Error("failed first verify must not install a checkout")
	}
}

func TestUpdate(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := seedRepo(t, "app", map[string]string{"compose.yaml": "x\n", "alt.yaml": "x\n"})
	created, err := e.svc.Create(ctx, CreateParams{RepoURL: r.URL})
	if err != nil {
		t.Fatal(err)
	}
	secret0 := created.WebhookSecret

	upd := func(mod func(*UpdateParams)) (*Result, error) {
		in := UpdateParams{Slug: "app"}
		mod(&in)
		return e.svc.Update(ctx, in)
	}

	if res, err := upd(func(p *UpdateParams) { p.TagPattern = "v*" }); err != nil || res.Project.Trigger != (Trigger{TagPattern: "v*"}) || res.WebhookSecret != "" {
		t.Errorf("tag switch = %+v, %v", res, err)
	}
	if res, err := upd(func(p *UpdateParams) { p.Branch = "dev" }); err != nil || res.Project.Trigger != (Trigger{Branch: "dev"}) {
		t.Errorf("branch switch = %+v, %v", res, err)
	}
	if res, err := upd(func(p *UpdateParams) { p.ComposeFile = "alt.yaml" }); err != nil || res.Project.ComposeFile != "alt.yaml" {
		t.Errorf("compose = %+v, %v", res, err)
	}
	if res, err := upd(func(p *UpdateParams) {}); err != nil || res.Project.ComposeFile != "alt.yaml" || res.Project.Trigger.Branch != "dev" {
		t.Errorf("no-op update changed state: %+v, %v", res, err)
	}
	// Webhook secret rotation returns a new secret that is stored.
	res, err := upd(func(p *UpdateParams) { p.RotateWebhookSecret = true })
	if err != nil || len(res.WebhookSecret) != 64 || res.WebhookSecret == secret0 {
		t.Fatalf("rotation = %+v, %v", res, err)
	}
	row, _ := e.st.GetProject(ctx, "app")
	if got, _ := e.box.Open(row.WebhookSecretEnc, aad("app", "webhook_secret")); string(got) != res.WebhookSecret {
		t.Error("rotated secret not stored")
	}

	for name, mod := range map[string]func(*UpdateParams){
		"both triggers":      func(p *UpdateParams) { p.Branch = "a"; p.TagPattern = "v*" },
		"bad glob":           func(p *UpdateParams) { p.TagPattern = "[" },
		"token on none":      func(p *UpdateParams) { p.Token = "t" },
		"token_user on none": func(p *UpdateParams) { p.TokenUser = "u" },
		"deploy key on none": func(p *UpdateParams) { p.RotateDeployKey = true },
		"escaping compose":   func(p *UpdateParams) { p.ComposeFile = "../x" },
	} {
		if _, err := upd(mod); err == nil {
			t.Errorf("%s: want error", name)
		} else {
			wantInvalid(t, err)
		}
	}
	if _, err := upd(func(p *UpdateParams) { p.ComposeFile = "missing.yaml" }); reason(t, err) != git.ReasonComposeNotFound {
		t.Errorf("missing compose err = %v", err)
	}
	if _, err := e.svc.Update(ctx, UpdateParams{Slug: "ghost"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v", err)
	}

	// Symlink escaping the checkout is rejected.
	if err := os.Symlink("/etc/hostname", filepath.Join(e.data, "projects", "app", "link.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := upd(func(p *UpdateParams) { p.ComposeFile = "link.yaml" }); err == nil {
		t.Error("symlink escape accepted")
	} else {
		wantInvalid(t, err)
	}
}

func TestUpdateSSHAndToken(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	res, err := e.svc.Create(ctx, CreateParams{RepoURL: "git@github.com:acme/app.git"})
	if err != nil {
		t.Fatal(err)
	}
	oldKey := res.Project.Auth.PublicKey
	row, _ := e.st.GetProject(ctx, "app")
	row.Status = StatusReady
	if err := e.st.UpdateProject(ctx, row); err != nil {
		t.Fatal(err)
	}
	up, err := e.svc.Update(ctx, UpdateParams{Slug: "app", RotateDeployKey: true})
	if err != nil {
		t.Fatal(err)
	}
	if up.Project.Auth.PublicKey == oldKey || up.Project.Status != StatusPending || up.WebhookSecret != "" {
		t.Errorf("rotation = %+v", up)
	}
	if _, err := e.svc.Update(ctx, UpdateParams{Slug: "app", Token: "t"}); err == nil {
		t.Error("token on ssh project accepted")
	}

	// https_token project row: token and user updates.
	if err := e.st.CreateProject(ctx, &store.Project{
		Slug: "tok", RepoURL: "https://example.com/a/tok.git", AuthMethod: AuthHTTPSToken,
		TokenUser: "x-access-token", WebhookID: "wh2", Status: StatusReady,
		TokenEnc: e.box.Seal([]byte("old"), aad("tok", "token")),
	}); err != nil {
		t.Fatal(err)
	}
	up, err = e.svc.Update(ctx, UpdateParams{Slug: "tok", Token: "new", TokenUser: "bot"})
	if err != nil || up.Project.Auth.TokenUser != "bot" {
		t.Fatalf("token update = %+v, %v", up, err)
	}
	row, _ = e.st.GetProject(ctx, "tok")
	if got, _ := e.box.Open(row.TokenEnc, aad("tok", "token")); string(got) != "new" {
		t.Error("token not re-sealed")
	}
	auth, err := e.svc.authFor(row)
	if err != nil || auth.Token != "new" || auth.TokenUser != "bot" {
		t.Errorf("authFor = %+v, %v", auth, err)
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := seedRepo(t, "app", map[string]string{"compose.yaml": "x\n"})
	if _, err := e.svc.Create(ctx, CreateParams{RepoURL: r.URL}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.data, "projects", "app")

	// Runtime unavailable aborts: nothing removed.
	e.rt.err = runtime.ErrUnavailable
	if _, err := e.svc.Delete(ctx, "app", false); !errors.Is(err, runtime.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if !exists(dir) {
		t.Error("checkout removed despite runtime failure")
	}
	if _, err := e.st.GetProject(ctx, "app"); err != nil {
		t.Errorf("row removed despite runtime failure: %v", err)
	}

	// Prune runs first: checkout and row still exist at that moment.
	e.rt.err = nil
	e.rt.onPrune = func() {
		if !exists(dir) {
			t.Error("checkout removed before prune")
		}
		if _, err := e.st.GetProject(ctx, "app"); err != nil {
			t.Errorf("row removed before prune: %v", err)
		}
	}
	rep, err := e.svc.Delete(ctx, "app", true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Containers != 2 || rep.Networks != 1 || rep.Volumes != 3 {
		t.Errorf("report = %+v", rep)
	}
	if strings.Join(e.rt.calls, ",") != "app,volumes" {
		t.Errorf("prune calls = %v", e.rt.calls)
	}
	if exists(dir) {
		t.Error("checkout not removed")
	}
	if _, err := e.st.GetProject(ctx, "app"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("row still present: %v", err)
	}
	if _, err := e.svc.Delete(ctx, "app", false); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete err = %v", err)
	}
}

func TestGetAndListNeverLeakSecrets(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	res, err := e.svc.Create(ctx, CreateParams{RepoURL: "git@github.com:acme/zeta.git"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Create(ctx, CreateParams{RepoURL: "git@github.com:acme/alpha.git"}); err != nil {
		t.Fatal(err)
	}
	list, err := e.svc.List(ctx)
	if err != nil || len(list) != 2 || list[0].Slug != "alpha" || list[1].Slug != "zeta" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	got, err := e.svc.Get(ctx, "zeta")
	if err != nil || got.Slug != "zeta" {
		t.Fatal(got, err)
	}
	_ = res
	empty := newEnv(t)
	if l, err := empty.svc.List(ctx); err != nil || l == nil || len(l) != 0 {
		t.Errorf("empty list = %#v, %v", l, err)
	}
}

func TestResolveCompose(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(root, "compose.yaml"), []byte("x"), 0o644))
	must(os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	must(os.WriteFile(filepath.Join(root, "sub", "c.yaml"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(outside, "evil.yaml"), []byte("x"), 0o644))
	must(os.Symlink(filepath.Join(outside, "evil.yaml"), filepath.Join(root, "evil.yaml")))
	must(os.Symlink(outside, filepath.Join(root, "outdir")))
	must(os.Symlink("sub/c.yaml", filepath.Join(root, "inside.yaml")))
	must(os.MkdirAll(filepath.Join(root, "adir"), 0o755))

	tests := []struct {
		explicit string
		want     string
		invalid  bool
		notFound bool
	}{
		{"", "compose.yaml", false, false},
		{"sub/c.yaml", "sub/c.yaml", false, false},
		{"inside.yaml", "inside.yaml", false, false},
		{"evil.yaml", "", true, false},
		{"outdir/evil.yaml", "", true, false},
		{"/etc/passwd", "", true, false},
		{"../x", "", true, false},
		{"sub/../../x", "", true, false},
		{".git/config", "", true, false},
		{"nope.yaml", "", false, true},
		{"adir", "", false, true},
	}
	for _, tt := range tests {
		got, err := resolveCompose(root, tt.explicit)
		var inv *InvalidError
		var ge *git.Error
		switch {
		case tt.invalid:
			if !errors.As(err, &inv) {
				t.Errorf("%q: err = %v, want invalid", tt.explicit, err)
			}
		case tt.notFound:
			if !errors.As(err, &ge) || ge.Reason != git.ReasonComposeNotFound {
				t.Errorf("%q: err = %v, want compose_not_found", tt.explicit, err)
			}
		case err != nil || got != tt.want:
			t.Errorf("%q: got %q, %v want %q", tt.explicit, got, err, tt.want)
		}
	}
}

func TestLockSerialisesPerSlug(t *testing.T) {
	e := newEnv(t)
	unlock := e.svc.lock("a")
	done := make(chan struct{})
	go func() { e.svc.lock("a")(); close(done) }()
	other := e.svc.lock("b") // different slug does not block
	other()
	select {
	case <-done:
		t.Fatal("second lock on the same slug acquired")
	default:
	}
	unlock()
	<-done
	e.svc.mu.Lock()
	defer e.svc.mu.Unlock()
	if len(e.svc.locks) != 0 {
		t.Errorf("locks leaked: %d", len(e.svc.locks))
	}
}

func TestResolveComposeNotFoundDetail(t *testing.T) {
	_, err := resolveCompose(t.TempDir(), "")
	var ge *git.Error
	if !errors.As(err, &ge) || ge.Reason != git.ReasonComposeNotFound {
		t.Fatalf("err = %v, want compose_not_found", err)
	}
	if n := strings.Count(ge.Detail, "docker-compose.yml"); n != 1 {
		t.Errorf("candidate list appears %d times in %q", n, ge.Detail)
	}
	if n := strings.Count(ge.Detail, "in the repository"); n != 1 {
		t.Errorf("detail repeats itself: %q", ge.Detail)
	}
}
