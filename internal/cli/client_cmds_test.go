package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/agent"
	"github.com/open-southeners/ki-agent/internal/config"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/server"
)

const projectJSON = `{"slug":"app","repo_url":"https://github.com/acme/app.git","trigger":{"branch":"main"},
"default_branch":"main","compose_file":"compose.yaml","auth":{"method":"none"},
"webhook":{"path":"/hooks/abc"},"status":"ready","status_detail":"","commit":"1a2b3c4d5e6f7a8b9c",
"created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}`

const sshProjectJSON = `{"slug":"priv","repo_url":"git@github.com:acme/priv.git","trigger":{"tag_pattern":"v*"},
"default_branch":"","compose_file":"","auth":{"method":"ssh_key","public_key":"ssh-ed25519 AAAAKEY ki-agent:priv"},
"webhook":{"path":"/hooks/def"},"status":"pending_access","status_detail":"","commit":"",
"created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}`

// calls records the params of every stubbed method call.
type calls struct {
	mu sync.Mutex
	m  map[string][]json.RawMessage
}

func (c *calls) add(method string, p json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string][]json.RawMessage{}
	}
	c.m[method] = append(c.m[method], p)
}

// last returns the params of the last call to method, decoded.
func (c *calls) last(t *testing.T, method string) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.m[method]
	if len(l) == 0 {
		t.Fatalf("%s was not called", method)
	}
	var out map[string]any
	if err := json.Unmarshal(l[len(l)-1], &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (c *calls) count(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m[method])
}

// startAgent serves the real agent methods plus register's stubs on a temp
// socket and returns the socket path.
func startAgent(t *testing.T, register func(*rpc.Router)) string {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Socket.Path = filepath.Join(dir, "ki.sock")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.API.Listen = "127.0.0.1:0"
	cfg.API.EdgeListen = "127.0.0.1:0"
	router := rpc.NewRouter()
	agent.New().Register(router)
	if register != nil {
		register(router)
	}
	srv, err := server.New(server.Options{Config: &cfg, Router: router, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})
	return cfg.Socket.Path
}

// stub returns a handler answering with canned JSON and recording its params.
func stub(rec *calls, method, result string) (string, rpc.Handler) {
	return method, func(_ context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
		rec.add(method, params)
		return json.RawMessage(result), nil
	}
}

func projectsStubs(rec *calls) func(*rpc.Router) {
	return func(r *rpc.Router) {
		r.Handle(stub(rec, "projects.create", `{"project":`+sshProjectJSON+`,"webhook_secret":"s3cr3t"}`))
		r.Handle(stub(rec, "projects.get", projectJSON))
		r.Handle(stub(rec, "projects.list", `[`+projectJSON+`,`+sshProjectJSON+`]`))
		r.Handle(stub(rec, "projects.verify", projectJSON))
		r.Handle(stub(rec, "projects.update", `{"project":`+projectJSON+`}`))
		r.Handle(stub(rec, "projects.delete", `{"containers":3,"networks":1,"volumes":0,"kept_volumes":[]}`))
	}
}

// runCLI executes the root command against sock.
func runCLI(t *testing.T, sock, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newRootCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append([]string{"--socket", sock}, args...))
	err = cmd.Execute()
	return out.String(), errb.String(), err
}

func TestProjectsCreate(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, projectsStubs(rec))

	out, _, err := runCLI(t, sock, "", "projects", "create", "git@github.com:acme/priv.git", "--tags", "v*", "--slug", "priv")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Project priv created (pending_access)", "tags v*", "shown only once", "s3cr3t",
		"/hooks/def", "application/json", "Tag push events", "ssh-ed25519 AAAAKEY",
		"read-only deploy key", "ki-agent projects verify priv",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	p := rec.last(t, "projects.create")
	if p["repo_url"] != "git@github.com:acme/priv.git" || p["tag_pattern"] != "v*" || p["slug"] != "priv" {
		t.Errorf("params = %v", p)
	}
	for _, k := range []string{"token", "token_user", "branch", "compose_file"} {
		if _, ok := p[k]; ok {
			t.Errorf("param %q should be absent: %v", k, p)
		}
	}

	out, _, err = runCLI(t, sock, "", "projects", "create", "git@github.com:acme/priv.git", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		WebhookSecret string `json:"webhook_secret"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.WebhookSecret != "s3cr3t" {
		t.Fatalf("json output %q: %v", out, err)
	}
	if strings.Contains(out, "shown only once") {
		t.Error("--json must print the raw result only")
	}
}

func TestProjectsCreateToken(t *testing.T) {
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "tok")
	if err := os.WriteFile(tokFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		stdin     string
		args      []string
		wantToken string
		wantUser  string
		wantErr   string
	}{
		{"stdin", "stdin-token\n", []string{"--token-stdin", "--token-user", "bot"}, "stdin-token", "bot", ""},
		{"file", "", []string{"--token-file", tokFile}, "file-token", "", ""},
		{"empty stdin", "  \n", []string{"--token-stdin"}, "", "", "token is empty"},
		{"both", "x", []string{"--token-stdin", "--token-file", tokFile}, "", "", "none of the others"},
		{"user without token", "", []string{"--token-user", "bot"}, "", "", "--token-user needs"},
		{"branch and tags", "", []string{"--branch", "a", "--tags", "v*"}, "", "", "none of the others"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &calls{}
			sock := startAgent(t, projectsStubs(rec))
			args := append([]string{"projects", "create", "https://github.com/acme/app.git"}, tt.args...)
			_, _, err := runCLI(t, sock, tt.stdin, args...)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			p := rec.last(t, "projects.create")
			if p["token"] != tt.wantToken {
				t.Errorf("token = %v", p["token"])
			}
			if tt.wantUser == "" {
				if _, ok := p["token_user"]; ok {
					t.Error("token_user should be absent")
				}
			} else if p["token_user"] != tt.wantUser {
				t.Errorf("token_user = %v", p["token_user"])
			}
		})
	}
}

func TestNoPlainTokenFlag(t *testing.T) {
	_, _, err := runCLI(t, "/nonexistent", "", "projects", "create", "https://x/y.git", "--token", "abc")
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("err = %v", err)
	}
}

func TestProjectsListGetVerify(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, projectsStubs(rec))

	out, _, err := runCLI(t, sock, "", "projects", "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("output:\n%s", out)
	}
	if f := strings.Fields(lines[0]); strings.Join(f, " ") != "SLUG STATUS TRIGGER REPOSITORY" {
		t.Errorf("header = %q", lines[0])
	}
	// Columns are aligned: every row has REPOSITORY content at the same offset.
	col := strings.Index(lines[0], "REPOSITORY")
	if strings.Index(lines[1], "https://") != col || strings.Index(lines[2], "git@") != col {
		t.Errorf("misaligned:\n%s", out)
	}
	if !strings.Contains(lines[2], "pending_access") || !strings.Contains(lines[2], "tags v*") {
		t.Errorf("row = %q", lines[2])
	}

	out, _, err = runCLI(t, sock, "", "projects", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list) != 2 {
		t.Fatalf("json: %v %q", err, out)
	}

	out, _, err = runCLI(t, sock, "", "projects", "get", "app")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Slug", "Repository", "branch main", "compose.yaml", "1a2b3c4d5e6f", "/hooks/abc"} {
		if !strings.Contains(out, want) {
			t.Errorf("get output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "1a2b3c4d5e6f7") {
		t.Error("commit should be abbreviated")
	}
	if rec.last(t, "projects.get")["slug"] != "app" {
		t.Error("slug not sent")
	}

	if _, _, err = runCLI(t, sock, "", "projects", "verify", "app"); err != nil {
		t.Fatal(err)
	}
	if rec.count("projects.verify") != 1 {
		t.Error("verify not called")
	}
}

func TestProjectsListEmpty(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, func(r *rpc.Router) { r.Handle(stub(rec, "projects.list", `[]`)) })
	out, _, err := runCLI(t, sock, "", "projects", "list")
	if err != nil || !strings.Contains(out, "No projects.") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestProjectsUpdate(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, func(r *rpc.Router) {
		projectsStubs(rec)(r)
	})
	if _, _, err := runCLI(t, sock, "", "projects", "update", "app"); err == nil || !strings.Contains(err.Error(), "nothing to update") {
		t.Fatalf("err = %v", err)
	}
	if rec.count("projects.update") != 0 {
		t.Fatal("empty update must not call the agent")
	}
	_, _, err := runCLI(t, sock, "", "projects", "update", "app", "--branch", "dev", "--compose", "deploy/c.yaml", "--rotate-webhook-secret", "--rotate-deploy-key")
	if err != nil {
		t.Fatal(err)
	}
	p := rec.last(t, "projects.update")
	if p["slug"] != "app" || p["branch"] != "dev" || p["compose_file"] != "deploy/c.yaml" ||
		p["rotate_webhook_secret"] != true || p["rotate_deploy_key"] != true {
		t.Errorf("params = %v", p)
	}
	if _, ok := p["tag_pattern"]; ok {
		t.Error("tag_pattern should be absent")
	}
}

func TestProjectsUpdateShowsRotatedSecret(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, func(r *rpc.Router) {
		r.Handle(stub(rec, "projects.update", `{"project":`+projectJSON+`,"webhook_secret":"n3w"}`))
	})
	out, _, err := runCLI(t, sock, "", "projects", "update", "app", "--rotate-webhook-secret")
	if err != nil || !strings.Contains(out, "n3w") || !strings.Contains(out, "shown only once") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestProjectsDelete(t *testing.T) {
	origTTY := stdinIsTerminal
	t.Cleanup(func() { stdinIsTerminal = origTTY })

	tests := []struct {
		name      string
		tty       bool
		stdin     string
		args      []string
		wantErr   string
		wantCalls int
		wantVol   bool
	}{
		{"yes", false, "", []string{"--yes"}, "", 1, false},
		{"yes with volumes", false, "", []string{"-y", "--volumes"}, "", 1, true},
		{"non-tty without yes", false, "app\n", nil, "--yes", 0, false},
		{"tty correct slug", true, "app\n", nil, "", 1, false},
		{"tty wrong slug", true, "other\n", nil, "slug did not match", 0, false},
		{"tty eof", true, "", nil, "no confirmation", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdinIsTerminal = func(io.Reader) bool { return tt.tty }
			rec := &calls{}
			sock := startAgent(t, projectsStubs(rec))
			out, errOut, err := runCLI(t, sock, tt.stdin, append([]string{"projects", "delete", "app"}, tt.args...)...)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := rec.count("projects.delete"); got != tt.wantCalls {
				t.Fatalf("delete calls = %d", got)
			}
			if tt.wantCalls == 1 {
				p := rec.last(t, "projects.delete")
				if (p["volumes"] == true) != tt.wantVol {
					t.Errorf("volumes = %v", p["volumes"])
				}
				if !strings.Contains(out, "3 containers") {
					t.Errorf("output = %q", out)
				}
			}
			if tt.tty && !strings.Contains(errOut, "Type the project slug") {
				t.Errorf("prompt missing: %q", errOut)
			}
		})
	}
}

func TestProjectsDeleteKeptVolumes(t *testing.T) {
	stubs := func(r *rpc.Router) {
		r.Handle(stub(&calls{}, "projects.delete", `{"containers":1,"networks":0,"volumes":0,"kept_volumes":["aaa","bbb"]}`))
	}
	sock := startAgent(t, stubs)
	out, _, err := runCLI(t, sock, "", "projects", "delete", "app", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Kept 2 anonymous volume(s) that may hold data:", "  aaa\n", "  bbb\n", "Remove them with: docker volume rm aaa bbb\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %q", want, out)
		}
	}

	out, _, err = runCLI(t, sock, "", "projects", "delete", "app", "--yes", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"kept_volumes"`) || strings.Contains(out, "docker volume rm") {
		t.Errorf("json output = %q", out)
	}
}

func TestRepositoryErrorHint(t *testing.T) {
	sock := startAgent(t, func(r *rpc.Router) {
		r.Handle("projects.create", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
			return nil, rpc.NewError(rpc.CodeRepositoryError, "repository error").
				WithData(map[string]string{"reason": "auth_failed", "detail": "Authentication failed"})
		})
		r.Handle("projects.get", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
			return nil, rpc.NewError(rpc.CodeNotFound, "project not found")
		})
	})
	_, _, err := runCLI(t, sock, "", "projects", "create", "https://github.com/acme/app.git")
	if err == nil || !strings.Contains(err.Error(), "Authentication failed") || !strings.Contains(err.Error(), "hint:") ||
		!strings.Contains(err.Error(), "--token-stdin") {
		t.Fatalf("err = %v", err)
	}
	_, _, err = runCLI(t, sock, "", "projects", "get", "nope")
	if err == nil || err.Error() != "project not found" {
		t.Fatalf("err = %v", err)
	}
}

func TestStatus(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, func(r *rpc.Router) {
		r.Handle("runtime.info", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
			return map[string]string{"name": "docker", "version": "27.0.1", "api_version": "1.46"}, nil
		})
		projectsStubs(rec)(r)
	})
	out, _, err := runCLI(t, sock, "", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Agent", "ki-agent", "Uptime", "Control plane", "disabled", "docker 27.0.1 (API 1.46)", "Projects"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "Projects       2") {
		t.Errorf("project count missing:\n%s", out)
	}

	out, _, err = runCLI(t, sock, "", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var js map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &js); err != nil || js["agent"] == nil || string(js["runtime"]) == "null" {
		t.Fatalf("json = %q %v", out, err)
	}
}

func TestStatusMissingMethods(t *testing.T) {
	sock := startAgent(t, nil) // no runtime.info, no projects.*
	out, _, err := runCLI(t, sock, "", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Engine         unavailable") || !strings.Contains(out, "not supported by this agent") {
		t.Errorf("output:\n%s", out)
	}
	out, _, err = runCLI(t, sock, "", "status", "--json")
	if err != nil || !strings.Contains(out, `"runtime": null`) || !strings.Contains(out, `"projects": null`) {
		t.Errorf("json = %q %v", out, err)
	}
}

// logsStub serves containers.logs.subscribe, sending the given updates
// (data JSON) with seq 1..n before the response.
func logsStub(rec *calls, updates []string) func(*rpc.Router) {
	return func(r *rpc.Router) {
		r.Handle("containers.logs.subscribe", func(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
			rec.add("containers.logs.subscribe", params)
			for i, u := range updates {
				_ = p.Notify(ctx, "containers.logs.update", rpc.Update{Subscription: "L1", Seq: uint64(i + 1), Data: json.RawMessage(u)})
			}
			return map[string]string{"subscription": "L1"}, nil
		})
		r.Handle("containers.logs.unsubscribe", func(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
			rec.add("containers.logs.unsubscribe", params)
			return struct{}{}, nil
		})
	}
}

func TestLogsUntilEOF(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, logsStub(rec, []string{
		`{"stream":"stdout","text":"one\n"}`,
		`{"stream":"stderr","text":"oops"}`,
		`{"stream":"stdout","time":"2026-01-02T03:04:05Z","text":"two"}`,
		`{"eof":true}`,
	}))
	out, errOut, err := runCLI(t, sock, "", "logs", "web-1", "--tail", "5", "--timestamps", "--since", "10m")
	if err != nil {
		t.Fatal(err)
	}
	if out != "one\n2026-01-02T03:04:05Z two\n" {
		t.Errorf("stdout = %q", out)
	}
	if errOut != "oops\n" {
		t.Errorf("stderr = %q", errOut)
	}
	p := rec.last(t, "containers.logs.subscribe")
	if p["id"] != "web-1" || p["tail"] != float64(5) || p["timestamps"] != true {
		t.Errorf("params = %v", p)
	}
	since, err := time.Parse(time.RFC3339Nano, p["since"].(string))
	if err != nil || time.Since(since) < 9*time.Minute || time.Since(since) > 11*time.Minute {
		t.Errorf("since = %v (%v)", p["since"], err)
	}
}

func TestLogsEOFErrorAndGapAndJSON(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, logsStub(rec, []string{
		`{"stream":"stdout","text":"a"}`,
		`{"eof":true,"error":"engine went away"}`,
	}))
	_, _, err := runCLI(t, sock, "", "logs", "c")
	if err == nil || !strings.Contains(err.Error(), "engine went away") {
		t.Fatalf("err = %v", err)
	}
	out, _, err := runCLI(t, sock, "", "logs", "c", "--json")
	if err == nil {
		t.Fatal("expected eof error")
	}
	if !strings.Contains(out, `{"stream":"stdout","text":"a"}`) {
		t.Errorf("json lines = %q", out)
	}
}

func TestLogsCtrlC(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, logsStub(rec, []string{`{"stream":"stdout","text":"hello"}`}))
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--socket", sock, "logs", "c"})
	ctx, cancel := context.WithCancel(context.Background())
	cmd.SetContext(ctx)
	errc := make(chan error, 1)
	go func() { errc <- cmd.Execute() }()
	deadline := time.Now().Add(5 * time.Second)
	for rec.count("containers.logs.subscribe") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("logs did not stop")
	}
	if rec.count("containers.logs.unsubscribe") != 1 {
		t.Error("expected an unsubscribe call")
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct{ in, want string }{
		{"30m", "2026-01-01T11:30:00Z"},
		{"2026-01-01T00:00:00+02:00", "2025-12-31T22:00:00Z"},
		{"bogus", ""},
		{"-5m", ""},
	}
	for _, tt := range tests {
		got, err := parseSince(tt.in, now)
		if (err != nil) != (tt.want == "") || got != tt.want {
			t.Errorf("parseSince(%q) = %q, %v", tt.in, got, err)
		}
	}
}

func TestResolveSocket(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("socket:\n  path: /from/config.sock\nunknown_key: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	def := config.Default().Socket.Path
	tests := []struct {
		name string
		f    flags
		env  string
		want string
	}{
		{"flag wins", flags{socket: "/flag.sock", configPath: cfgPath}, "/env.sock", "/flag.sock"},
		{"env beats config", flags{configPath: cfgPath}, "/env.sock", "/env.sock"},
		{"config", flags{configPath: cfgPath}, "", "/from/config.sock"},
		{"missing config", flags{configPath: filepath.Join(dir, "none.yaml")}, "", def},
		{"unreadable config dir", flags{configPath: dir}, "", def},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KI_SOCKET_PATH", tt.env)
			if got := resolveSocket(&tt.f); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConnectErrors(t *testing.T) {
	_, _, err := runCLI(t, filepath.Join(t.TempDir(), "none.sock"), "", "status")
	if err == nil || !strings.Contains(err.Error(), "cannot reach the agent") {
		t.Fatalf("err = %v", err)
	}
}

func TestConnectSocketPathTooLong(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("a", 110)+".sock")
	_, _, err := runCLI(t, long, "", "status")
	want := fmt.Sprintf("socket path is too long (%d bytes, max 107): %s", len(long), long)
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestEmptyTriggerView(t *testing.T) {
	var p project
	if err := json.Unmarshal([]byte(`{"slug":"x","repo_url":"git@github.com:a/x.git","trigger":{},
"auth":{"method":"ssh_key","public_key":"ssh-ed25519 K"},"status":"pending_access"}`), &p); err != nil {
		t.Fatal(err)
	}
	if got := p.Trigger.String(); !strings.Contains(got, "resolved on verify") {
		t.Errorf("trigger = %q", got)
	}
	var buf bytes.Buffer
	printProject(&buf, &p)
	if !strings.Contains(buf.String(), "default branch (resolved on verify)") {
		t.Errorf("project view = %q", buf.String())
	}
}

func TestRepositoryErrorDetailNotRepeated(t *testing.T) {
	err := explainError(rpc.NewError(rpc.CodeRepositoryError, "repository error: boom").
		WithData(map[string]string{"reason": "failed", "detail": "boom"}))
	if got := strings.Count(err.Error(), "boom"); got != 1 {
		t.Fatalf("detail repeated %d times: %v", got, err)
	}
}
