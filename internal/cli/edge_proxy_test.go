package cli

import (
	"strings"
	"testing"

	"github.com/open-southeners/ki-agent/internal/rpc"
)

const behindStatusJSON = `{"enabled":true,"state":"running","image":"traefik:v3.1","http_port":80,"https_port":443,
"mode":"behind","listen":"127.0.0.1:8480","server_hostnames":["srv.example.com"],
"self_signed":{"fingerprint_sha256":"AA","not_after":"2036-01-01T00:00:00Z"},"certificates":[],"routes":2}`

func proxyStubs(rec *calls, status string) func(*rpc.Router) {
	return func(r *rpc.Router) {
		r.Handle(stub(rec, "edge.status", status))
		r.Handle(stub(rec, "projects.list", `[`+projectJSON+`]`))
		r.Handle(stub(rec, "projects.get", exposedProjectJSON))
	}
}

func TestEdgeProxyConfigNginxExplicit(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, proxyStubs(rec, behindStatusJSON))
	out, _, err := runCLI(t, sock, "", "edge", "proxy-config", "--domain", "b.example.com", "--domain", "a.example.com", "--domain", "a.example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# --- a.example.com", "# --- b.example.com",
		"proxy_pass http://127.0.0.1:8480;", "proxy_http_version 1.1;", "proxy_set_header Host $host;",
		"X-Forwarded-For $proxy_add_x_forwarded_for;", "X-Forwarded-Proto $scheme;",
		"Upgrade $http_upgrade;", "Connection $connection_upgrade;", "proxy_read_timeout 300s;",
		"map $http_upgrade $connection_upgrade", "TLS stays on this front server", "never writes nginx config",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "location / {") != 2 || strings.Index(out, "a.example.com") > strings.Index(out, "b.example.com") {
		t.Errorf("blocks:\n%s", out)
	}
	if rec.count("projects.list") != 0 {
		t.Error("projects queried despite --domain")
	}
}

func TestEdgeProxyConfigDerivedCaddy(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, proxyStubs(rec, behindStatusJSON))
	out, _, err := runCLI(t, sock, "", "edge", "proxy-config", "--format", "caddy")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"srv.example.com", "shop.example.com", "www.example.com", "api.example.com"} {
		if !strings.Contains(out, "\n"+d+" {\n    reverse_proxy 127.0.0.1:8480\n}\n") {
			t.Errorf("missing site %s:\n%s", d, out)
		}
	}
	if strings.Contains(out, "proxy_pass") {
		t.Errorf("nginx output in caddy format:\n%s", out)
	}
}

func TestEdgeProxyConfigDerivedNginx(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, proxyStubs(rec, behindStatusJSON))
	out, _, err := runCLI(t, sock, "", "edge", "proxy-config")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "location / {") != 4 {
		t.Errorf("want 4 blocks:\n%s", out)
	}
}

func TestEdgeProxyConfigRefusals(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, proxyStubs(rec, edgeStatusJSON)) // front mode
	out, _, err := runCLI(t, sock, "", "edge", "proxy-config", "--domain", "a.example.com")
	if err == nil || !strings.Contains(err.Error(), "only for edge.mode: behind") || out != "" {
		t.Errorf("front mode: err=%v out=%q", err, out)
	}

	sock = startAgent(t, proxyStubs(&calls{}, behindStatusJSON))
	if _, _, err := runCLI(t, sock, "", "edge", "proxy-config", "--format", "apache"); err == nil {
		t.Error("bad format accepted")
	}
	if _, _, err := runCLI(t, sock, "", "edge", "proxy-config", "--domain", "bad host"); err == nil {
		t.Error("bad domain accepted")
	}
}
