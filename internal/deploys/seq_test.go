package deploys

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (cp *capture) maxSeq() int64 {
	var max int64
	for _, u := range cp.updates() {
		if u.Type == "log" && u.Seq > max {
			max = u.Seq
		}
	}
	return max
}

func TestResubscribeWithLastSeq(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.comp.block = make(chan struct{})
	f.comp.entered = make(chan string, 4)
	d := f.trigger(t, "")
	<-f.comp.entered
	first := newCapture(t, f.mgr, d.ID, 0)
	stored, _ := f.st.DeployLogs(context.Background(), d.ID, 0)
	for first.maxSeq() < int64(len(stored)) {
		time.Sleep(5 * time.Millisecond)
	}
	last := first.maxSeq()
	seen := map[int64]string{}
	for _, u := range first.updates() {
		if u.Type == "log" {
			if u.Seq != int64(len(seen)+1) {
				t.Fatalf("seq %d out of order", u.Seq)
			}
			seen[u.Seq] = u.Stream + ":" + u.Text
		}
	}
	// The first connection drops; more output arrives while nobody listens.
	first.c.p.Close()
	f.mgr.mu.Lock()
	bc := f.mgr.casts[d.ID]
	f.mgr.mu.Unlock()
	bc.line("stdout", "while away 1")
	bc.line("stdout", "while away 2")
	resumed := newCapture(t, f.mgr, d.ID, last)
	bc.line("stdout", "live")
	close(f.comp.block)
	resumed.waitFinal(t)
	for _, u := range resumed.updates() {
		if u.Type != "log" {
			continue
		}
		if _, dup := seen[u.Seq]; dup || u.Seq != int64(len(seen)+1) {
			t.Fatalf("resumed seq %d duplicates or skips (have %d)", u.Seq, len(seen))
		}
		seen[u.Seq] = u.Stream + ":" + u.Text
	}
	all, _ := f.st.DeployLogs(context.Background(), d.ID, 0)
	if len(seen) != len(all) {
		t.Fatalf("seen %d lines, stored %d", len(seen), len(all))
	}
	for _, l := range all {
		if seen[l.Seq] != l.Stream+":"+l.Text {
			t.Errorf("line %d = %q, stored %s:%s", l.Seq, seen[l.Seq], l.Stream, l.Text)
		}
	}
}

func TestDotenvTrackedByRepoIsKept(t *testing.T) {
	f := newFixture(t, fixtureOpts{files: map[string]string{"compose.yaml": okCompose, ".env": "FROM_REPO=1\n"}})
	envPath := filepath.Join(f.data, "projects", "app", ".env")
	// No project vars: the repository's own file stays.
	if got := f.wait(t, f.trigger(t, "").ID); got.Status != StatusSucceeded {
		t.Fatalf("deploy = %+v", got)
	}
	if b, err := os.ReadFile(envPath); err != nil || string(b) != "FROM_REPO=1\n" {
		t.Fatalf(".env = %q, %v", b, err)
	}
	// Project vars win over the tracked file.
	if _, err := f.svc.SetEnv(context.Background(), "app", map[string]string{"X": "y"}); err != nil {
		t.Fatal(err)
	}
	f.wait(t, f.trigger(t, "").ID)
	if b, _ := os.ReadFile(envPath); strings.Contains(string(b), "FROM_REPO") || !strings.Contains(string(b), "X=") {
		t.Errorf(".env = %q", b)
	}
	// Without vars again the next checkout restores the tracked file.
	if _, err := f.svc.UnsetEnv(context.Background(), "app", []string{"X"}); err != nil {
		t.Fatal(err)
	}
	f.wait(t, f.trigger(t, "").ID)
	if b, _ := os.ReadFile(envPath); string(b) != "FROM_REPO=1\n" {
		t.Errorf(".env = %q", b)
	}
}
