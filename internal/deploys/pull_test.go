package deploys

import (
	"strings"
	"testing"
)

func TestPullMissingSkipsPullStage(t *testing.T) {
	f := newFixture(t, fixtureOpts{tweak: func(o *Options) { o.Pull = "missing" }})
	got := f.wait(t, f.trigger(t, "").ID)
	if got.Status != StatusSucceeded {
		t.Fatalf("deploy = %+v (%+v)", got, got.Error)
	}
	if want := "config app,build app,up app"; strings.Join(f.comp.Calls(), ",") != want {
		t.Errorf("calls = %v, want %s", f.comp.Calls(), want)
	}
}
