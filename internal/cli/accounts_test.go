package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-southeners/ki/internal/rpc"
)

const setupResult = `{"user":{"username":"bob","role":"viewer","status":"pending"},
"setup_url":"https://10.0.0.5/setup#TOKEN","fingerprint_sha256":"AA:BB:CC","expires_at":"2026-01-02T03:34:05Z"}`

const setupResultNoFP = `{"user":{"username":"admin","role":"admin","status":"pending"},
"setup_url":"https://10.0.0.5/setup#TOKEN","expires_at":"2026-01-02T03:34:05Z"}`

func accountsStubs(rec *calls) func(*rpc.Router) {
	return func(r *rpc.Router) {
		r.Handle(stub(rec, "users.list", `[{"username":"admin","role":"admin","status":"active","totp_enrolled":true,
"created_at":"2026-01-01T00:00:00Z","last_login_at":"2026-01-02T00:00:00Z"},
{"username":"bob","role":"viewer","status":"pending","totp_enrolled":false,"created_at":"2026-01-01T00:00:00Z"}]`))
		r.Handle(stub(rec, "users.create", setupResult))
		r.Handle(stub(rec, "users.reset", setupResult))
		r.Handle(stub(rec, "panel.setup", setupResultNoFP))
		r.Handle(stub(rec, "users.update", `{"username":"bob","role":"operator","status":"disabled","totp_enrolled":false,"created_at":"x"}`))
		r.Handle(stub(rec, "users.delete", `{}`))
		r.Handle(stub(rec, "tokens.create", `{"token":"ki_SECRET","name":"ci","role":"operator","expires_at":"2026-04-01T00:00:00Z"}`))
		r.Handle(stub(rec, "tokens.list", `[{"name":"ci","role":"operator","created_at":"2026-01-01T00:00:00Z","created_by":"admin"}]`))
		r.Handle(stub(rec, "tokens.revoke", `{}`))
		r.Handle(stub(rec, "audit.list", `[{"id":2,"time":"2026-01-02T03:04:05Z","principal_kind":"user","principal_name":"admin","ip":"10.0.0.9","action":"projects.delete","target":"app","result":"ok"},
{"id":1,"time":"2026-01-02T03:00:00Z","principal_kind":"local","principal_name":"","ip":"","action":"users.create","target":"bob","result":"ok"}]`))
	}
}

func mustContain(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q:\n%s", w, out)
		}
	}
}

func TestUsersList(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, accountsStubs(rec))
	out, _, err := runCLI(t, sock, "", "users", "list")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "USERNAME", "admin", "viewer", "pending", "2026-01-02T00:00:00Z")
	out, _, err = runCLI(t, sock, "", "users", "list", "--json")
	if err != nil || !json.Valid([]byte(out)) {
		t.Fatalf("json: %q %v", out, err)
	}
}

func TestUsersCreateAndReset(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, accountsStubs(rec))
	out, _, err := runCLI(t, sock, "", "users", "create", "bob", "--role", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "https://10.0.0.5/setup#TOKEN", "AA:BB:CC", "valid for 30 minutes", "share it only with bob")
	if p := rec.last(t, "users.create"); p["username"] != "bob" || p["role"] != "viewer" {
		t.Errorf("params = %v", p)
	}
	if _, _, err := runCLI(t, sock, "", "users", "create", "bob"); err == nil {
		t.Error("create without --role should fail")
	}
	out, _, err = runCLI(t, sock, "", "users", "reset", "bob")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "/setup#TOKEN", "share it only with bob")
	out, _, err = runCLI(t, sock, "", "users", "reset", "bob", "--json")
	if err != nil || !strings.Contains(out, `"setup_url"`) || strings.Contains(out, "valid for") {
		t.Fatalf("json: %q %v", out, err)
	}
}

func TestUsersUpdate(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, accountsStubs(rec))
	out, _, err := runCLI(t, sock, "", "users", "update", "bob", "--role", "operator", "--disable")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "User bob updated")
	p := rec.last(t, "users.update")
	if p["role"] != "operator" || p["disabled"] != true {
		t.Errorf("params = %v", p)
	}
	if _, _, err := runCLI(t, sock, "", "users", "update", "bob", "--enable"); err != nil {
		t.Fatal(err)
	}
	if p := rec.last(t, "users.update"); p["disabled"] != false {
		t.Errorf("params = %v", p)
	}
	if _, _, err := runCLI(t, sock, "", "users", "update", "bob"); err == nil || !strings.Contains(err.Error(), "nothing to update") {
		t.Errorf("err = %v", err)
	}
	if _, _, err := runCLI(t, sock, "", "users", "update", "bob", "--enable", "--disable"); err == nil {
		t.Error("--enable with --disable should fail")
	}
}

func TestUsersDelete(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, accountsStubs(rec))
	if _, _, err := runCLI(t, sock, "", "users", "delete", "bob"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("err = %v", err)
	}
	if rec.count("users.delete") != 0 {
		t.Fatal("deleted without confirmation")
	}
	out, _, err := runCLI(t, sock, "", "users", "delete", "bob", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "User bob deleted")
}

func TestPanelSetup(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, accountsStubs(rec))
	out, _, err := runCLI(t, sock, "", "panel", "setup")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "https://10.0.0.5/setup#TOKEN", "share it only with admin")
	if strings.Contains(out, "fingerprint") {
		t.Errorf("no fingerprint expected:\n%s", out)
	}
	if p := rec.last(t, "panel.setup"); len(p) != 0 {
		t.Errorf("params = %v", p)
	}
	if _, _, err := runCLI(t, sock, "", "panel", "setup", "--username", "root"); err != nil {
		t.Fatal(err)
	}
	if p := rec.last(t, "panel.setup"); p["username"] != "root" {
		t.Errorf("params = %v", p)
	}
}

func TestTokens(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, accountsStubs(rec))
	out, _, err := runCLI(t, sock, "", "tokens", "create", "ci", "--role", "operator", "--expires-in", "90d")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "ki_SECRET", "shown only once", "2026-04-01")
	if p := rec.last(t, "tokens.create"); p["name"] != "ci" || p["role"] != "operator" || p["expires_in"] != "2160h" {
		t.Errorf("params = %v", p)
	}
	if _, _, err := runCLI(t, sock, "", "tokens", "create", "ci", "--role", "viewer", "--expires-in", "36h"); err != nil {
		t.Fatal(err)
	}
	if p := rec.last(t, "tokens.create"); p["expires_in"] != "36h" {
		t.Errorf("params = %v", p)
	}
	if _, _, err := runCLI(t, sock, "", "tokens", "create", "ci", "--role", "viewer", "--expires-in", "soon"); err == nil {
		t.Error("bad duration should fail")
	}
	if _, _, err := runCLI(t, sock, "", "tokens", "create", "ci", "--role", "viewer"); err != nil {
		t.Fatal(err)
	}
	if p := rec.last(t, "tokens.create"); p["expires_in"] != nil {
		t.Errorf("expires_in should be absent: %v", p)
	}
	out, _, err = runCLI(t, sock, "", "tokens", "list")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "NAME", "ci", "operator")
	if strings.Contains(out, "ki_") {
		t.Error("list must not show tokens")
	}
	out, _, err = runCLI(t, sock, "", "tokens", "revoke", "ci")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "Token ci revoked")
}

func TestAudit(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, accountsStubs(rec))
	out, _, err := runCLI(t, sock, "", "audit", "--limit", "10")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "TIME", "PRINCIPAL", "IP", "ACTION", "TARGET", "RESULT", "projects.delete", "10.0.0.9", "local")
	if p := rec.last(t, "audit.list"); p["limit"] != float64(10) {
		t.Errorf("params = %v", p)
	}
	if _, _, err := runCLI(t, sock, "", "audit"); err != nil {
		t.Fatal(err)
	}
	if p := rec.last(t, "audit.list"); p["limit"] != nil {
		t.Errorf("limit should be absent: %v", p)
	}
}

func TestDurationParam(t *testing.T) {
	for in, want := range map[string]string{"90d": "2160h", "1d": "24h", "36h": "36h", "30m": "30m"} {
		if got, err := durationParam(in); err != nil || got != want {
			t.Errorf("durationParam(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0d", "-1d", "xd", "-5h", "1w"} {
		if _, err := durationParam(in); err == nil {
			t.Errorf("durationParam(%q) should fail", in)
		}
	}
}
