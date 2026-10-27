package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/open-southeners/ki/internal/git"
	"github.com/open-southeners/ki/internal/projects"
	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/runtime"
	"github.com/open-southeners/ki/internal/secret"
	"github.com/open-southeners/ki/internal/store"
)

// pruneRuntime counts PruneProject calls; other methods panic if reached.
type pruneRuntime struct {
	runtime.Runtime
	pruned []string
}

func (p *pruneRuntime) PruneProject(_ context.Context, project string, _ bool) (runtime.PruneReport, error) {
	p.pruned = append(p.pruned, project)
	return runtime.PruneReport{Containers: 1}, nil
}

// seedBareRepo creates a bare repo with a compose.yaml on main.
func seedBareRepo(t *testing.T, name string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	bare := filepath.Join(root, name+".git")
	work := filepath.Join(root, "work")
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(root, "init", "--bare", "--initial-branch=main", bare)
	run(root, "init", "--initial-branch=main", work)
	if err := os.WriteFile(filepath.Join(work, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "compose.yaml")
	run(work, "commit", "-m", "init")
	run(work, "push", bare, "main")
	return "file://" + bare
}

func TestProjectsEndToEnd(t *testing.T) {
	repoURL := seedBareRepo(t, "demo")
	data := t.TempDir()
	db, err := store.Open(filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	box, err := secret.LoadOrCreate(filepath.Join(data, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	protocols := []string{"https", "ssh", "file"}
	gc, err := git.New(git.Options{DataDir: data, Protocols: protocols})
	if err != nil {
		t.Fatal(err)
	}
	rt := &pruneRuntime{}
	svc, err := projects.New(projects.Options{
		Store: db, Secret: box, Git: gc, Runtime: rt, DataDir: data, Protocols: protocols,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := startWith(t, func(r *rpc.Router) { projects.NewHandlers(svc).Register(r) })
	c := dialLogClient(t, h.sock)

	wantCode(t, c.call(t, "projects.list", nil, nil), rpc.CodeHandshakeRequired)
	if err := c.call(t, "agent.hello", map[string]any{"protocol": "1.0.0"}, nil); err != nil {
		t.Fatal(err)
	}

	var created struct {
		Project       projects.Project `json:"project"`
		WebhookSecret string           `json:"webhook_secret"`
	}
	if err := c.call(t, "projects.create", map[string]any{"repo_url": repoURL}, &created); err != nil {
		t.Fatal(err)
	}
	if created.Project.Slug != "demo" || created.Project.Status != "ready" || created.Project.Trigger.Branch != "main" ||
		created.Project.ComposeFile != "compose.yaml" || len(created.WebhookSecret) != 64 {
		t.Fatalf("created = %+v", created)
	}
	if _, err := os.Stat(filepath.Join(data, "projects", "demo", "compose.yaml")); err != nil {
		t.Errorf("checkout: %v", err)
	}

	wantCode(t, c.call(t, "projects.create", map[string]any{"repo_url": repoURL}, nil), rpc.CodeAlreadyExists)
	wantCode(t, c.call(t, "projects.create", map[string]any{"repo_url": "ext::sh"}, nil), rpc.CodeRepositoryError)
	wantCode(t, c.call(t, "projects.create", map[string]any{}, nil), rpc.CodeInvalidParams)
	wantCode(t, c.call(t, "projects.get", map[string]any{"slug": "nope"}, nil), rpc.CodeNotFound)

	var got projects.Project
	if err := c.call(t, "projects.get", map[string]any{"slug": "demo"}, &got); err != nil || !reflect.DeepEqual(got, created.Project) {
		t.Fatalf("get = %+v, %v", got, err)
	}
	var list []projects.Project
	if err := c.call(t, "projects.list", nil, &list); err != nil || len(list) != 1 || list[0].Slug != "demo" {
		t.Fatalf("list = %+v, %v", list, err)
	}

	var del runtime.PruneReport
	if err := c.call(t, "projects.delete", map[string]any{"slug": "demo"}, &del); err != nil || del.Containers != 1 {
		t.Fatalf("delete = %+v, %v", del, err)
	}
	if strings.Join(rt.pruned, ",") != "demo" {
		t.Errorf("pruned = %v", rt.pruned)
	}
	if _, err := os.Stat(filepath.Join(data, "projects", "demo")); !os.IsNotExist(err) {
		t.Errorf("checkout still present: %v", err)
	}
	if err := c.call(t, "projects.list", nil, &list); err != nil || len(list) != 0 {
		t.Fatalf("list after delete = %+v, %v", list, err)
	}
}
