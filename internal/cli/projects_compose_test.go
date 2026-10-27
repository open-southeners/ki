package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-southeners/ki/internal/rpc"
)

const composeProjectJSON = `{"slug":"site","source":"compose","compose_version":3,"status":"ready",
"created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}`

const composeYAML = "services:\n  web:\n    image: nginx:1\n"

func composeStubs(rec *calls, setResult string, updates []string) func(*rpc.Router) {
	base := deployStubs(rec, deployQueuedJSON, updates)
	return func(r *rpc.Router) {
		base(r)
		r.Handle(stub(rec, "projects.create", `{"project":`+composeProjectJSON+`}`))
		r.Handle(stub(rec, "projects.get", composeProjectJSON))
		r.Handle(stub(rec, "projects.list", `[`+composeProjectJSON+`,`+projectJSON+`]`))
		r.Handle(stub(rec, "projects.compose.get", `{"version":2,"content":`+jsonString(composeYAML)+`,"created_at":"x","created_by":"ann"}`))
		r.Handle(stub(rec, "projects.compose.set", setResult))
		r.Handle(stub(rec, "projects.compose.history", `[{"version":3,"created_at":"2026-01-03","created_by":"ann","note":"bump","deployed":true},{"version":2,"created_at":"2026-01-02","created_by":"bob","deployed":false}]`))
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func writeCompose(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(p, []byte(composeYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProjectsCreateCompose(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, composeStubs(rec, `{"version":1}`, nil))
	out, _, err := runCLI(t, sock, "", "projects", "create", "site", "--compose-file", writeCompose(t))
	if err != nil {
		t.Fatal(err)
	}
	p := rec.last(t, "projects.create")
	if p["slug"] != "site" || p["compose"] != composeYAML || len(p) != 2 {
		t.Errorf("params = %v", p)
	}
	if !strings.Contains(out, "created") || !strings.Contains(out, "compose") {
		t.Errorf("out = %q", out)
	}

	_, _, err = runCLI(t, sock, composeYAML, "projects", "create", "site", "--compose-file", "-")
	if err != nil || rec.last(t, "projects.create")["compose"] != composeYAML {
		t.Errorf("stdin: %v", err)
	}
}

func TestProjectsCreateComposeRejectsGitFlags(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, composeStubs(rec, `{"version":1}`, nil))
	f := writeCompose(t)
	for _, extra := range [][]string{{"--branch", "main"}, {"--tags", "v*"}, {"--compose", "x.yml"}, {"--token-stdin"}, {"--token-user", "u"}, {"--slug", "y"}} {
		args := append([]string{"projects", "create", "site", "--compose-file", f}, extra...)
		if _, _, err := runCLI(t, sock, "", args...); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Errorf("%v: err = %v", extra, err)
		}
	}
	if _, _, err := runCLI(t, sock, "", "projects", "create", "https://x/y.git", "--compose-file", f); err == nil {
		t.Error("git URL accepted")
	}
	if rec.count("projects.create") != 0 {
		t.Error("create called")
	}
}

func TestComposeFileTooLarge(t *testing.T) {
	sock := startAgent(t, composeStubs(&calls{}, `{"version":1}`, nil))
	big := strings.Repeat("a", maxComposeSize+1)
	_, _, err := runCLI(t, sock, big, "projects", "create", "site", "--compose-file", "-")
	if err == nil || !strings.Contains(err.Error(), "larger than 256 KiB") {
		t.Errorf("err = %v", err)
	}
}

func TestComposeGet(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, composeStubs(rec, `{"version":1}`, nil))
	out, _, err := runCLI(t, sock, "", "projects", "compose", "get", "site", "--version", "2")
	if err != nil {
		t.Fatal(err)
	}
	if out != composeYAML {
		t.Errorf("out = %q", out)
	}
	if p := rec.last(t, "projects.compose.get"); p["slug"] != "site" || p["version"] != float64(2) {
		t.Errorf("params = %v", p)
	}
	out, _, err = runCLI(t, sock, "", "projects", "compose", "get", "site", "--json")
	if err != nil || !strings.Contains(out, `"content"`) {
		t.Errorf("json: %v %q", err, out)
	}
	if p := rec.last(t, "projects.compose.get"); len(p) != 1 {
		t.Errorf("params = %v", p)
	}
}

func TestComposeSet(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, composeStubs(rec, `{"version":4}`, nil))
	out, _, err := runCLI(t, sock, "", "projects", "compose", "set", "site", "--file", writeCompose(t), "--note", "bump")
	if err != nil {
		t.Fatal(err)
	}
	if out != "Stored compose v4 for site.\n" {
		t.Errorf("out = %q", out)
	}
	if p := rec.last(t, "projects.compose.set"); p["compose"] != composeYAML || p["note"] != "bump" || p["deploy"] != nil {
		t.Errorf("params = %v", p)
	}
	if _, _, err := runCLI(t, sock, "", "projects", "compose", "set", "site", "--file", writeCompose(t), "--detach"); err == nil {
		t.Error("--detach without --deploy accepted")
	}
}

func TestComposeSetDeployFollows(t *testing.T) {
	rec := &calls{}
	res := `{"version":4,"deploy":` + deployQueuedJSON + `}`
	sock := startAgent(t, composeStubs(rec, res, []string{
		`{"type":"log","stream":"agent","text":"fetch"}`,
		`{"type":"log","stream":"stdout","text":"pulling\n"}`,
		statusUpdate(deploySucceededJSON),
	}))
	out, _, err := runCLI(t, sock, "", "projects", "compose", "set", "site", "--file", writeCompose(t), "--deploy")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "Stored compose v4 for site.\nDeploy 42 queued") || !strings.Contains(out, "pulling") {
		t.Errorf("out = %q", out)
	}
	if rec.last(t, "projects.compose.set")["deploy"] != true || rec.last(t, "deploys.subscribe")["id"] != float64(42) {
		t.Error("deploy not requested or followed")
	}

	out, _, err = runCLI(t, sock, "", "projects", "compose", "set", "site", "--file", writeCompose(t), "--deploy", "--detach")
	if err != nil || rec.count("deploys.subscribe") != 1 || !strings.Contains(out, "Deploy 42 queued") {
		t.Errorf("detach: %v %q", err, out)
	}
}

func TestComposeSetViolations(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, func(r *rpc.Router) {
		r.Handle("projects.compose.set", func(_ context.Context, _ *rpc.Peer, _ json.RawMessage) (any, error) {
			return nil, rpc.NewError(rpc.CodeInvalidParams, "compose file rejected").WithData(map[string]any{
				"violations": []map[string]string{{"service": "web", "rule": "privileged", "detail": "not allowed"}},
			})
		})
	})
	_ = rec
	_, _, err := runCLI(t, sock, "", "projects", "compose", "set", "site", "--file", writeCompose(t))
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"compose file rejected", "SERVICE", "web", "privileged", "not allowed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestComposeHistory(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, composeStubs(rec, `{"version":1}`, nil))
	out, _, err := runCLI(t, sock, "", "projects", "compose", "history", "site", "--limit", "5")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || strings.Join(strings.Fields(lines[0]), " ") != "VERSION CREATED BY NOTE DEPLOYED" {
		t.Fatalf("out:\n%s", out)
	}
	if !strings.HasSuffix(lines[1], "yes") || !strings.HasSuffix(lines[2], "no") {
		t.Errorf("rows:\n%s", out)
	}
	if rec.last(t, "projects.compose.history")["limit"] != float64(5) {
		t.Error("limit not sent")
	}
}

func TestComposeProjectView(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, composeStubs(rec, `{"version":1}`, nil))
	out, _, err := runCLI(t, sock, "", "projects", "get", "site")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Source", "compose", "v3", "ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("get missing %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"Repository", "Trigger", "Commit"} {
		if strings.Contains(out, not) {
			t.Errorf("get shows %q:\n%s", not, out)
		}
	}
	out, _, err = runCLI(t, sock, "", "projects", "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.Contains(lines[1], "compose v3") || !strings.Contains(lines[2], "git") {
		t.Errorf("list:\n%s", out)
	}
}
