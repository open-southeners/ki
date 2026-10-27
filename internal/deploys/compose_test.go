package deploys

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/open-southeners/ki-agent/internal/policy"
	"github.com/open-southeners/ki-agent/internal/projects"
	"github.com/open-southeners/ki-agent/internal/store"
)

const cmpSlug = "cmp"

// composeFixture is a fixture with a compose project "cmp" holding one version.
func composeFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, fixtureOpts{})
	ctx := context.Background()
	p := &store.Project{Slug: cmpSlug, ComposeFile: "compose.yaml", AuthMethod: "none", WebhookID: "cmpwebhook",
		Status: projects.StatusReady, Source: "compose"}
	if err := f.st.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	f.addVersion(t, okCompose)
	return f
}

func (f *fixture) addVersion(t *testing.T, content string) int {
	t.Helper()
	v, err := f.st.AddComposeVersion(context.Background(), cmpSlug, content, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (f *fixture) cmpTrigger(ref string) (*Deploy, error) {
	return f.mgr.Trigger(context.Background(), TriggerRequest{Project: cmpSlug, Ref: ref})
}

func (f *fixture) cmpDeploy(t *testing.T, ref string) Deploy {
	t.Helper()
	d, err := f.cmpTrigger(ref)
	if err != nil {
		t.Fatalf("Trigger(%q) = %v", ref, err)
	}
	return f.wait(t, d.ID)
}

func (f *fixture) cmpFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.data, "projects", cmpSlug, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestComposeTriggerRefs(t *testing.T) {
	f := composeFixture(t)
	f.addVersion(t, okCompose+"# v2\n")

	for _, ref := range []string{"", "compose@v2", "v2", "2"} {
		d, err := f.cmpTrigger(ref)
		if err != nil {
			t.Fatalf("Trigger(%q) = %v", ref, err)
		}
		if d.Ref != "compose@v2" {
			t.Errorf("Trigger(%q) ref = %q", ref, d.Ref)
		}
		got := f.wait(t, d.ID)
		if got.Status != StatusSucceeded {
			t.Fatalf("Trigger(%q) deploy = %+v (error %+v)", ref, got, got.Error)
		}
		row, err := f.st.GetDeploy(context.Background(), d.ID)
		if err != nil || row.ComposeVersion != 2 || row.Commit != "" || row.Ref != "compose@v2" {
			t.Fatalf("stored = %+v, %v", row, err)
		}
	}
	d := f.cmpDeploy(t, "v1")
	if d.Ref != "compose@v1" || d.Status != StatusSucceeded {
		t.Fatalf("v1 deploy = %+v", d)
	}

	for _, ref := range []string{"v9", "9"} {
		if _, err := f.cmpTrigger(ref); triggerReason(err) != ReasonRefNotFound {
			t.Errorf("Trigger(%q) err = %v", ref, err)
		}
	}
	for _, ref := range []string{"main", "refs/heads/main", "v", "v0", "v01", "-1", "compose@", "x2", "v1.0"} {
		if _, err := f.cmpTrigger(ref); triggerReason(err) != ReasonRefNotAllowed {
			t.Errorf("Trigger(%q) err = %v", ref, err)
		}
	}
	if _, err := f.mgr.Trigger(context.Background(),
		TriggerRequest{Project: cmpSlug, Commit: "0123456789012345678901234567890123456789"}); triggerReason(err) != ReasonRefNotAllowed {
		t.Errorf("commit err = %v", err)
	}
}

func TestComposeBuildRejectedByPolicy(t *testing.T) {
	f := composeFixture(t)
	f.comp.model = `{"services":{"web":{"image":"x","build":{"context":"."}}}}`
	d := f.cmpDeploy(t, "v1")
	if d.Status != StatusFailed || d.Error == nil || d.Error.Reason != ReasonPolicyViolation || d.Stage != StagePolicy {
		t.Fatalf("deploy = %+v (error %+v)", d, d.Error)
	}
	if len(d.Error.Violations) != 1 || d.Error.Violations[0].Rule != policy.RuleBuildNotAllowed {
		t.Errorf("violations = %+v, want build_not_allowed", d.Error.Violations)
	}
	if calls := f.comp.Calls(); slices.Contains(calls, "up cmp") || slices.Contains(calls, "build cmp") {
		t.Errorf("compose calls = %v", calls)
	}
}

func TestComposeTriggerNoVersion(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	p := &store.Project{Slug: cmpSlug, ComposeFile: "compose.yaml", AuthMethod: "none", WebhookID: "w",
		Status: projects.StatusReady, Source: "compose"}
	if err := f.st.CreateProject(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cmpTrigger(""); triggerReason(err) != ReasonRefNotFound {
		t.Fatalf("err = %v", err)
	}
}

func TestComposePrepareWritesVersionAndEnv(t *testing.T) {
	f := composeFixture(t)
	v2 := okCompose + "# v2\n"
	f.addVersion(t, v2)

	d := f.cmpDeploy(t, "v1")
	if d.Status != StatusSucceeded {
		t.Fatalf("deploy = %+v (error %+v)", d, d.Error)
	}
	if got := f.cmpFile(t, "compose.yaml"); got != okCompose {
		t.Errorf("compose.yaml = %q", got)
	}
	fi, err := os.Stat(filepath.Join(f.data, "projects", cmpSlug, "compose.yaml"))
	if err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("compose.yaml mode = %v, %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(f.data, "projects", cmpSlug, ".env")); !os.IsNotExist(err) {
		t.Errorf(".env without variables: %v", err)
	}
	// The pipeline: no fetch, no build; pull, up run.
	if got, want := f.comp.Calls(), []string{"config cmp", "pull cmp", "up cmp"}; !slices.Equal(got, want) {
		t.Errorf("compose calls = %v, want %v", got, want)
	}

	if _, err := f.svc.SetEnv(context.Background(), cmpSlug, map[string]string{"X": "y"}); err != nil {
		t.Fatal(err)
	}
	if d := f.cmpDeploy(t, ""); d.Status != StatusSucceeded {
		t.Fatalf("deploy = %+v (error %+v)", d, d.Error)
	}
	if got := f.cmpFile(t, "compose.yaml"); got != v2 {
		t.Errorf("compose.yaml = %q", got)
	}
	if got := f.cmpFile(t, ".env"); got != "X='y'\n" {
		t.Errorf(".env = %q", got)
	}
	// Variables removed: the stale .env goes away.
	if _, err := f.svc.UnsetEnv(context.Background(), cmpSlug, []string{"X"}); err != nil {
		t.Fatal(err)
	}
	f.cmpDeploy(t, "")
	if _, err := os.Stat(filepath.Join(f.data, "projects", cmpSlug, ".env")); !os.IsNotExist(err) {
		t.Errorf("stale .env: %v", err)
	}

	// The stage is recorded and the project commit is not touched.
	row, err := f.st.GetProject(context.Background(), cmpSlug)
	if err != nil || row.Commit != "" {
		t.Errorf("project = %+v, %v", row, err)
	}
}

func TestComposePrepareEnvSymlinkRejected(t *testing.T) {
	f := composeFixture(t)
	f.cmpDeploy(t, "") // creates the directory
	secret := filepath.Join(t.TempDir(), "other.env")
	if err := os.WriteFile(secret, []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(f.data, "projects", cmpSlug, ".env")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetEnv(context.Background(), cmpSlug, map[string]string{"X": "y"}); err != nil {
		t.Fatal(err)
	}
	before := len(f.comp.Calls())
	got := f.cmpDeploy(t, "")
	if got.Status != StatusFailed || got.Error == nil || got.Error.Detail != "'.env' must be a regular file" {
		t.Fatalf("deploy = %+v (error %+v)", got, got.Error)
	}
	if len(f.comp.Calls()) != before {
		t.Errorf("compose ran: %v", f.comp.Calls())
	}
	if b, _ := os.ReadFile(secret); string(b) != "SECRET=1\n" {
		t.Errorf("target modified: %q", b)
	}
}

func TestComposeConfigFailureStage(t *testing.T) {
	f := composeFixture(t)
	f.comp.cfgErr = errors.New("boom")
	d := f.cmpDeploy(t, "")
	if d.Status != StatusFailed || d.Stage != StageConfig {
		t.Fatalf("deploy = %+v", d)
	}
}

func TestComposeRollback(t *testing.T) {
	f := composeFixture(t)
	ctx := context.Background()
	f.addVersion(t, okCompose+"# v2\n")
	f.addVersion(t, okCompose+"# v3\n")
	d1 := f.cmpDeploy(t, "v1")
	d2 := f.cmpDeploy(t, "v2")
	d3 := f.cmpDeploy(t, "v3")
	for _, d := range []Deploy{d1, d2, d3} {
		if d.Status != StatusSucceeded {
			t.Fatalf("deploy = %+v (error %+v)", d, d.Error)
		}
	}

	// Default: the previous successful deploy (v2), skipping the newest.
	rb, err := f.mgr.Rollback(ctx, RollbackRequest{Project: cmpSlug})
	if err != nil {
		t.Fatal(err)
	}
	if rb.RollbackOf != d2.ID || rb.Ref != "compose@v2" {
		t.Fatalf("rollback = %+v", rb)
	}
	if got := f.wait(t, rb.ID); got.Status != StatusSucceeded || got.RollbackOf != d2.ID {
		t.Fatalf("rollback deploy = %+v", got)
	}
	if got := f.cmpFile(t, "compose.yaml"); got != okCompose+"# v2\n" {
		t.Errorf("compose.yaml = %q", got)
	}
	if row, _ := f.st.GetDeploy(ctx, rb.ID); row.ComposeVersion != 2 {
		t.Errorf("stored compose version = %d", row.ComposeVersion)
	}

	// Explicit id.
	rb, err = f.mgr.Rollback(ctx, RollbackRequest{Project: cmpSlug, Deploy: d1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.wait(t, rb.ID); got.Ref != "compose@v1" || got.Status != StatusSucceeded {
		t.Fatalf("explicit rollback = %+v", got)
	}
	if got := f.cmpFile(t, "compose.yaml"); got != okCompose {
		t.Errorf("compose.yaml = %q", got)
	}
}

func TestComposeRollbackPrunedVersion(t *testing.T) {
	f := composeFixture(t)
	ctx := context.Background()
	f.addVersion(t, okCompose+"# v2\n")
	d1 := f.cmpDeploy(t, "v1")
	f.cmpDeploy(t, "v2")
	if n, err := f.st.PruneComposeVersions(ctx, cmpSlug, 1, 2); err != nil || n != 1 {
		t.Fatalf("prune = %d, %v", n, err)
	}
	_, err := f.mgr.Rollback(ctx, RollbackRequest{Project: cmpSlug, Deploy: d1.ID})
	var te *TriggerError
	if triggerReason(err) != ReasonNoRollbackTarget || !errors.As(err, &te) || te.Detail == "" {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.mgr.Rollback(ctx, RollbackRequest{Project: cmpSlug}); triggerReason(err) != ReasonNoRollbackTarget {
		t.Fatalf("default err = %v", err)
	}
}

func TestGitProjectIgnoresComposeRefs(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := f.deployOK(t)
	row, err := f.st.GetDeploy(context.Background(), d.ID)
	if err != nil || row.ComposeVersion != 0 || row.Commit == "" {
		t.Fatalf("stored = %+v, %v", row, err)
	}
	if _, err := f.mgr.Trigger(context.Background(), TriggerRequest{Project: "app", Ref: "v1"}); triggerReason(err) != ReasonRefNotFound {
		// "v1" is not a git ref here.
		t.Errorf("err = %v", err)
	}
	if got := f.comp.Calls(); !slices.Contains(got, "build app") {
		t.Errorf("git deploy did not build: %v", got)
	}
}
