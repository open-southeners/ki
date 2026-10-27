package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/mod/semver"
)

// Release channels.
const (
	ChannelStable     = "stable"
	ChannelPrerelease = "prerelease"
)

const (
	// DefaultAPIBaseURL is the GitHub REST API root.
	DefaultAPIBaseURL = "https://api.github.com"

	repoPath = "/repos/open-southeners/ki"

	// MaxNotesSize caps the release notes kept and shown.
	MaxNotesSize = 64 << 10

	// minCheckGap is how often CheckNow may contact GitHub.
	minCheckGap = 60 * time.Second

	checkInterval = 6 * time.Hour
	checkJitter   = 30 * time.Minute
	firstCheckMin = 1 * time.Minute
	firstCheckMax = 5 * time.Minute

	// maxResponseSize bounds the GitHub response body read.
	maxResponseSize = 8 << 20

	// maxBackoff bounds a rate-limit wait derived from response headers.
	maxBackoff = 24 * time.Hour
)

// tagPattern is the only tag shape the agent accepts as a release.
var tagPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// ValidTag reports whether tag is a release tag of the form vX.Y.Z[-pre].
func ValidTag(tag string) bool {
	return tagPattern.MatchString(tag) && semver.IsValid(tag)
}

// Canonical returns v in release-tag form (a single leading "v"). Release
// builds report their version without the "v" ("1.0.1", from GoReleaser's
// {{ .Version }}) while release tags and everything in this package use
// "v1.0.1", so versions must go through Canonical before being compared. A v
// that is not a valid release version ("dev", "") is returned unchanged.
func Canonical(v string) string {
	if c := "v" + strings.TrimPrefix(v, "v"); ValidTag(c) {
		return c
	}
	return v
}

// CheckerOptions configures a [Checker]. Only Dir and Current are required.
type CheckerOptions struct {
	// Dir is the exchange directory holding status.json.
	Dir string
	// Current is the running version ("dev" in source builds).
	Current string
	// Channel is ChannelStable (default) or ChannelPrerelease.
	Channel string
	// Enabled false means GitHub is never contacted.
	Enabled bool
	// BaseURL overrides the GitHub API root (tests).
	BaseURL string
	// Client overrides the HTTP client (tests).
	Client *http.Client
	Logger *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
	// Sleep waits for d or until ctx ends (tests). It returns ctx.Err() when cancelled.
	Sleep func(ctx context.Context, d time.Duration) error
	// Jitter returns a duration in [lo, hi] (tests).
	Jitter func(lo, hi time.Duration) time.Duration
}

// Checker polls GitHub Releases for a newer agent version and caches the
// answer in <dir>/status.json.
type Checker struct {
	opts CheckerOptions
	log  *slog.Logger

	checkMu sync.Mutex // serialises network checks

	mu           sync.Mutex // guards the fields below
	st           cachedState
	lastContact  time.Time
	backoffUntil time.Time
}

// NewChecker builds a checker and loads any cached state from disk.
func NewChecker(o CheckerOptions) *Checker {
	if o.Channel != ChannelPrerelease {
		o.Channel = ChannelStable
	}
	if o.BaseURL == "" {
		o.BaseURL = DefaultAPIBaseURL
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 20 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}
	if o.Jitter == nil {
		o.Jitter = randomBetween
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	c := &Checker{opts: o, log: log}
	c.st = loadState(o.Dir)
	if c.st.Channel != o.Channel {
		// A cache from another channel (or none) says nothing about this one.
		c.st = cachedState{Channel: o.Channel}
	}
	return c
}

// Run checks periodically until ctx ends: the first time 1-5 minutes after
// the call, then every 6 hours plus or minus 30 minutes. It returns at once
// when checks are disabled.
func (c *Checker) Run(ctx context.Context) {
	if !c.opts.Enabled {
		return
	}
	wait := c.opts.Jitter(firstCheckMin, firstCheckMax)
	for {
		if err := c.opts.Sleep(ctx, wait); err != nil {
			return
		}
		c.check(ctx)
		wait = c.opts.Jitter(checkInterval-checkJitter, checkInterval+checkJitter)
		c.mu.Lock()
		if until := c.backoffUntil.Sub(c.opts.Now()); until > wait {
			wait = until
		}
		c.mu.Unlock()
	}
}

// CheckNow refreshes the cache from GitHub, at most once per 60 seconds;
// within that window (or while rate limited, or when checks are disabled) it
// returns the cached status.
func (c *Checker) CheckNow(ctx context.Context) UpdateStatus {
	if c.opts.Enabled {
		c.mu.Lock()
		recent := !c.lastContact.IsZero() && c.opts.Now().Sub(c.lastContact) < minCheckGap
		c.mu.Unlock()
		if !recent {
			c.check(ctx)
		}
	}
	return c.Status()
}

// Latest returns the cached latest release (nil if none) and the last check
// error ("" if the last check succeeded).
func (c *Checker) Latest() (*Release, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.Latest, c.st.CheckError
}

// Status builds the checker's part of [UpdateStatus]. ApplySupported, State
// and LastResult are left for the caller that owns the apply flow.
func (c *Checker) Status() UpdateStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := UpdateStatus{
		Current:      c.opts.Current,
		Channel:      c.opts.Channel,
		CheckEnabled: c.opts.Enabled,
		CheckedAt:    c.st.CheckedAt,
		Latest:       c.st.Latest,
		State:        StateIdle,
	}
	if c.st.CheckError != "" {
		msg := c.st.CheckError
		s.CheckError = &msg
	}
	s.UpdateAvailable = IsNewer(c.opts.Current, c.st.Latest)
	return s
}

// IsNewer reports whether latest is a newer release than current. A "dev" or
// otherwise non-release current version never has an update.
func IsNewer(current string, latest *Release) bool {
	current = Canonical(current)
	if latest == nil || !ValidTag(current) || !ValidTag(latest.Tag) {
		return false
	}
	return semver.Compare(latest.Tag, current) > 0
}

// check contacts GitHub once and records the outcome.
func (c *Checker) check(ctx context.Context) {
	c.checkMu.Lock()
	defer c.checkMu.Unlock()

	now := c.opts.Now()
	c.mu.Lock()
	if now.Before(c.backoffUntil) {
		c.mu.Unlock()
		return
	}
	c.lastContact = now
	st := c.st
	c.mu.Unlock()

	next, until, err := c.fetch(ctx, st)
	if err != nil {
		c.log.Warn("update check failed", "error", err)
		next = st
		next.CheckError = err.Error()
	}

	c.mu.Lock()
	c.st = next
	if !until.IsZero() {
		c.backoffUntil = until
	}
	c.mu.Unlock()

	if err := saveState(c.opts.Dir, next); err != nil {
		c.log.Warn("could not save update state", "error", err)
	}
}

// ghRelease is the subset of a GitHub release the checker reads.
type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
}

// fetch performs the conditional request and returns the new cached state. A
// non-zero until asks the caller to hold off until then.
func (c *Checker) fetch(ctx context.Context, st cachedState) (cachedState, time.Time, error) {
	url := c.opts.BaseURL + repoPath + "/releases/latest"
	if c.opts.Channel == ChannelPrerelease {
		url = c.opts.BaseURL + repoPath + "/releases?per_page=10"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return st, time.Time{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "ki-agent/"+c.opts.Current)
	if st.ETag != "" {
		req.Header.Set("If-None-Match", st.ETag)
	}

	resp, err := c.opts.Client.Do(req)
	if err != nil {
		return st, time.Time{}, fmt.Errorf("contact GitHub: %w", err)
	}
	defer resp.Body.Close()

	now := c.opts.Now()
	switch resp.StatusCode {
	case http.StatusNotModified:
		st.CheckedAt = &now
		st.CheckError = ""
		return st, time.Time{}, nil
	case http.StatusNotFound:
		// No release has been published (yet).
		return cachedState{Channel: st.Channel, CheckedAt: &now}, time.Time{}, nil
	case http.StatusForbidden, http.StatusTooManyRequests:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			until := rateLimitReset(resp, now)
			return st, until, fmt.Errorf("GitHub rate limit reached; next check after %s", until.UTC().Format(time.RFC3339))
		}
		return st, time.Time{}, fmt.Errorf("GitHub returned %s", resp.Status)
	case http.StatusOK:
	default:
		return st, time.Time{}, fmt.Errorf("GitHub returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return st, time.Time{}, fmt.Errorf("read GitHub response: %w", err)
	}
	var releases []ghRelease
	if c.opts.Channel == ChannelPrerelease {
		err = json.Unmarshal(body, &releases)
	} else {
		var one ghRelease
		if err = json.Unmarshal(body, &one); err == nil {
			releases = []ghRelease{one}
		}
	}
	if err != nil {
		return st, time.Time{}, fmt.Errorf("decode GitHub response: %w", err)
	}

	out := cachedState{Channel: st.Channel, ETag: resp.Header.Get("ETag"), CheckedAt: &now}
	for _, r := range releases {
		if r.Draft || !ValidTag(r.TagName) {
			continue
		}
		out.Latest = &Release{
			Tag:         r.TagName,
			Name:        r.Name,
			PublishedAt: r.PublishedAt,
			URL:         r.HTMLURL,
			Notes:       capNotes(r.Body),
			Prerelease:  r.Prerelease,
		}
		break
	}
	return out, time.Time{}, nil
}

// rateLimitReset returns when the rate limit lifts, from X-RateLimit-Reset
// (Unix seconds), bounded to a sane window.
func rateLimitReset(resp *http.Response, now time.Time) time.Time {
	until := now.Add(time.Hour)
	if n, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		until = time.Unix(n, 0)
	}
	if until.Before(now) {
		until = now
	}
	if limit := now.Add(maxBackoff); until.After(limit) {
		until = limit
	}
	return until
}

// capNotes truncates release notes to MaxNotesSize bytes on a rune boundary.
func capNotes(s string) string {
	if len(s) <= MaxNotesSize {
		return s
	}
	s = s[:MaxNotesSize]
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size != 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func randomBetween(lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + rand.N(hi-lo+1)
}
