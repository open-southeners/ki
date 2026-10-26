package deploys

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/compose"
	"github.com/open-southeners/ki-agent/internal/git"
	"github.com/open-southeners/ki-agent/internal/projects"
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

const okCompose = "services:\n  web:\n    image: example/web\n"

// seedRepo builds a bare repository with compose.yaml on main, a dev branch
// carrying DEV and the tags v1.0.0 (lightweight), v1.1.0 (annotated) and x1.
func seedRepo(t testing.TB, files map[string]string) *repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "app.git")
	work := filepath.Join(root, "work")
	gitRun(t, root, "init", "--bare", "--initial-branch=main", bare)
	gitRun(t, root, "init", "--initial-branch=main", work)
	gitRun(t, work, "remote", "add", "origin", bare)
	r := &repo{t: t, URL: "file://" + bare, work: work}
	r.commit(files)
	gitRun(t, work, "tag", "v1.0.0")
	r.commit(map[string]string{"A": "1\n"})
	gitRun(t, work, "tag", "-a", "-m", "release", "v1.1.0")
	gitRun(t, work, "tag", "x1")
	gitRun(t, work, "checkout", "-b", "dev")
	r.commit(map[string]string{"DEV": "dev\n"})
	gitRun(t, work, "checkout", "main")
	gitRun(t, work, "push", "origin", "main", "dev", "--tags")
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

func (r *repo) rev(ref string) string { return gitRun(r.t, r.work, "rev-parse", ref+"^{commit}") }

// fakeComposer is a scriptable Composer.
type fakeComposer struct {
	mu       sync.Mutex
	model    string
	cfgErr   error
	buildErr error
	upErr    error
	pullErr  error
	// block, when set, makes Build wait for it to be closed (or ctx).
	block chan struct{}
	// entered receives the project each time Build is entered.
	entered chan string
	calls   []string
	files   []string
}

func newComposer() *fakeComposer {
	return &fakeComposer{model: `{"name":"app","services":{"web":{"image":"example/web"}}}`}
}

func (f *fakeComposer) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeComposer) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeComposer) Config(_ context.Context, dir, file, project string) ([]byte, error) {
	f.record("config " + project)
	if f.cfgErr != nil {
		return nil, f.cfgErr
	}
	return []byte(f.model), nil
}

func (f *fakeComposer) Build(ctx context.Context, file, project string, out func(string, string)) error {
	f.record("build " + project)
	f.mu.Lock()
	f.files = append(f.files, file)
	f.mu.Unlock()
	if f.entered != nil {
		f.entered <- project
	}
	out("stdout", "building "+project)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return &compose.Error{Op: "build", Err: ctx.Err()}
		}
	}
	return f.buildErr
}

func (f *fakeComposer) Pull(_ context.Context, file, project string, out func(string, string)) error {
	f.record("pull " + project)
	out("stdout", "pulling "+project)
	return f.pullErr
}

func (f *fakeComposer) Up(_ context.Context, file, project string, out func(string, string)) error {
	f.record("up " + project)
	out("stderr", "starting "+project)
	return f.upErr
}

// fakeRT is a scriptable Runtime (the deploys one) that also stands in for the
// projects service's runtime.
type fakeRT struct {
	runtime.Runtime
	mu      sync.Mutex
	listErr error
	state   string
	health  string
	exit    int
	none    bool
}

func newRT() *fakeRT { return &fakeRT{state: "running"} }

func (f *fakeRT) set(fn func(*fakeRT)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeRT) List(_ context.Context, project string) ([]runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.none {
		return nil, nil
	}
	return []runtime.Container{{ID: "c1", Name: "web-1", Service: "web", Project: project}}, nil
}

func (f *fakeRT) Inspect(_ context.Context, id string) (runtime.ContainerDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return runtime.ContainerDetail{Container: runtime.Container{ID: id, State: f.state, Service: "web"},
		Health: f.health, ExitCode: f.exit}, nil
}

func (f *fakeRT) PruneProject(context.Context, string, bool) (runtime.PruneReport, error) {
	return runtime.PruneReport{}, nil
}

type fixture struct {
	st   *store.Store
	svc  *projects.Service
	mgr  *Manager
	repo *repo
	comp *fakeComposer
	rt   *fakeRT
	data string
}

type fixtureOpts struct {
	files   map[string]string
	slug    string
	branch  string
	pattern string
	tweak   func(*Options)
	// noStart leaves the Manager unstarted.
	noStart bool
}

func newFixture(t *testing.T, fo fixtureOpts) *fixture {
	t.Helper()
	if fo.files == nil {
		fo.files = map[string]string{"compose.yaml": okCompose}
	}
	if fo.slug == "" {
		fo.slug = "app"
	}
	r := seedRepo(t, fo.files)
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
	protocols := []string{"file"}
	g, err := git.New(git.Options{DataDir: data, Protocols: protocols})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := newRT()
	svc, err := projects.New(projects.Options{Store: st, Secret: box, Git: g, Runtime: rt, DataDir: data,
		Protocols: protocols, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	cp := projects.CreateParams{RepoURL: r.URL, Slug: fo.slug, Branch: fo.branch, TagPattern: fo.pattern}
	if _, err := svc.Create(context.Background(), cp); err != nil {
		t.Fatal(err)
	}
	comp := newComposer()
	opts := Options{Store: st, Projects: svc, Git: g, Compose: comp, Runtime: rt, DataDir: data,
		PollInterval: 5 * time.Millisecond, HealthTimeout: 2 * time.Second, Logger: log}
	if fo.tweak != nil {
		fo.tweak(&opts)
	}
	mgr, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !fo.noStart {
		if err := mgr.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(mgr.Close)
	}
	return &fixture{st: st, svc: svc, mgr: mgr, repo: r, comp: comp, rt: rt, data: data}
}

func terminal(status string) bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusSuperseded, StatusInterrupted:
		return true
	}
	return false
}

// wait blocks until deploy id is finished and returns it.
func (f *fixture) wait(t *testing.T, id int64) Deploy {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		d, err := f.mgr.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if terminal(d.Status) {
			return *d
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("deploy %d did not finish", id)
	return Deploy{}
}

func (f *fixture) trigger(t *testing.T, ref string) *Deploy {
	t.Helper()
	d, err := f.mgr.Trigger(context.Background(), TriggerRequest{Project: "app", Ref: ref, Source: SourceAPI})
	if err != nil {
		t.Fatalf("Trigger(%q) = %v", ref, err)
	}
	return d
}

func triggerReason(err error) string {
	var te *TriggerError
	if errors.As(err, &te) {
		return te.Reason
	}
	return ""
}

func (f *fakeRT) Stats(context.Context, string) (runtime.StatsSnapshot, error) {
	return runtime.StatsSnapshot{}, nil
}
