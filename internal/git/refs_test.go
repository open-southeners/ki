package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedRefs extends seedRepo with an annotated tag v2 and a lightweight tag
// v10 on main's new tip; it returns the relevant shas.
func seedRefs(t *testing.T) (url, work string, mainTip, devTip, v1 string) {
	t.Helper()
	url, work = seedRepo(t)
	v1 = gitRun(t, work, "rev-parse", "v1")
	mainTip = commit(t, work, "M2", "m2\n")
	gitRun(t, work, "tag", "-a", "v2", "-m", "v2")
	gitRun(t, work, "tag", "v10")
	gitRun(t, work, "push", "origin", "main", "v2", "v10")
	devTip = gitRun(t, work, "rev-parse", "dev")
	return
}

func TestResolveRef(t *testing.T) {
	url, work, mainTip, devTip, v1 := seedRefs(t)
	c := newClient(t, nil)
	if v2Obj := gitRun(t, work, "rev-parse", "v2"); v2Obj == mainTip {
		t.Fatal("v2 should be an annotated tag object")
	}
	upper := strings.ToUpper(mainTip)
	tests := []struct {
		name, ref string
		want      Ref
		reason    string
	}{
		{"full branch", "refs/heads/main", Ref{"refs/heads/main", mainTip}, ""},
		{"full tag", "refs/tags/v1", Ref{"refs/tags/v1", v1}, ""},
		{"annotated peeled", "refs/tags/v2", Ref{"refs/tags/v2", mainTip}, ""},
		{"bare branch", "dev", Ref{"refs/heads/dev", devTip}, ""},
		{"bare tag", "v2", Ref{"refs/tags/v2", mainTip}, ""},
		{"sha as is", upper, Ref{"", mainTip}, ""},
		{"missing", "nope", Ref{}, ReasonRefNotFound},
		{"missing full", "refs/heads/nope", Ref{}, ReasonRefNotFound},
		{"suffix is not a match", "ain", Ref{}, ReasonRefNotFound},
		{"other namespace", "refs/pull/1/head", Ref{}, ReasonRefNotFound},
		{"leading dash", "--upload-pack=x", Ref{}, ReasonRefNotFound},
		{"dotdot", "a..b", Ref{}, ReasonRefNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.ResolveRef(context.Background(), url, Auth{}, tt.ref)
			if tt.reason != "" {
				if r := reasonOf(t, err); r != tt.reason {
					t.Fatalf("reason = %q", r)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestResolveRefBranchBeatsTag(t *testing.T) {
	url, work, _, _, v1 := seedRefs(t)
	tip := gitRun(t, work, "rev-parse", "main")
	gitRun(t, work, "push", "origin", "main:refs/heads/v1")
	c := newClient(t, nil)
	got, err := c.ResolveRef(context.Background(), url, Auth{}, "v1")
	if err != nil || got != (Ref{"refs/heads/v1", tip}) || got.Commit == v1 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestResolveRefErrors(t *testing.T) {
	c := newClient(t, nil)
	_, err := c.ResolveRef(context.Background(), "file://"+filepath.Join(t.TempDir(), "nope.git"), Auth{}, "main")
	if r := reasonOf(t, err); r != ReasonNotFound {
		t.Errorf("reason = %q", r)
	}
	_, err = c.ResolveRef(context.Background(), "ext::sh -c id", Auth{}, "main")
	if r := reasonOf(t, err); r != ReasonInvalidURL {
		t.Errorf("reason = %q", r)
	}
}

func TestListTags(t *testing.T) {
	url, _, mainTip, _, v1 := seedRefs(t)
	c := newClient(t, nil)
	got, err := c.ListTags(context.Background(), url, Auth{})
	if err != nil {
		t.Fatal(err)
	}
	want := []Ref{
		{"refs/tags/v10", mainTip},
		{"refs/tags/v2", mainTip},
		{"refs/tags/v1", v1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tag %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	empty := filepath.Join(t.TempDir(), "empty.git")
	gitRun(t, t.TempDir(), "init", "--bare", empty)
	got, err = c.ListTags(context.Background(), "file://"+empty, Auth{})
	if err != nil || len(got) != 0 {
		t.Fatalf("empty: %+v, %v", got, err)
	}
}

func TestCheckout(t *testing.T) {
	url, work, mainTip, devTip, v1 := seedRefs(t)
	c := newClient(t, nil)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "co")

	// Missing dir: initialised and fetched; head of the ref.
	got, err := c.Checkout(ctx, url, Auth{}, dir, "refs/heads/main", "")
	if err != nil || got != mainTip {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "M2")); err != nil {
		t.Error("M2 missing:", err)
	}
	// Untracked files survive.
	if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pinned older commit on the branch.
	got, err = c.Checkout(ctx, url, Auth{}, dir, "refs/heads/main", v1)
	if err != nil || got != v1 {
		t.Fatalf("pinned: got %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "M2")); !os.IsNotExist(err) {
		t.Error("M2 should be gone")
	}
	if _, err := os.Stat(filepath.Join(dir, "keep")); err != nil {
		t.Error("untracked file removed:", err)
	}

	// Commit equal to tip, uppercase accepted.
	got, err = c.Checkout(ctx, url, Auth{}, dir, "refs/heads/main", strings.ToUpper(mainTip))
	if err != nil || got != mainTip {
		t.Fatalf("tip: got %q, %v", got, err)
	}

	// Annotated tag.
	got, err = c.Checkout(ctx, url, Auth{}, dir, "refs/tags/v2", mainTip)
	if err != nil || got != mainTip {
		t.Fatalf("tag: got %q, %v", got, err)
	}
	got, err = c.Checkout(ctx, url, Auth{}, dir, "refs/tags/v2", "")
	if err != nil || got != mainTip {
		t.Fatalf("tag head: got %q, %v", got, err)
	}

	// New upstream commit is picked up.
	next := commit(t, work, "M3", "m3\n")
	gitRun(t, work, "push", "origin", "main")
	got, err = c.Checkout(ctx, url, Auth{}, dir, "refs/heads/main", "")
	if err != nil || got != next {
		t.Fatalf("update: got %q, %v", got, err)
	}

	// Commit not on the ref (dev-only commit, unknown commit).
	for name, sha := range map[string]string{"dev commit": devTip, "unknown": strings.Repeat("a", 40)} {
		_, err = c.Checkout(ctx, url, Auth{}, dir, "refs/heads/main", sha)
		if r := reasonOf(t, err); r != ReasonRefNotFound {
			t.Errorf("%s: reason = %q", name, r)
		}
	}
	if head := gitRun(t, dir, "rev-parse", "HEAD"); head != next {
		t.Errorf("HEAD moved to %s on rejected commit", head)
	}
}

func TestCheckoutErrors(t *testing.T) {
	url, _, _, _, _ := seedRefs(t)
	c := newClient(t, nil)
	ctx := context.Background()
	root := t.TempDir()
	tests := []struct {
		name, ref, commit, reason string
	}{
		{"missing ref", "refs/heads/nope", "", ReasonRefNotFound},
		{"dash ref", "--upload-pack=x", "", ReasonRefNotFound},
		{"bad commit", "refs/heads/main", "not-a-sha", ReasonRefNotFound},
		{"dash commit", "refs/heads/main", "--" + strings.Repeat("a", 38), ReasonRefNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(root, strings.ReplaceAll(tt.name, " ", "-"))
			_, err := c.Checkout(ctx, url, Auth{}, dir, tt.ref, tt.commit)
			if r := reasonOf(t, err); r != tt.reason {
				t.Fatalf("reason = %q (%v)", r, err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Error("failed checkout left directory behind")
			}
		})
	}
	if _, err := c.Checkout(ctx, url, Auth{}, "", "refs/heads/main", ""); err == nil {
		t.Error("expected error without dir")
	}
	_, err := c.Checkout(ctx, "ext::sh -c id", Auth{}, filepath.Join(root, "x"), "refs/heads/main", "")
	if r := reasonOf(t, err); r != ReasonInvalidURL {
		t.Errorf("reason = %q", r)
	}
}
