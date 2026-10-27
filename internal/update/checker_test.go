package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitHub serves canned release responses and records requests.
type fakeGitHub struct {
	mu       sync.Mutex
	handler  func(w http.ResponseWriter, r *http.Request)
	requests []*http.Request
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Clone(r.Context()))
	h := f.handler
	f.mu.Unlock()
	h(w, r)
}

func (f *fakeGitHub) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeGitHub) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func writeJSON(w http.ResponseWriter, etag string, v any) {
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func relJSON(tag string, draft, pre bool, body string) map[string]any {
	return map[string]any{
		"tag_name": tag, "name": "Release " + tag, "body": body, "draft": draft,
		"prerelease": pre, "html_url": "https://github.com/open-southeners/ki/releases/tag/" + tag,
		"published_at": "2026-10-01T12:00:00Z",
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestChecker(t *testing.T, gh *fakeGitHub, mut func(*CheckerOptions)) (*Checker, *clock) {
	t.Helper()
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	clk := &clock{t: time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)}
	o := CheckerOptions{
		Dir:     t.TempDir(),
		Current: "v0.3.0",
		Enabled: true,
		BaseURL: srv.URL,
		Client:  srv.Client(),
		Now:     clk.now,
	}
	if mut != nil {
		mut(&o)
	}
	return NewChecker(o), clk
}

func TestCheckerStable(t *testing.T) {
	gh := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/open-southeners/ki/releases/latest" {
			t.Errorf("path = %s", r.URL.Path)
		}
		writeJSON(w, `"e1"`, relJSON("v0.4.0", false, false, "## Notes"))
	}}
	c, _ := newTestChecker(t, gh, nil)

	st := c.CheckNow(context.Background())
	if st.Latest == nil || st.Latest.Tag != "v0.4.0" || st.Latest.Notes != "## Notes" || st.Latest.Prerelease {
		t.Fatalf("latest = %+v", st.Latest)
	}
	if !st.UpdateAvailable || st.CheckedAt == nil || st.CheckError != nil {
		t.Fatalf("status = %+v", st)
	}
	if st.Current != "v0.3.0" || st.Channel != "stable" || !st.CheckEnabled || st.State != StateIdle {
		t.Fatalf("status = %+v", st)
	}
	if ua := gh.last().Header.Get("User-Agent"); ua != "ki-agent/v0.3.0" {
		t.Fatalf("User-Agent = %q", ua)
	}
}

func TestCheckerPrereleaseSkipsDraftsAndMalformed(t *testing.T) {
	gh := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/open-southeners/ki/releases" || r.URL.Query().Get("per_page") != "10" {
			t.Errorf("url = %s", r.URL)
		}
		writeJSON(w, "", []any{
			relJSON("v0.6.0", true, false, "draft"),
			relJSON("nightly", false, true, "bad tag"),
			relJSON("v0.5", false, false, "short tag"),
			relJSON("v0.5.0-rc.1", false, true, "rc"),
			relJSON("v0.4.0", false, false, "stable"),
		})
	}}
	c, _ := newTestChecker(t, gh, func(o *CheckerOptions) { o.Channel = ChannelPrerelease })

	st := c.CheckNow(context.Background())
	if st.Latest == nil || st.Latest.Tag != "v0.5.0-rc.1" || !st.Latest.Prerelease {
		t.Fatalf("latest = %+v", st.Latest)
	}
	if st.Channel != "prerelease" || !st.UpdateAvailable {
		t.Fatalf("status = %+v", st)
	}
}

func TestCheckerMalformedStableTagIgnored(t *testing.T) {
	gh := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, "", relJSON("latest", false, false, ""))
	}}
	c, _ := newTestChecker(t, gh, nil)
	st := c.CheckNow(context.Background())
	if st.Latest != nil || st.UpdateAvailable {
		t.Fatalf("status = %+v", st)
	}
}

func TestCheckerNoReleases(t *testing.T) {
	gh := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}}
	c, _ := newTestChecker(t, gh, nil)
	st := c.CheckNow(context.Background())
	if st.Latest != nil || st.CheckError != nil || st.CheckedAt == nil || st.UpdateAvailable {
		t.Fatalf("status = %+v", st)
	}
}

func TestCheckerVersionComparison(t *testing.T) {
	tests := []struct {
		current, latest string
		want            bool
	}{
		{"v0.3.0", "v0.4.0", true},
		{"v0.4.0", "v0.4.0", false},
		{"v0.5.0", "v0.4.0", false},
		{"v0.10.0", "v0.9.0", false},
		{"v0.4.0-rc.1", "v0.4.0", true},
		{"v0.4.0", "v0.4.0-rc.1", false},
		{"dev", "v9.9.9", false},
		{"", "v9.9.9", false},
	}
	for _, tt := range tests {
		if got := IsNewer(tt.current, &Release{Tag: tt.latest}); got != tt.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.want)
		}
	}
	if IsNewer("v0.1.0", nil) {
		t.Error("IsNewer with nil latest = true")
	}
}

func TestCheckerDevBuildNeverHasUpdate(t *testing.T) {
	gh := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, "", relJSON("v9.9.9", false, false, ""))
	}}
	c, _ := newTestChecker(t, gh, func(o *CheckerOptions) { o.Current = "dev" })
	st := c.CheckNow(context.Background())
	if st.Latest == nil || st.UpdateAvailable {
		t.Fatalf("status = %+v", st)
	}
	if ua := gh.last().Header.Get("User-Agent"); ua != "ki-agent/dev" {
		t.Fatalf("User-Agent = %q", ua)
	}
}

func TestCheckerETag(t *testing.T) {
	gh := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"e1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		writeJSON(w, `"e1"`, relJSON("v0.4.0", false, false, "n"))
	}}
	c, clk := newTestChecker(t, gh, nil)
	dir := c.opts.Dir

	first := c.CheckNow(context.Background())
	if gh.last().Header.Get("If-None-Match") != "" {
		t.Fatal("first request sent If-None-Match")
	}
	clk.advance(2 * time.Minute)
	second := c.CheckNow(context.Background())
	if gh.count() != 2 || gh.last().Header.Get("If-None-Match") != `"e1"` {
		t.Fatalf("requests = %d, inm = %q", gh.count(), gh.last().Header.Get("If-None-Match"))
	}
	if second.Latest == nil || second.Latest.Tag != "v0.4.0" || second.CheckError != nil {
		t.Fatalf("after 304: %+v", second)
	}
	if !second.CheckedAt.After(*first.CheckedAt) {
		t.Fatal("checked_at not refreshed on 304")
	}

	// A new checker on the same dir reuses the persisted ETag.
	clk.advance(2 * time.Minute)
	c2 := NewChecker(CheckerOptions{Dir: dir, Current: "v0.3.0", Enabled: true,
		BaseURL: c.opts.BaseURL, Client: c.opts.Client, Now: clk.now})
	c2.CheckNow(context.Background())
	if gh.last().Header.Get("If-None-Match") != `"e1"` {
		t.Fatal("persisted ETag not sent")
	}
}

func TestCheckerThrottlesCheckNow(t *testing.T) {
	gh := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, "", relJSON("v0.4.0", false, false, ""))
	}}
	c, clk := newTestChecker(t, gh, nil)
	ctx := context.Background()

	c.CheckNow(ctx)
	clk.advance(30 * time.Second)
	st := c.CheckNow(ctx)
	if gh.count() != 1 || st.Latest == nil {
		t.Fatalf("requests = %d, status = %+v", gh.count(), st)
	}
	clk.advance(31 * time.Second)
	c.CheckNow(ctx)
	if gh.count() != 2 {
		t.Fatalf("requests = %d, want 2", gh.count())
	}
}

func TestCheckerRateLimit(t *testing.T) {
	var limited atomic.Bool
	limited.Store(true)
	var reset time.Time
	gh := &fakeGitHub{}
	gh.handler = func(w http.ResponseWriter, r *http.Request) {
		if limited.Load() {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeJSON(w, "", relJSON("v0.4.0", false, false, ""))
	}
	c, clk := newTestChecker(t, gh, nil)
	reset = clk.now().Add(20 * time.Minute)
	ctx := context.Background()

	st := c.CheckNow(ctx)
	if st.CheckError == nil || !strings.Contains(*st.CheckError, "rate limit") {
		t.Fatalf("check_error = %v", st.CheckError)
	}

	// Still limited: no request is made even after the 60 s throttle.
	clk.advance(5 * time.Minute)
	c.CheckNow(ctx)
	if gh.count() != 1 {
		t.Fatalf("requests during back-off = %d, want 1", gh.count())
	}

	// After the reset time the checker contacts GitHub again.
	limited.Store(false)
	clk.advance(16 * time.Minute)
	st = c.CheckNow(ctx)
	if gh.count() != 2 || st.Latest == nil || st.CheckError != nil {
		t.Fatalf("requests = %d, status = %+v", gh.count(), st)
	}
}

func TestCheckerForbiddenWithoutRateLimitIsPlainError(t *testing.T) {
	gh := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "42")
		w.WriteHeader(http.StatusForbidden)
	}}
	c, clk := newTestChecker(t, gh, nil)
	c.CheckNow(context.Background())
	clk.advance(2 * time.Minute)
	c.CheckNow(context.Background())
	if gh.count() != 2 {
		t.Fatalf("requests = %d, want 2 (no back-off)", gh.count())
	}
}

func TestCheckerErrorKeepsCachedRelease(t *testing.T) {
	var fail atomic.Bool
	gh := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		writeJSON(w, "", relJSON("v0.4.0", false, false, ""))
	}}
	c, clk := newTestChecker(t, gh, nil)
	ctx := context.Background()
	c.CheckNow(ctx)
	fail.Store(true)
	clk.advance(2 * time.Minute)
	st := c.CheckNow(ctx)
	if st.CheckError == nil || st.Latest == nil || !st.UpdateAvailable {
		t.Fatalf("status = %+v", st)
	}
	rel, errStr := c.Latest()
	if rel == nil || rel.Tag != "v0.4.0" || errStr == "" {
		t.Fatalf("Latest = %+v, %q", rel, errStr)
	}
}

func TestCheckerNotesCap(t *testing.T) {
	// A multi-byte rune straddling the cap must not leave invalid UTF-8.
	body := strings.Repeat("a", MaxNotesSize-1) + "é" + strings.Repeat("b", 100)
	gh := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, "", relJSON("v0.4.0", false, false, body))
	}}
	c, _ := newTestChecker(t, gh, nil)
	st := c.CheckNow(context.Background())
	if len(st.Latest.Notes) != MaxNotesSize-1 {
		t.Fatalf("notes length = %d, want %d", len(st.Latest.Notes), MaxNotesSize-1)
	}
	if got := capNotes("short"); got != "short" {
		t.Fatalf("capNotes short = %q", got)
	}
}

func TestCheckerDisabledNeverContacts(t *testing.T) {
	gh := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request) {
		t.Error("GitHub contacted while disabled")
	}}
	c, _ := newTestChecker(t, gh, func(o *CheckerOptions) { o.Enabled = false })
	st := c.CheckNow(context.Background())
	c.Run(context.Background()) // returns immediately
	if st.CheckEnabled || st.Latest != nil || gh.count() != 0 {
		t.Fatalf("status = %+v", st)
	}
}

func TestCheckerStatePersistence(t *testing.T) {
	gh := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `"e1"`, relJSON("v0.4.0", false, false, "notes"))
	}}
	c, clk := newTestChecker(t, gh, nil)
	c.CheckNow(context.Background())

	path := filepath.Join(c.opts.Dir, "status.json")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("status.json mode = %v", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(c.opts.Dir)
	if len(entries) != 1 {
		t.Fatalf("leftover files in dir: %v", entries)
	}

	// A fresh checker serves the cache without contacting GitHub.
	c2 := NewChecker(CheckerOptions{Dir: c.opts.Dir, Current: "v0.3.0", Enabled: true,
		BaseURL: c.opts.BaseURL, Client: c.opts.Client, Now: clk.now})
	st := c2.Status()
	if st.Latest == nil || st.Latest.Tag != "v0.4.0" || st.Latest.Notes != "notes" || !st.UpdateAvailable || st.CheckedAt == nil {
		t.Fatalf("restored status = %+v", st)
	}
	if gh.count() != 1 {
		t.Fatalf("requests = %d", gh.count())
	}

	// A cache written for another channel is discarded.
	c3 := NewChecker(CheckerOptions{Dir: c.opts.Dir, Current: "v0.3.0", Enabled: true,
		Channel: ChannelPrerelease, Now: clk.now})
	if st := c3.Status(); st.Latest != nil || st.CheckedAt != nil {
		t.Fatalf("cross-channel status = %+v", st)
	}
}

func TestLoadStateCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte("{not json"), 0o640); err != nil {
		t.Fatal(err)
	}
	c := NewChecker(CheckerOptions{Dir: dir, Current: "v0.3.0"})
	if st := c.Status(); st.Latest != nil {
		t.Fatalf("status = %+v", st)
	}
}

func TestUpdateStatusJSONShape(t *testing.T) {
	b, err := json.Marshal(UpdateStatus{Current: "dev", Channel: "stable", State: StateIdle})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"current", "channel", "check_enabled", "checked_at", "check_error",
		"latest", "update_available", "apply_supported", "state", "last_result"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing field %q in %s", k, b)
		}
	}
	if m["latest"] != nil || m["last_result"] != nil || m["checked_at"] != nil || m["check_error"] != nil {
		t.Errorf("nullable fields should be null: %s", b)
	}
}

func TestRunSchedulesChecks(t *testing.T) {
	gh := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, "", relJSON("v0.4.0", false, false, ""))
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sleeps []time.Duration
	var jitters [][2]time.Duration
	c, _ := newTestChecker(t, gh, func(o *CheckerOptions) {
		o.Jitter = func(lo, hi time.Duration) time.Duration {
			jitters = append(jitters, [2]time.Duration{lo, hi})
			return lo
		}
		o.Sleep = func(ctx context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			if len(sleeps) == 3 {
				cancel()
				return ctx.Err()
			}
			return nil
		}
	})
	c.Run(ctx)

	if gh.count() != 2 {
		t.Fatalf("requests = %d, want 2", gh.count())
	}
	if len(jitters) < 2 || jitters[0] != [2]time.Duration{time.Minute, 5 * time.Minute} ||
		jitters[1] != [2]time.Duration{5*time.Hour + 30*time.Minute, 6*time.Hour + 30*time.Minute} {
		t.Fatalf("jitter ranges = %v", jitters)
	}
	if sleeps[0] != time.Minute || sleeps[1] != 5*time.Hour+30*time.Minute {
		t.Fatalf("sleeps = %v", sleeps)
	}
}
