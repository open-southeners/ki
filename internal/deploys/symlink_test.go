package deploys

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDotenvSymlinkRejected(t *testing.T) {
	for _, withVars := range []bool{false, true} {
		name := "no vars"
		if withVars {
			name = "vars"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			secret := filepath.Join(t.TempDir(), "other.env")
			if err := os.WriteFile(secret, []byte("SECRET=1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(secret, filepath.Join(f.repo.work, ".env")); err != nil {
				t.Fatal(err)
			}
			gitRun(t, f.repo.work, "add", ".env")
			gitRun(t, f.repo.work, "commit", "-m", "symlink")
			gitRun(t, f.repo.work, "push", "origin", "main")
			if withVars {
				if _, err := f.svc.SetEnv(context.Background(), "app", map[string]string{"X": "y"}); err != nil {
					t.Fatal(err)
				}
			}
			got := f.wait(t, f.trigger(t, "").ID)
			if got.Status != StatusFailed || got.Error == nil || got.Error.Reason != ReasonComposeInvalid ||
				got.Error.Detail != "'.env' must be a regular file" {
				t.Fatalf("deploy = %+v (error %+v)", got, got.Error)
			}
			if len(f.comp.Calls()) != 0 {
				t.Errorf("compose ran: %v", f.comp.Calls())
			}
			if b, _ := os.ReadFile(secret); string(b) != "SECRET=1\n" {
				t.Errorf("target modified: %q", b)
			}
		})
	}
}
