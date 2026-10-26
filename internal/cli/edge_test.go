package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-southeners/ki-agent/internal/rpc"
)

const exposedProjectJSON = `{"slug":"shop","repo_url":"https://github.com/acme/shop.git","trigger":{"branch":"main"},
"default_branch":"main","compose_file":"compose.yaml","auth":{"method":"none"},
"webhook":{"path":"/hooks/abc"},"status":"ready","commit":"1a2b3c4d5e6f",
"created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z",
"exposure":[{"service":"web","port":8080,"domains":["shop.example.com","www.example.com"],"source":"project"},
{"service":"api","port":3000,"domains":["api.example.com"],"path":"/v1","source":"labels"}],
"exposure_warnings":["service worker: ki.expose.port is not a number"]}`

const edgeStatusJSON = `{"enabled":true,"state":"running","image":"traefik:v3.1","http_port":80,"https_port":443,
"server_hostnames":["srv.example.com"],"self_signed":{"fingerprint_sha256":"AA:BB","not_after":"2036-01-01T00:00:00Z"},
"certificates":[{"hostname":"srv.example.com","source":"installed","not_after":"2027-01-01T00:00:00Z"},
{"hostname":"shop.example.com","source":"acme"}],"routes":3,"last_error":"boom"}`

func exposeStubs(rec *calls) func(*rpc.Router) {
	return func(r *rpc.Router) {
		r.Handle(stub(rec, "projects.get", exposedProjectJSON))
		r.Handle(stub(rec, "projects.expose", exposedProjectJSON))
		r.Handle(stub(rec, "projects.unexpose", projectJSON))
		r.Handle(stub(rec, "edge.status", edgeStatusJSON))
	}
}

func TestProjectsExpose(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, exposeStubs(rec))

	out, _, err := runCLI(t, sock, "", "projects", "expose", "shop", "web", "--port", "8080",
		"--domain", "shop.example.com", "--domain", "www.example.com", "--path", "/v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Service web of project shop exposed.", "SERVICE", "shop.example.com,www.example.com", "/v1", "labels", "Exposure warnings", "not a number"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	p := rec.last(t, "projects.expose")
	doms, _ := p["domains"].([]any)
	if p["slug"] != "shop" || p["service"] != "web" || p["port"] != float64(8080) || len(doms) != 2 || p["path"] != "/v1" {
		t.Errorf("params = %v", p)
	}

	if _, _, err := runCLI(t, sock, "", "projects", "expose", "shop", "web", "--domain", "a.example.com"); err == nil {
		t.Error("missing --port should fail")
	}
	if _, _, err := runCLI(t, sock, "", "projects", "expose", "shop", "web", "--port", "80"); err == nil {
		t.Error("missing --domain should fail")
	}

	_, _, err = runCLI(t, sock, "", "projects", "expose", "shop", "web", "--port", "80", "--domain", "a.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.last(t, "projects.expose")["path"]; ok {
		t.Error("path should be omitted when unset")
	}

	out, _, err = runCLI(t, sock, "", "projects", "expose", "shop", "web", "--port", "80", "--domain", "a.example.com", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res project
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Exposure) != 2 {
		t.Fatalf("json output %q: %v", out, err)
	}
}

func TestProjectsUnexpose(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, exposeStubs(rec))
	out, _, err := runCLI(t, sock, "", "projects", "unexpose", "app", "web")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Service web of project app unexposed.") || !strings.Contains(out, "No services are exposed.") {
		t.Errorf("output:\n%s", out)
	}
	p := rec.last(t, "projects.unexpose")
	if p["slug"] != "app" || p["service"] != "web" {
		t.Errorf("params = %v", p)
	}
	if _, _, err := runCLI(t, sock, "", "projects", "unexpose", "app"); err == nil {
		t.Error("missing service should fail")
	}
}

func TestProjectsGetExposure(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, exposeStubs(rec))
	out, _, err := runCLI(t, sock, "", "projects", "get", "shop")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SERVICE  PORT  DOMAINS", "PATH", "SOURCE", "api", "3000", "project", "Exposure warnings"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// A project without exposure prints no table.
	sock = startAgent(t, projectsStubs(&calls{}))
	out, _, err = runCLI(t, sock, "", "projects", "get", "app")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Exposure") {
		t.Errorf("unexpected exposure section:\n%s", out)
	}
}

func TestEdgeStatus(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, exposeStubs(rec))
	out, _, err := runCLI(t, sock, "", "edge", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"State", "running", "http 80, https 443", "traefik:v3.1", "srv.example.com",
		"SHA-256 AA:BB, expires 2036-01-01", "Routes", "3", "Last error", "boom",
		"NAMES", "SERVER", "installed", "2027-01-01", "shop.example.com", "acme",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	out, _, err = runCLI(t, sock, "", "edge", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var st edgeStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil || st.Routes != 3 {
		t.Fatalf("json output %q: %v", out, err)
	}
}

func TestEdgeStatusDisabled(t *testing.T) {
	sock := startAgent(t, func(r *rpc.Router) {
		r.Handle(stub(&calls{}, "edge.status", `{"enabled":false,"state":"disabled"}`))
	})
	out, _, err := runCLI(t, sock, "", "edge", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("output:\n%s", out)
	}
}
