package deploys

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/compose"
	"github.com/open-southeners/ki-agent/internal/runtime"
	"github.com/open-southeners/ki-agent/internal/store"
)

func TestSuccessfulDeploy(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	ctx := context.Background()
	if _, err := f.svc.SetEnv(ctx, "app", map[string]string{"TOKEN": "s3cret value"}); err != nil {
		t.Fatal(err)
	}
	d := f.trigger(t, "")
	if d.Status != StatusRunning || d.Ref != "refs/heads/main" || d.Source != SourceAPI || d.Commit != "" {
		t.Fatalf("triggered = %+v", d)
	}
	got := f.wait(t, d.ID)
	want := f.repo.rev("main")
	if got.Status != StatusSucceeded || got.Stage != StageHealth || got.Commit != want ||
		got.Error != nil || got.StartedAt == nil || got.FinishedAt == nil {
		t.Fatalf("deploy = %+v", got)
	}
	p, err := f.svc.Get(ctx, "app")
	if err != nil || p.Commit != want {
		t.Fatalf("project commit = %q, %v; want %s", p.Commit, err, want)
	}

	// The labelled model is what ran, with restrictive permissions.
	model := filepath.Join(f.data, "deploys", "app", "compose.json")
	st, err := os.Stat(model)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("model stat = %v, %v", st, err)
	}
	if ds, _ := os.Stat(filepath.Dir(model)); ds.Mode().Perm() != 0o700 {
		t.Errorf("model dir mode = %v", ds.Mode().Perm())
	}
	b, _ := os.ReadFile(model)
	if !strings.Contains(string(b), `"ki.project": "app"`) && !strings.Contains(string(b), `"ki.project":"app"`) {
		t.Errorf("model not labelled: %s", b)
	}
	if files := f.comp.files; len(files) != 1 || files[0] != model {
		t.Errorf("build files = %v, want [%s]", files, model)
	}
	if want := []string{"config app", "pull app", "build app", "up app"}; strings.Join(f.comp.Calls(), ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v", f.comp.Calls())
	}

	// .env is written with 0600, and removed once the project has no vars.
	envPath := filepath.Join(f.data, "projects", "app", ".env")
	if st, err := os.Stat(envPath); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf(".env stat = %v, %v", st, err)
	}
	if _, err := f.svc.UnsetEnv(ctx, "app", []string{"TOKEN"}); err != nil {
		t.Fatal(err)
	}
	f.wait(t, f.trigger(t, "").ID)
	if _, err := os.Stat(envPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".env still there: %v", err)
	}

	// Stored log: ordered, includes compose output and agent stage lines.
	logs, err := f.st.DeployLogs(ctx, d.ID, 0)
	if err != nil || len(logs) < 4 {
		t.Fatalf("logs = %v, %v", logs, err)
	}
	var streams []string
	for i, l := range logs {
		if l.Seq != int64(i+1) {
			t.Errorf("log %d seq = %d", i, l.Seq)
		}
		streams = append(streams, l.Stream+":"+l.Text)
	}
	joined := strings.Join(streams, "\n")
	for _, w := range []string{"stdout:building app", "stderr:starting app", "agent:deploy succeeded"} {
		if !strings.Contains(joined, w) {
			t.Errorf("log lacks %q:\n%s", w, joined)
		}
	}
}

func TestQueueAndSupersede(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.comp.block = make(chan struct{})
	f.comp.entered = make(chan string, 8)

	a := f.trigger(t, "")
	if a.Status != StatusRunning {
		t.Fatalf("a = %+v", a)
	}
	<-f.comp.entered
	b := f.trigger(t, "")
	c := f.trigger(t, "")
	if b.Status != StatusQueued || c.Status != StatusQueued {
		t.Fatalf("b, c = %s, %s", b.Status, c.Status)
	}
	bg := f.wait(t, b.ID)
	if bg.Status != StatusSuperseded || bg.FinishedAt == nil {
		t.Errorf("b = %+v, want superseded", bg)
	}
	// The running deploy is never cancelled by a trigger.
	if d, _ := f.mgr.Get(context.Background(), a.ID); d.Status != StatusRunning {
		t.Errorf("a = %s", d.Status)
	}
	close(f.comp.block)
	if got := f.wait(t, a.ID); got.Status != StatusSucceeded {
		t.Errorf("a = %+v", got)
	}
	if got := f.wait(t, c.ID); got.Status != StatusSucceeded {
		t.Errorf("c = %+v", got)
	}
	if n := len(f.comp.Calls()); n != 8 { // a and c: config, pull, build, up
		t.Errorf("calls = %v", f.comp.Calls())
	}
}

func TestRefRulesBranch(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mainSHA, devSHA := f.repo.rev("main"), f.repo.rev("dev")
	tests := []struct {
		name, ref  string
		wantErr    string
		wantRef    string
		wantStatus string
	}{
		{"default", "", "", "refs/heads/main", StatusSucceeded},
		{"bare", "main", "", "refs/heads/main", StatusSucceeded},
		{"full", "refs/heads/main", "", "refs/heads/main", StatusSucceeded},
		{"sha on branch", mainSHA, "", mainSHA, StatusSucceeded},
		{"sha upper case", strings.ToUpper(mainSHA), "", mainSHA, StatusSucceeded},
		{"other branch bare", "dev", ReasonRefNotAllowed, "", ""},
		{"other branch full", "refs/heads/dev", ReasonRefNotAllowed, "", ""},
		{"tag bare", "v1.0.0", ReasonRefNotAllowed, "", ""},
		{"tag full", "refs/tags/v1.0.0", ReasonRefNotAllowed, "", ""},
		{"missing", "nope", ReasonRefNotFound, "", ""},
		{"invalid", "--oops", ReasonRefNotFound, "", ""},
		// Reachability is enforced by the checkout, at run time.
		{"sha not on branch", devSHA, "", devSHA, StatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := f.mgr.Trigger(context.Background(), TriggerRequest{Project: "app", Ref: tt.ref})
			if tt.wantErr != "" {
				if got := triggerReason(err); got != tt.wantErr {
					t.Fatalf("err = %v, want reason %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.Ref != tt.wantRef {
				t.Errorf("ref = %q, want %q", d.Ref, tt.wantRef)
			}
			got := f.wait(t, d.ID)
			if got.Status != tt.wantStatus {
				t.Errorf("deploy = %+v, want %s", got, tt.wantStatus)
			}
			if tt.wantStatus == StatusFailed && (got.Error == nil || got.Error.Reason != ReasonFetchFailed) {
				t.Errorf("error = %+v, want fetch_failed", got.Error)
			}
		})
	}
	t.Run("webhook commit pin", func(t *testing.T) {
		d, err := f.mgr.Trigger(context.Background(), TriggerRequest{
			Project: "app", Ref: "refs/heads/main", Commit: f.repo.rev("v1.0.0"), Source: SourceWebhook})
		if err != nil {
			t.Fatal(err)
		}
		got := f.wait(t, d.ID)
		if got.Status != StatusSucceeded || got.Commit != f.repo.rev("v1.0.0") || got.Source != SourceWebhook {
			t.Errorf("deploy = %+v", got)
		}
	})
}

func TestRefRulesTag(t *testing.T) {
	f := newFixture(t, fixtureOpts{pattern: "v*"})
	v110 := f.repo.rev("v1.1.0")
	tests := []struct {
		name, ref, commit string
		wantErr           string
		wantRef           string
	}{
		{"newest", "", "", "", "refs/tags/v1.1.0"},
		{"bare", "v1.0.0", "", "", "refs/tags/v1.0.0"},
		{"full", "refs/tags/v1.0.0", "", "", "refs/tags/v1.0.0"},
		{"annotated", "v1.1.0", "", "", "refs/tags/v1.1.0"},
		{"annotated with webhook commit", "refs/tags/v1.1.0", v110, "", "refs/tags/v1.1.0"},
		{"sha of matching tag", f.repo.rev("v1.0.0"), "", "", f.repo.rev("v1.0.0")},
		{"non-matching tag", "x1", "", ReasonRefNotAllowed, ""},
		{"non-matching tag full", "refs/tags/x1", "", ReasonRefNotAllowed, ""},
		{"branch", "main", "", ReasonRefNotAllowed, ""},
		{"branch full", "refs/heads/main", "", ReasonRefNotAllowed, ""},
		{"sha of no tag", f.repo.rev("dev"), "", ReasonRefNotAllowed, ""},
		{"missing tag", "v9.9.9", "", ReasonRefNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := f.mgr.Trigger(context.Background(), TriggerRequest{Project: "app", Ref: tt.ref, Commit: tt.commit})
			if tt.wantErr != "" {
				if got := triggerReason(err); got != tt.wantErr {
					t.Fatalf("err = %v, want reason %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.Ref != tt.wantRef {
				t.Errorf("ref = %q, want %q", d.Ref, tt.wantRef)
			}
			if got := f.wait(t, d.ID); got.Status != StatusSucceeded {
				t.Errorf("deploy = %+v", got)
			}
		})
	}
}

func TestTriggerRejections(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	if _, err := f.mgr.Trigger(context.Background(), TriggerRequest{Project: "nope"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown project err = %v", err)
	}
	p, _ := f.st.GetProject(context.Background(), "app")
	p.Status = "pending_access"
	if err := f.st.UpdateProject(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.Trigger(context.Background(), TriggerRequest{Project: "app"}); triggerReason(err) != ReasonProjectNotReady {
		t.Errorf("pending err = %v", err)
	}
}

func TestFailureStages(t *testing.T) {
	stderr := "first\nfailed to solve: boom"
	tests := []struct {
		name       string
		setup      func(*fixture)
		reason     string
		stage      string
		detail     string
		violations int
	}{
		{"compose not found", func(f *fixture) {
			gitRun(t, f.repo.work, "rm", "compose.yaml")
			gitRun(t, f.repo.work, "commit", "-m", "rm")
			gitRun(t, f.repo.work, "push", "origin", "main")
		}, ReasonComposeNotFound, StageConfig, "compose.yaml does not exist", 0},
		{"include escapes", func(f *fixture) {
			f.repo.commit(map[string]string{"compose.yaml": "include:\n  - ../other.yaml\n" + okCompose})
			gitRun(t, f.repo.work, "push", "origin", "main")
		}, ReasonComposeInvalid, StageConfig, "other.yaml", 0},
		{"config error", func(f *fixture) {
			f.comp.cfgErr = &compose.Error{Op: "config", Err: errors.New("exit status 1"), Stderr: stderr}
		}, ReasonComposeInvalid, StageConfig, "failed to solve: boom", 0},
		{"unparsable model", func(f *fixture) { f.comp.model = `{"services":{"web":"x"}}` },
			ReasonComposeInvalid, StagePolicy, "", 0},
		{"policy", func(f *fixture) {
			f.comp.model = `{"services":{"web":{"image":"x","privileged":true},"db":{"image":"y","network_mode":"host"}}}`
		}, ReasonPolicyViolation, StagePolicy, "2 policy violation(s)", 2},
		{"pull", func(f *fixture) {
			f.comp.pullErr = &compose.Error{Op: "pull", Err: errors.New("exit status 1"), Stderr: "pull access denied"}
		}, ReasonPullFailed, StagePull, "pull access denied", 0},
		{"build", func(f *fixture) {
			f.comp.buildErr = &compose.Error{Op: "build", Err: errors.New("exit status 1"), Stderr: stderr}
		}, ReasonBuildFailed, StageBuild, "failed to solve: boom", 0},
		{"up", func(f *fixture) {
			f.comp.upErr = &compose.Error{Op: "up", Err: errors.New("exit status 1"), Stderr: stderr}
		}, ReasonUpFailed, StageUp, "failed to solve: boom", 0},
		{"unhealthy", func(f *fixture) {
			f.rt.set(func(r *fakeRT) { r.health = "unhealthy" })
		}, ReasonHealthFailed, StageHealth, "web is unhealthy", 0},
		{"exited", func(f *fixture) {
			f.rt.set(func(r *fakeRT) { r.state, r.exit = "exited", 3 })
		}, ReasonHealthFailed, StageHealth, "web exited with code 3", 0},
		{"health timeout", func(f *fixture) {
			f.rt.set(func(r *fakeRT) { r.none = true })
			f.mgr.opts.HealthTimeout = 100 * time.Millisecond
		}, ReasonHealthFailed, StageHealth, "timed out", 0},
		{"runtime unavailable", func(f *fixture) {
			f.rt.set(func(r *fakeRT) { r.listErr = runtime.ErrUnavailable })
		}, ReasonRuntimeUnavailable, StageHealth, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			tt.setup(f)
			got := f.wait(t, f.trigger(t, "").ID)
			if got.Status != StatusFailed || got.Error == nil || got.Error.Reason != tt.reason || got.Stage != tt.stage {
				t.Fatalf("deploy = %+v (error %+v), want failed/%s at %s", got, got.Error, tt.reason, tt.stage)
			}
			if !strings.Contains(got.Error.Detail, tt.detail) || strings.Contains(got.Error.Detail, "\n") {
				t.Errorf("detail = %q, want containing %q on one line", got.Error.Detail, tt.detail)
			}
			if len(got.Error.Violations) != tt.violations {
				t.Errorf("violations = %+v", got.Error.Violations)
			}
			if tt.violations > 0 {
				b, _ := json.Marshal(got)
				if !strings.Contains(string(b), `"violations":[`) {
					t.Errorf("wire = %s", b)
				}
			}
		})
	}
	t.Run("compose stderr goes to the log", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		f.comp.cfgErr = &compose.Error{Op: "config", Err: errors.New("exit status 1"), Stderr: stderr}
		d := f.wait(t, f.trigger(t, "").ID)
		logs, _ := f.st.DeployLogs(context.Background(), d.ID, 0)
		var found bool
		for _, l := range logs {
			if l.Stream == "agent" && l.Text == "first" {
				found = true
			}
		}
		if !found {
			t.Errorf("logs = %+v", logs)
		}
	})
}

func TestFailedDeployKeepsProjectCommit(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	before, _ := f.svc.Get(context.Background(), "app")
	f.repo.commit(map[string]string{"B": "2\n"})
	gitRun(t, f.repo.work, "push", "origin", "main")
	f.comp.buildErr = errors.New("boom")
	d := f.wait(t, f.trigger(t, "").ID)
	after, _ := f.svc.Get(context.Background(), "app")
	if d.Status != StatusFailed || d.Commit != f.repo.rev("main") || after.Commit != before.Commit {
		t.Errorf("deploy %+v, project commit %q -> %q", d, before.Commit, after.Commit)
	}
}

func TestStartInterruptsUnfinished(t *testing.T) {
	f := newFixture(t, fixtureOpts{noStart: true})
	ctx := context.Background()
	mk := func(status string) int64 {
		d := &store.Deploy{Project: "app", Ref: "refs/heads/main", Source: SourceAPI, Status: status}
		if err := f.st.CreateDeploy(ctx, d); err != nil {
			t.Fatal(err)
		}
		return d.ID
	}
	q, r, ok := mk(StatusQueued), mk(StatusRunning), mk(StatusSucceeded)
	if _, err := f.mgr.Trigger(ctx, TriggerRequest{Project: "app"}); err == nil {
		t.Error("Trigger before Start succeeded")
	}
	if err := f.mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer f.mgr.Close()
	for id, want := range map[int64]string{q: StatusInterrupted, r: StatusInterrupted, ok: StatusSucceeded} {
		if d, _ := f.mgr.Get(ctx, id); d.Status != want {
			t.Errorf("deploy %d = %s, want %s", id, d.Status, want)
		}
	}
}

func TestShutdownInterruptsRunningAndQueued(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.comp.block = make(chan struct{})
	f.comp.entered = make(chan string, 4)
	a := f.trigger(t, "")
	<-f.comp.entered
	b := f.trigger(t, "")
	f.mgr.Close()
	for _, id := range []int64{a.ID, b.ID} {
		d, _ := f.mgr.Get(context.Background(), id)
		if d.Status != StatusInterrupted || d.FinishedAt == nil {
			t.Errorf("deploy %d = %+v", id, d)
		}
	}
	if _, err := f.mgr.Trigger(context.Background(), TriggerRequest{Project: "app"}); err == nil {
		t.Error("Trigger after Close succeeded")
	}
}

func TestParentContextCancelInterrupts(t *testing.T) {
	f := newFixture(t, fixtureOpts{noStart: true})
	ctx, cancel := context.WithCancel(context.Background())
	if err := f.mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer f.mgr.Close()
	f.comp.block = make(chan struct{})
	f.comp.entered = make(chan string, 4)
	d := f.trigger(t, "")
	<-f.comp.entered
	cancel()
	if got := f.wait(t, d.ID); got.Status != StatusInterrupted {
		t.Errorf("deploy = %+v", got)
	}
}

func TestLogCap(t *testing.T) {
	f := newFixture(t, fixtureOpts{tweak: func(o *Options) { o.LogMaxBytes = 80 }})
	f.comp.block = make(chan struct{})
	f.comp.entered = make(chan string, 4)
	d := f.trigger(t, "")
	<-f.comp.entered
	// A subscriber sees every line live, even beyond the cap.
	sub := newCapture(t, f.mgr, d.ID, 0)
	big := strings.Repeat("x", 200)
	f.mgr.mu.Lock()
	bc := f.mgr.casts[d.ID]
	f.mgr.mu.Unlock()
	bc.line("stdout", big)
	bc.line("stdout", "after")
	close(f.comp.block)
	f.wait(t, d.ID)
	sub.waitFinal(t)
	if !sub.has("stdout", big) || !sub.has("stdout", "after") || !sub.has("agent", "deploy succeeded") {
		t.Errorf("live updates = %+v", sub.updates())
	}

	logs, _ := f.st.DeployLogs(context.Background(), d.ID, 0)
	var truncated int
	for i, l := range logs {
		if l.Seq != int64(i+1) {
			t.Errorf("seq gap at %d: %+v", i, l)
		}
		if strings.HasPrefix(l.Text, "log truncated") {
			truncated++
			if l.Stream != "agent" {
				t.Errorf("truncation stream = %s", l.Stream)
			}
		}
		if l.Text == big || l.Text == "after" || l.Text == "deploy succeeded" {
			t.Errorf("stored line past cap: %+v", l)
		}
	}
	if truncated != 1 {
		t.Errorf("truncation lines = %d in %+v", truncated, logs)
	}
	// A late subscriber replays only the stored part, then the final status.
	late := newCapture(t, f.mgr, d.ID, 0)
	late.waitFinal(t)
	if late.has("stdout", big) {
		t.Error("late subscriber got an unstored line")
	}
	if fin := late.final(); fin == nil || fin.Status != StatusSucceeded {
		t.Errorf("final = %+v", fin)
	}
}

func TestDeleteHookCancelsDeploys(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.svc.OnDelete(f.mgr.Cancel)
	f.comp.block = make(chan struct{})
	f.comp.entered = make(chan string, 4)
	a := f.trigger(t, "")
	<-f.comp.entered
	b := f.trigger(t, "")

	done := make(chan error, 1)
	go func() {
		_, err := f.svc.Delete(context.Background(), "app", false)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Delete = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Delete blocked behind the running deploy")
	}
	// Rows are cascaded away with the project; the point is that nothing runs.
	if _, err := f.st.GetDeploy(context.Background(), a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deploy a: %v", err)
	}
	if _, err := f.st.GetDeploy(context.Background(), b.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deploy b: %v", err)
	}
	if calls := f.comp.Calls(); strings.Contains(strings.Join(calls, ","), "up") {
		t.Errorf("calls = %v", calls)
	}
	f.mgr.mu.Lock()
	n := len(f.mgr.states) + len(f.mgr.casts)
	f.mgr.mu.Unlock()
	if n != 0 {
		t.Errorf("manager state not released: %d", n)
	}
}

func TestListAndPrune(t *testing.T) {
	f := newFixture(t, fixtureOpts{tweak: func(o *Options) { o.Keep = 2 }})
	var ids []int64
	for range 3 {
		d := f.trigger(t, "")
		f.wait(t, d.ID)
		ids = append(ids, d.ID)
	}
	var ds []Deploy
	var err error
	for range 400 { // pruning runs just after the final status is stored
		if ds, err = f.mgr.List(context.Background(), "app", 0, 0); err != nil || len(ds) == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || len(ds) != 2 || ds[0].ID != ids[2] || ds[1].ID != ids[1] {
		t.Fatalf("List = %+v, %v", ds, err)
	}
	if page, _ := f.mgr.List(context.Background(), "app", 1, ids[2]); len(page) != 1 || page[0].ID != ids[1] {
		t.Errorf("page = %+v", page)
	}
	if _, err := f.mgr.List(context.Background(), "nope", 0, 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown project err = %v", err)
	}
}

func TestViewWire(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	d := &store.Deploy{ID: 7, Project: "app", Ref: "refs/heads/main", Source: "cli", Status: StatusFailed,
		Stage: "policy", ErrorReason: "policy_violation", ErrorDetail: "1 policy violation(s)",
		ErrorData: `{"violations":[{"service":"web","rule":"privileged","detail":"x"}]}`, CreatedAt: now, StartedAt: &now}
	b, _ := json.Marshal(View(d))
	want := `{"id":7,"project":"app","ref":"refs/heads/main","commit":"","source":"cli","status":"failed","stage":"policy",` +
		`"error":{"reason":"policy_violation","detail":"1 policy violation(s)","violations":[{"service":"web","rule":"privileged","detail":"x"}]},` +
		`"created_at":"2026-01-02T03:04:05Z","started_at":"2026-01-02T03:04:05Z","finished_at":null}`
	if string(b) != want {
		t.Errorf("wire =\n%s\nwant\n%s", b, want)
	}
	d.Status, d.ErrorReason = StatusSucceeded, ""
	if v := View(d); v.Error != nil {
		t.Errorf("error on non-failed deploy: %+v", v.Error)
	}
}
