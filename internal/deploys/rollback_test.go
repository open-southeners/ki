package deploys

import (
	"context"
	"errors"
	"testing"

	"github.com/open-southeners/ki/internal/projects"
	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/store"
)

// pushCommit adds a commit to main and pushes it, returning its sha.
func (f *fixture) pushCommit(t *testing.T, name string) string {
	t.Helper()
	sha := f.repo.commit(map[string]string{name: name + "\n"})
	gitRun(t, f.repo.work, "push", "origin", "main")
	return sha
}

func (f *fixture) deployOK(t *testing.T) Deploy {
	t.Helper()
	d := f.wait(t, f.trigger(t, "").ID)
	if d.Status != StatusSucceeded {
		t.Fatalf("deploy = %+v", d)
	}
	return d
}

func (f *fixture) rollback(req RollbackRequest) (*Deploy, error) {
	req.Project = "app"
	return f.mgr.Rollback(context.Background(), req)
}

func (f *fixture) setBuildErr(err error) {
	f.comp.mu.Lock()
	f.comp.buildErr = err
	f.comp.mu.Unlock()
}

func TestRollbackTargetSelection(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	ctx := context.Background()

	if _, err := f.rollback(RollbackRequest{}); triggerReason(err) != ReasonNoRollbackTarget {
		t.Fatalf("no deploys: err = %v", err)
	}
	d1 := f.deployOK(t)
	if _, err := f.rollback(RollbackRequest{}); triggerReason(err) != ReasonNoRollbackTarget {
		t.Fatalf("one succeeded: err = %v", err)
	}

	f.pushCommit(t, "B")
	d2 := f.deployOK(t)
	f.pushCommit(t, "C")
	f.setBuildErr(errors.New("boom"))
	failed := f.wait(t, f.trigger(t, "").ID)
	if failed.Status != StatusFailed {
		t.Fatalf("failed deploy = %+v", failed)
	}
	f.setBuildErr(nil)

	// The newest succeeded deploy is d2; the failed one is skipped, so the
	// previous good one is d1.
	rb, err := f.rollback(RollbackRequest{Source: SourceCLI})
	if err != nil {
		t.Fatal(err)
	}
	if rb.RollbackOf != d1.ID || rb.Ref != d1.Ref || rb.Source != SourceCLI {
		t.Fatalf("rollback = %+v, want rollback of %d", rb, d1.ID)
	}
	got := f.wait(t, rb.ID)
	if got.Status != StatusSucceeded || got.Commit != d1.Commit || got.RollbackOf != d1.ID {
		t.Fatalf("rollback deploy = %+v, want commit %s", got, d1.Commit)
	}
	st, err := f.st.GetDeploy(ctx, rb.ID)
	if err != nil || st.RollbackOf != d1.ID {
		t.Fatalf("stored = %+v, %v", st, err)
	}
	if o, _ := f.st.GetDeploy(ctx, d2.ID); o.RollbackOf != 0 {
		t.Errorf("ordinary deploy has rollback_of %d", o.RollbackOf)
	}

	// Explicit id.
	rb, err = f.rollback(RollbackRequest{Deploy: d2.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.wait(t, rb.ID); got.Status != StatusSucceeded || got.Commit != d2.Commit || got.RollbackOf != d2.ID {
		t.Fatalf("explicit rollback = %+v", got)
	}

	// Unusable explicit targets.
	if _, err := f.rollback(RollbackRequest{Deploy: failed.ID}); triggerReason(err) != ReasonNoRollbackTarget {
		t.Errorf("failed target: err = %v", err)
	}
	if _, err := f.rollback(RollbackRequest{Deploy: 9999}); triggerReason(err) != ReasonNoRollbackTarget {
		t.Errorf("unknown target: err = %v", err)
	}
	if _, err := f.mgr.Rollback(ctx, RollbackRequest{Project: "nope"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown project: err = %v", err)
	}
}

func TestRollbackOtherProjectsDeploy(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	ctx := context.Background()
	d := f.deployOK(t)
	if _, err := f.svc.Create(ctx, projects.CreateParams{RepoURL: f.repo.URL, Slug: "other"}); err != nil {
		t.Fatal(err)
	}
	_, err := f.mgr.Rollback(ctx, RollbackRequest{Project: "other", Deploy: d.ID})
	if triggerReason(err) != ReasonNoRollbackTarget {
		t.Fatalf("err = %v", err)
	}
}

func TestRollbackCommitNoLongerOnBranchFailsAtFetch(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	base := f.deployOK(t)
	f.pushCommit(t, "B")
	d2 := f.deployOK(t)
	// Rewrite history: main goes back to base's commit plus a different one.
	gitRun(t, f.repo.work, "reset", "--hard", base.Commit)
	f.repo.commit(map[string]string{"other": "x\n"})
	gitRun(t, f.repo.work, "push", "-f", "origin", "main")
	rb, err := f.rollback(RollbackRequest{Deploy: d2.ID})
	if err != nil {
		t.Fatal(err)
	}
	got := f.wait(t, rb.ID)
	if got.Status != StatusFailed || got.Error == nil || got.Error.Reason != ReasonFetchFailed || got.RollbackOf != d2.ID {
		t.Fatalf("deploy = %+v", got)
	}
}

func TestRollbackTagMovedIsRejected(t *testing.T) {
	f := newFixture(t, fixtureOpts{pattern: "v*"})
	d := f.wait(t, f.trigger(t, "v1.0.0").ID)
	if d.Status != StatusSucceeded {
		t.Fatalf("deploy = %+v", d)
	}
	rb, err := f.rollback(RollbackRequest{Deploy: d.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.wait(t, rb.ID); got.Status != StatusSucceeded || got.Commit != d.Commit {
		t.Fatalf("rollback = %+v", got)
	}
	gitRun(t, f.repo.work, "tag", "-f", "v1.0.0", f.repo.rev("dev"))
	gitRun(t, f.repo.work, "push", "-f", "origin", "refs/tags/v1.0.0")
	if _, err := f.rollback(RollbackRequest{Deploy: d.ID}); triggerReason(err) != ReasonNoRollbackTarget {
		t.Fatalf("err = %v", err)
	}
}

func TestRollbackHandler(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d1 := f.deployOK(t)
	f.pushCommit(t, "B")
	f.deployOK(t)
	c := newClient(t, f.mgr, rpc.TransportUnix)

	var rb Deploy
	if err := c.call(MethodRollback, m{"project": "app"}, &rb); err != nil {
		t.Fatal(err)
	}
	if rb.RollbackOf != d1.ID || rb.Source != SourceCLI {
		t.Fatalf("rollback = %+v", rb)
	}
	f.wait(t, rb.ID)

	var re *rpc.Error
	err := c.call(MethodRollback, m{"project": "app", "deploy": 9999}, &rb)
	if !errors.As(err, &re) || re.Code != rpc.CodeDeployError {
		t.Fatalf("err = %v", err)
	}
	if err := c.call(MethodRollback, m{"project": "nope"}, &rb); !errors.As(err, &re) || re.Code != rpc.CodeNotFound {
		t.Fatalf("unknown project err = %v", err)
	}
	if err := c.call(MethodRollback, m{}, &rb); !errors.As(err, &re) || re.Code != rpc.CodeInvalidParams {
		t.Fatalf("missing project err = %v", err)
	}
	ws := newClient(t, f.mgr, rpc.TransportWebSocket)
	if err := ws.call(MethodRollback, m{"project": "app", "deploy": d1.ID}, &rb); err != nil || rb.Source != SourceAPI {
		t.Fatalf("ws rollback = %+v, %v", rb, err)
	}
	f.wait(t, rb.ID)
}
