package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-southeners/ki-agent/internal/rpc"
)

func imageStubs(rec *calls, setResult string, updates []string) func(*rpc.Router) {
	base := composeStubs(rec, `{"version":1}`, updates)
	return func(r *rpc.Router) {
		base(r)
		r.Handle(stub(rec, "projects.image.set", setResult))
	}
}

func TestProjectsCreateImage(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, imageStubs(rec, `{"version":2,"image":"x"}`, nil))
	out, _, err := runCLI(t, sock, "", "projects", "create", "app", "--image", "reg.io/a/b:1",
		"--service", "web", "--port", "8080", "--domain", "a.example.com", "--domain", "b.example.com",
		"--path", "/x", "--volume", "data:/d", "--volume", "c:/c:ro", "--env", "A=b=c", "--env", "E=")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "created") {
		t.Errorf("out = %q", out)
	}
	p := rec.last(t, "projects.create")
	img, _ := p["image"].(map[string]any)
	b, _ := json.Marshal(img)
	want := `{"domains":["a.example.com","b.example.com"],"env":{"A":"b=c","E":""},"image":"reg.io/a/b:1","path":"/x","port":8080,"service":"web","volumes":["data:/d","c:/c:ro"]}`
	if p["slug"] != "app" || len(p) != 2 || string(b) != want {
		t.Errorf("params = %v (%s)", p, b)
	}

	if _, _, err = runCLI(t, sock, "", "projects", "create", "app", "--image", "nginx:1", "--json"); err != nil {
		t.Fatal(err)
	}
	if img, _ := rec.last(t, "projects.create")["image"].(map[string]any); len(img) != 1 {
		t.Errorf("minimal image = %v", img)
	}
}

func TestProjectsCreateImageConflicts(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, imageStubs(rec, `{}`, nil))
	cases := [][]string{
		{"projects", "create", "app", "--image", "nginx:1", "--compose-file", "x"},
		{"projects", "create", "https://x/y.git", "--image", "nginx:1"},
		{"projects", "create", "app", "--image", "nginx:1", "--branch", "main"},
		{"projects", "create", "app", "--image", "nginx:1", "--domain", "a.example.com"},
		{"projects", "create", "app", "--image", "nginx:1", "--env", "NOEQUALS"},
		{"projects", "create", "app", "--image", "nginx:1", "--env", "A=1", "--env", "A=2"},
		{"projects", "create", "app", "--image", "nginx:1", "--port", "70000"},
		{"projects", "create", "https://x/y.git", "--port", "80"},
	}
	for _, args := range cases {
		if _, _, err := runCLI(t, sock, "", args...); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
	if rec.count("projects.create") != 0 {
		t.Error("create was called")
	}
}

func TestImageSet(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, imageStubs(rec, `{"version":5,"image":"reg.io/a/b:2"}`, nil))
	out, _, err := runCLI(t, sock, "", "projects", "image", "set", "app", "2", "--service", "web", "--note", "bump")
	if err != nil {
		t.Fatal(err)
	}
	if out != "Stored compose v5 for app: reg.io/a/b:2.\n" {
		t.Errorf("out = %q", out)
	}
	p := rec.last(t, "projects.image.set")
	if p["slug"] != "app" || p["tag"] != "2" || p["image"] != nil || p["service"] != "web" || p["note"] != "bump" || p["deploy"] != nil {
		t.Errorf("params = %v", p)
	}

	out, _, err = runCLI(t, sock, "", "projects", "image", "set", "app", "--image", "reg.io/a/b:3", "--json")
	if err != nil || !strings.Contains(out, `"version"`) {
		t.Fatalf("json: %v %q", err, out)
	}
	if p := rec.last(t, "projects.image.set"); p["image"] != "reg.io/a/b:3" || p["tag"] != nil {
		t.Errorf("ref params = %v", p)
	}

	for _, args := range [][]string{
		{"projects", "image", "set", "app"},
		{"projects", "image", "set", "app", "2", "--image", "x:1"},
		{"projects", "image", "set", "app", "2", "--detach"},
	} {
		if _, _, err := runCLI(t, sock, "", args...); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
}

func TestImageSetDeployFollows(t *testing.T) {
	rec := &calls{}
	res := `{"version":5,"image":"reg.io/a/b:2","deploy":` + deployQueuedJSON + `}`
	sock := startAgent(t, imageStubs(rec, res, []string{
		`{"type":"log","stream":"stdout","text":"pulling\n"}`,
		statusUpdate(deploySucceededJSON),
	}))
	out, _, err := runCLI(t, sock, "", "projects", "image", "set", "app", "2", "--deploy")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "Stored compose v5 for app: reg.io/a/b:2.\nDeploy 42 queued") || !strings.Contains(out, "pulling") {
		t.Errorf("out = %q", out)
	}
	if rec.last(t, "projects.image.set")["deploy"] != true || rec.count("deploys.subscribe") != 1 {
		t.Error("deploy not requested or followed")
	}
	out, _, err = runCLI(t, sock, "", "projects", "image", "set", "app", "2", "--deploy", "--detach")
	if err != nil || rec.count("deploys.subscribe") != 1 || !strings.Contains(out, "Deploy 42 queued") {
		t.Errorf("detach: %v %q", err, out)
	}
}
