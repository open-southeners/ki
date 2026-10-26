package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/open-southeners/ki-agent/internal/rpc"
)

const deployQueuedJSON = `{"id":42,"project":"app","ref":"refs/heads/main","commit":"","source":"cli",
"status":"queued","stage":"","error":null,"created_at":"2026-01-02T03:04:05Z","started_at":null,"finished_at":null}`

const deploySucceededJSON = `{"id":42,"project":"app","ref":"refs/heads/main","commit":"1a2b3c4d5e6f7a8b9c0d",
"source":"cli","status":"succeeded","stage":"health","error":null,"created_at":"2026-01-02T03:04:05Z",
"started_at":"2026-01-02T03:04:06Z","finished_at":"2026-01-02T03:05:00Z"}`

const deployPolicyJSON = `{"id":43,"project":"app","ref":"refs/heads/main","commit":"1a2b3c4d5e6f7a8b9c0d",
"source":"webhook","status":"failed","stage":"policy","error":{"reason":"policy_violation","detail":"2 violations",
"violations":[{"service":"web","rule":"privileged","detail":"privileged mode is not allowed"},
{"service":"","rule":"host_path","detail":"/etc is outside the project"}]},
"created_at":"2026-01-02T03:04:05Z","started_at":"2026-01-02T03:04:06Z","finished_at":"2026-01-02T03:04:09Z"}`

// deployStubs serves deploys.trigger, deploys.get/list and deploys.subscribe,
// which sends updates with seq 1..n before the response.
func deployStubs(rec *calls, trigger string, updates []string) func(*rpc.Router) {
	return func(r *rpc.Router) {
		r.Handle(stub(rec, "deploys.trigger", trigger))
		r.Handle(stub(rec, "deploys.get", deployPolicyJSON))
		r.Handle(stub(rec, "deploys.list", `[`+deploySucceededJSON+`,`+deployPolicyJSON+`]`))
		r.Handle("deploys.subscribe", func(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
			rec.add("deploys.subscribe", params)
			for i, u := range updates {
				_ = p.Notify(ctx, "deploys.update", rpc.Update{Subscription: "D1", Seq: uint64(i + 1), Data: json.RawMessage(u)})
			}
			return map[string]string{"subscription": "D1"}, nil
		})
		r.Handle("deploys.unsubscribe", func(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
			rec.add("deploys.unsubscribe", params)
			return struct{}{}, nil
		})
	}
}

func statusUpdate(d string) string { return `{"type":"status","deploy":` + d + `}` }

func TestDeployFollowsAndSucceeds(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, deployStubs(rec, deployQueuedJSON, []string{
		`{"type":"log","stream":"agent","text":"fetch"}`,
		`{"type":"log","stream":"stdout","text":"building\n"}`,
		`{"type":"log","stream":"stderr","text":"warn"}`,
		statusUpdate(deploySucceededJSON),
	}))
	out, errOut, err := runCLI(t, sock, "", "deploy", "--project", "app", "--ref", "main")
	if err != nil {
		t.Fatal(err)
	}
	want := "Deploy 42 queued for project app (ref refs/heads/main)\n==> fetch\nbuilding\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if errOut != "warn\n" {
		t.Errorf("stderr = %q", errOut)
	}
	if p := rec.last(t, "deploys.trigger"); p["project"] != "app" || p["ref"] != "main" {
		t.Errorf("trigger params = %v", p)
	}
	if p := rec.last(t, "deploys.subscribe"); p["id"] != float64(42) {
		t.Errorf("subscribe params = %v", p)
	}
}

func TestDeployNoRefOmitsParam(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, deployStubs(rec, deployQueuedJSON, nil))
	if _, _, err := runCLI(t, sock, "", "deploy", "--project", "app", "--detach"); err != nil {
		t.Fatal(err)
	}
	if p := rec.last(t, "deploys.trigger"); len(p) != 1 {
		t.Errorf("params = %v", p)
	}
	if rec.count("deploys.subscribe") != 0 {
		t.Error("--detach must not subscribe")
	}
}

func TestDeployDetach(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, deployStubs(rec, deployQueuedJSON, nil))
	out, _, err := runCLI(t, sock, "", "deploy", "--project", "app", "--detach")
	if err != nil {
		t.Fatal(err)
	}
	if out != "Deploy 42 queued for project app (ref refs/heads/main)\n" {
		t.Errorf("stdout = %q", out)
	}
	out, _, err = runCLI(t, sock, "", "deploy", "--project", "app", "--detach", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"id": 42`) {
		t.Errorf("json = %q", out)
	}
}

func rollbackStubs(rec *calls, result string, updates []string) func(*rpc.Router) {
	base := deployStubs(rec, deployQueuedJSON, updates)
	return func(r *rpc.Router) {
		base(r)
		r.Handle(stub(rec, "deploys.rollback", result))
	}
}

func TestDeploysRollbackFollows(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, rollbackStubs(rec, deployQueuedJSON, []string{
		`{"type":"log","stream":"agent","text":"fetch"}`,
		statusUpdate(deploySucceededJSON),
	}))
	out, _, err := runCLI(t, sock, "", "deploys", "rollback", "app", "--to", "7")
	if err != nil {
		t.Fatal(err)
	}
	want := "Deploy 42 queued for project app (ref refs/heads/main)\n==> fetch\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if p := rec.last(t, "deploys.rollback"); p["project"] != "app" || p["deploy"] != float64(7) {
		t.Errorf("rollback params = %v", p)
	}
	if p := rec.last(t, "deploys.subscribe"); p["id"] != float64(42) {
		t.Errorf("subscribe params = %v", p)
	}
}

func TestDeploysRollbackDetachAndDefaultTarget(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, rollbackStubs(rec, deployQueuedJSON, nil))
	out, _, err := runCLI(t, sock, "", "deploys", "rollback", "app", "--detach")
	if err != nil {
		t.Fatal(err)
	}
	if out != "Deploy 42 queued for project app (ref refs/heads/main)\n" {
		t.Errorf("stdout = %q", out)
	}
	if p := rec.last(t, "deploys.rollback"); len(p) != 1 {
		t.Errorf("params = %v, want only the project", p)
	}
	if rec.count("deploys.subscribe") != 0 {
		t.Error("--detach must not subscribe")
	}
	out, _, err = runCLI(t, sock, "", "deploys", "rollback", "app", "--detach", "--json")
	if err != nil || !strings.Contains(out, `"id": 42`) {
		t.Errorf("json = %q, %v", out, err)
	}
}

func TestDeploysRollbackFailsWhenDeployFails(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, rollbackStubs(rec, deployQueuedJSON, []string{statusUpdate(deployPolicyJSON)}))
	if _, _, err := runCLI(t, sock, "", "deploys", "rollback", "app"); err == nil {
		t.Fatal("expected error for a failed deploy")
	}
}

func TestDeploysRollbackArgs(t *testing.T) {
	sock := startAgent(t, nil)
	if _, _, err := runCLI(t, sock, "", "deploys", "rollback"); err == nil {
		t.Fatal("expected error without slug")
	}
	if _, _, err := runCLI(t, sock, "", "deploys", "rollback", "app", "--to", "0"); err == nil {
		t.Fatal("expected error for --to 0")
	}
}

func TestDeployRequiresProject(t *testing.T) {
	sock := startAgent(t, nil)
	if _, _, err := runCLI(t, sock, "", "deploy"); err == nil {
		t.Fatal("expected error without --project")
	}
}

func TestDeployPolicyFailure(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, deployStubs(rec, deployQueuedJSON, []string{
		`{"type":"log","stream":"agent","text":"policy"}`,
		statusUpdate(deployPolicyJSON),
	}))
	_, _, err := runCLI(t, sock, "", "deploy", "--project", "app")
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()
	for _, want := range []string{
		"deploy 43 failed at stage policy: policy_violation: 2 violations",
		"SERVICE", "web", "privileged", "privileged mode is not allowed", "host_path",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	if !strings.Contains(msg, "-  ") {
		t.Errorf("project-level violation should show - as service: %q", msg)
	}
}

func TestDeployInterruptedFails(t *testing.T) {
	rec := &calls{}
	d := strings.Replace(deploySucceededJSON, `"succeeded"`, `"interrupted"`, 1)
	sock := startAgent(t, deployStubs(rec, deployQueuedJSON, []string{statusUpdate(d)}))
	_, _, err := runCLI(t, sock, "", "deploy", "--project", "app")
	if err == nil || err.Error() != "deploy 42 interrupted" {
		t.Fatalf("err = %v", err)
	}
}

func TestDeployJSONStream(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, deployStubs(rec, deployQueuedJSON, []string{
		`{"type":"log","stream":"stdout","text":"x"}`,
		statusUpdate(deploySucceededJSON),
	}))
	out, _, err := runCLI(t, sock, "", "deploy", "--project", "app", "--json")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || lines[0] != `{"type":"log","stream":"stdout","text":"x"}` {
		t.Errorf("stdout = %q", out)
	}
}

func TestDeployErrorHint(t *testing.T) {
	sock := startAgent(t, func(r *rpc.Router) {
		r.Handle("deploys.trigger", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
			return nil, rpc.NewError(-32012, "deploy rejected").WithData(map[string]string{
				"reason": "ref_not_allowed", "detail": "branch dev is not the trigger",
			})
		})
	})
	_, _, err := runCLI(t, sock, "", "deploy", "--project", "app", "--ref", "dev")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"deploy rejected: branch dev is not the trigger", "hint:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestDeploysList(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, deployStubs(rec, deployQueuedJSON, nil))
	out, _, err := runCLI(t, sock, "", "deploys", "list", "app", "--limit", "5")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ID", "STATUS", "42", "succeeded", "1a2b3c4d5e6f", "webhook", "failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "1a2b3c4d5e6f7") {
		t.Errorf("commit not shortened:\n%s", out)
	}
	if p := rec.last(t, "deploys.list"); p["project"] != "app" || p["limit"] != float64(5) {
		t.Errorf("params = %v", p)
	}
	if _, _, err := runCLI(t, sock, "", "deploys", "list", "app"); err != nil {
		t.Fatal(err)
	}
	if p := rec.last(t, "deploys.list"); p["limit"] != nil {
		t.Errorf("limit sent without flag: %v", p)
	}
	out, _, err = runCLI(t, sock, "", "deploys", "list", "app", "--json")
	if err != nil || !strings.HasPrefix(out, "[\n") {
		t.Errorf("json = %q, %v", out, err)
	}
}

func TestDeploysGet(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, deployStubs(rec, deployQueuedJSON, nil))
	out, _, err := runCLI(t, sock, "", "deploys", "get", "43")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"policy_violation: 2 violations", "Policy violations:", "privileged mode is not allowed", "Finished"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if p := rec.last(t, "deploys.get"); p["id"] != float64(43) {
		t.Errorf("params = %v", p)
	}
	if _, _, err := runCLI(t, sock, "", "deploys", "get", "abc"); err == nil {
		t.Error("expected invalid id error")
	}
}

func TestDeploysLogsFinished(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, deployStubs(rec, deployQueuedJSON, []string{
		`{"type":"log","stream":"agent","text":"fetch"}`,
		statusUpdate(deploySucceededJSON),
	}))
	out, _, err := runCLI(t, sock, "", "deploys", "logs", "42")
	if err != nil {
		t.Fatal(err)
	}
	if out != "==> fetch\n" {
		t.Errorf("stdout = %q", out)
	}
}

func TestDeploysLogsUnfinishedWithoutFollowStops(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, deployStubs(rec, deployQueuedJSON, []string{
		`{"type":"log","stream":"stdout","text":"so far"}`,
	}))
	out, _, err := runCLI(t, sock, "", "deploys", "logs", "42")
	if err != nil {
		t.Fatal(err)
	}
	if out != "so far\n" {
		t.Errorf("stdout = %q", out)
	}
	if rec.count("deploys.unsubscribe") != 1 {
		t.Error("expected an unsubscribe call")
	}
}

func TestDeploysLogsFailedExitsNonZero(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, deployStubs(rec, deployQueuedJSON, []string{statusUpdate(deployPolicyJSON)}))
	if _, _, err := runCLI(t, sock, "", "deploys", "logs", "43", "--follow"); err == nil {
		t.Fatal("expected failure exit")
	}
}

func envStubs(rec *calls) func(*rpc.Router) {
	return func(r *rpc.Router) {
		r.Handle(stub(rec, "projects.env.set", `{"names":["A","B"]}`))
		r.Handle(stub(rec, "projects.env.unset", `{"names":["A"]}`))
		r.Handle(stub(rec, "projects.env.list", `{"vars":[{"name":"A","updated_at":"2026-01-02T03:04:05Z"},{"name":"B","updated_at":"2026-01-03T03:04:05Z"}]}`))
	}
}

func TestEnvList(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, envStubs(rec))
	out, _, err := runCLI(t, sock, "", "projects", "env", "list", "app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "B     2026-01-03T03:04:05Z") {
		t.Errorf("output:\n%s", out)
	}
	if p := rec.last(t, "projects.env.list"); p["slug"] != "app" {
		t.Errorf("params = %v", p)
	}
}

func TestEnvSetStdinAndFile(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, envStubs(rec))
	out, _, err := runCLI(t, sock, "s3cr3t value\n", "projects", "env", "set", "app", "TOKEN", "--value-stdin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Set 1 variable(s)") {
		t.Errorf("stdout = %q", out)
	}
	p := rec.last(t, "projects.env.set")
	if !reflect.DeepEqual(p["vars"], map[string]any{"TOKEN": "s3cr3t value"}) || p["slug"] != "app" {
		t.Errorf("params = %v", p)
	}

	file := filepath.Join(t.TempDir(), "v")
	if err := os.WriteFile(file, []byte("line1\nline2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, sock, "", "projects", "env", "set", "app", "MULTI", "--value-file", file); err != nil {
		t.Fatal(err)
	}
	p = rec.last(t, "projects.env.set")
	if !reflect.DeepEqual(p["vars"], map[string]any{"MULTI": "line1\nline2"}) {
		t.Errorf("params = %v", p)
	}
}

func TestEnvSetNeedsValueSource(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, envStubs(rec))
	if _, _, err := runCLI(t, sock, "", "projects", "env", "set", "app", "X"); err == nil {
		t.Error("expected error without a value source")
	}
	if _, _, err := runCLI(t, sock, "\n", "projects", "env", "set", "app", "X", "--value-stdin"); err == nil {
		t.Error("expected error for empty value")
	}
	if _, _, err := runCLI(t, sock, "v", "projects", "env", "set", "app", "X", "--value-stdin", "--value-file", "f"); err == nil {
		t.Error("expected exclusive flags error")
	}
	if rec.count("projects.env.set") != 0 {
		t.Error("nothing should reach the agent")
	}
}

func TestEnvImportAndUnset(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, envStubs(rec))
	file := filepath.Join(t.TempDir(), ".env")
	content := "# comment\n\nA=1\nexport B=\"two words\\nnext\"\nC='lit $x'\nD=plain # trailing\nE=\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, sock, "", "projects", "env", "import", "app", "--file", file); err != nil {
		t.Fatal(err)
	}
	if rec.count("projects.env.set") != 1 {
		t.Fatal("import must be one call")
	}
	want := map[string]any{"A": "1", "B": "two words\nnext", "C": "lit $x", "D": "plain", "E": ""}
	if p := rec.last(t, "projects.env.set"); !reflect.DeepEqual(p["vars"], want) {
		t.Errorf("vars = %v", p["vars"])
	}
	if _, _, err := runCLI(t, sock, "X=1\n", "projects", "env", "import", "app", "--file", "-"); err != nil {
		t.Fatal(err)
	}

	out, _, err := runCLI(t, sock, "", "projects", "env", "unset", "app", "A", "B")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Unset 2 variable(s)") {
		t.Errorf("stdout = %q", out)
	}
	if p := rec.last(t, "projects.env.unset"); !reflect.DeepEqual(p["names"], []any{"A", "B"}) {
		t.Errorf("params = %v", p)
	}
}

func TestParseDotenvErrors(t *testing.T) {
	for _, in := range []string{"novalue\n", "=x\n", "A=\"open\n", "A='open\n"} {
		_, err := parseDotenv(strings.NewReader(in))
		if err == nil {
			t.Errorf("parseDotenv(%q) should fail", in)
		} else if strings.Contains(err.Error(), "open") {
			t.Errorf("error echoes the value: %v", err)
		}
	}
}
