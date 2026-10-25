package projects

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/open-southeners/ki-agent/internal/git"
	"github.com/open-southeners/ki-agent/internal/store"
)

// Environment variable limits.
const (
	// MaxEnvVars is the most variables one project may hold.
	MaxEnvVars = 256
	// MaxEnvValue is the largest accepted value in bytes.
	MaxEnvValue = 64 << 10
)

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// EnvEntry is the wire view of one variable: the value is never included.
type EnvEntry struct {
	Name      string `json:"name"`
	UpdatedAt string `json:"updated_at"`
}

func envAAD(slug, name string) []byte { return aad(slug, "env/"+name) }

func envNames(vars []store.EnvVar) []string {
	names := make([]string, 0, len(vars))
	for _, v := range vars {
		names = append(names, v.Name)
	}
	return names
}

// SetEnv merges vars into the project's environment (existing names are
// overwritten, others kept) and returns all names afterwards, sorted. It takes
// the project lock.
func (s *Service) SetEnv(ctx context.Context, slug string, vars map[string]string) ([]string, error) {
	if len(vars) == 0 {
		return nil, invalidf("vars must not be empty")
	}
	for name, val := range vars {
		if !envNameRe.MatchString(name) {
			return nil, invalidf("variable name %q must match %s", name, envNameRe)
		}
		if len(val) > MaxEnvValue {
			return nil, invalidf("value of %s exceeds %d bytes", name, MaxEnvValue)
		}
		if strings.ContainsRune(val, 0) {
			return nil, invalidf("value of %s contains a NUL byte", name)
		}
	}
	unlock := s.lock(slug)
	defer unlock()
	cur, err := s.st.ListEnv(ctx, slug)
	if err != nil {
		return nil, err
	}
	total := len(cur)
	have := make(map[string]bool, len(cur))
	for _, v := range cur {
		have[v.Name] = true
	}
	for name := range vars {
		if !have[name] {
			total++
		}
	}
	if total > MaxEnvVars {
		return nil, invalidf("a project may hold at most %d variables", MaxEnvVars)
	}
	sealed := make(map[string][]byte, len(vars))
	for name, val := range vars {
		sealed[name] = s.box.Seal([]byte(val), envAAD(slug, name))
	}
	if err := s.st.SetEnv(ctx, slug, sealed); err != nil {
		return nil, err
	}
	after, err := s.st.ListEnv(ctx, slug)
	if err != nil {
		return nil, err
	}
	s.log.Info("project env set", "slug", slug, "count", len(vars))
	return envNames(after), nil
}

// UnsetEnv removes the named variables (unknown names are ignored) and returns
// the remaining names, sorted. It takes the project lock.
func (s *Service) UnsetEnv(ctx context.Context, slug string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, invalidf("names must not be empty")
	}
	unlock := s.lock(slug)
	defer unlock()
	if err := s.st.UnsetEnv(ctx, slug, names); err != nil {
		return nil, err
	}
	after, err := s.st.ListEnv(ctx, slug)
	if err != nil {
		return nil, err
	}
	s.log.Info("project env unset", "slug", slug, "count", len(names))
	return envNames(after), nil
}

// ListEnv returns the variables' names and update times, sorted by name.
// Values are never returned. The slice is never nil.
func (s *Service) ListEnv(ctx context.Context, slug string) ([]EnvEntry, error) {
	vars, err := s.st.ListEnv(ctx, slug)
	if err != nil {
		return nil, err
	}
	out := make([]EnvEntry, 0, len(vars))
	for _, v := range vars {
		out = append(out, EnvEntry{Name: v.Name, UpdatedAt: v.UpdatedAt.UTC().Format(time.RFC3339)})
	}
	return out, nil
}

// Env returns the decrypted environment for the deployer; it is not exposed
// over RPC. It does NOT take the project lock, so it is safe to call from
// inside WithProject (which holds it) as well as from outside.
func (s *Service) Env(ctx context.Context, slug string) (map[string]string, error) {
	vars, err := s.st.ListEnv(ctx, slug)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(vars))
	for _, v := range vars {
		val, err := s.box.Open(v.ValueEnc, envAAD(slug, v.Name))
		if err != nil {
			return nil, fmt.Errorf("decrypt env %s: %w", v.Name, err)
		}
		out[v.Name] = string(val)
	}
	return out, nil
}

// WithProject runs fn holding the project lock with a freshly loaded row and
// the checkout directory. Inside fn use SetCommitLocked (not SetCommit) and
// Env; calling any method that takes the lock (SetCommit, SetEnv, UnsetEnv,
// Verify, Update, Delete) from fn deadlocks. Returns store.ErrNotFound for an
// unknown slug.
func (s *Service) WithProject(ctx context.Context, slug string, fn func(p *store.Project, dir string) error) error {
	unlock := s.lock(slug)
	defer unlock()
	p, err := s.st.GetProject(ctx, slug)
	if err != nil {
		return err
	}
	return fn(p, s.checkoutDir(slug))
}

// SetCommitLocked records commit on p and persists it. The caller must hold
// the project lock (i.e. be inside WithProject).
func (s *Service) SetCommitLocked(ctx context.Context, p *store.Project, commit string) error {
	p.Commit = commit
	return s.st.UpdateProject(ctx, p)
}

// SetCommit records commit on the project, taking the project lock itself. It
// must not be called from inside WithProject; use SetCommitLocked there.
func (s *Service) SetCommit(ctx context.Context, slug, commit string) error {
	return s.WithProject(ctx, slug, func(p *store.Project, _ string) error {
		return s.SetCommitLocked(ctx, p, commit)
	})
}

// Auth decrypts the repository credentials of p.
func (s *Service) Auth(p *store.Project) (git.Auth, error) { return s.authFor(p) }

// WebhookHook is what the webhook receiver needs to authenticate and route a
// delivery.
type WebhookHook struct {
	Slug       string
	Branch     string
	TagPattern string
	Status     string
	// Secret is the decrypted HMAC secret.
	Secret []byte
}

// WebhookHook looks up the project behind a webhook id. Unknown ids return
// store.ErrNotFound.
func (s *Service) WebhookHook(ctx context.Context, webhookID string) (WebhookHook, error) {
	p, err := s.st.GetProjectByWebhookID(ctx, webhookID)
	if err != nil {
		return WebhookHook{}, err
	}
	sec, err := s.box.Open(p.WebhookSecretEnc, aad(p.Slug, "webhook_secret"))
	if err != nil {
		return WebhookHook{}, fmt.Errorf("decrypt webhook secret: %w", err)
	}
	return WebhookHook{Slug: p.Slug, Branch: p.Branch, TagPattern: p.TagPattern, Status: p.Status, Secret: sec}, nil
}

// OnDelete registers fn to run at the start of every Delete, after the project
// is confirmed to exist but BEFORE the project lock is taken, so fn may wait
// for work that holds the lock (such as a running deploy) to stop. Hooks run
// in registration order with the Delete context; registration is safe at any
// time from any goroutine.
func (s *Service) OnDelete(fn func(ctx context.Context, slug string)) {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	s.onDelete = append(s.onDelete, fn)
}

func (s *Service) runOnDelete(ctx context.Context, slug string) {
	s.hookMu.Lock()
	hooks := append([]func(ctx context.Context, slug string){}, s.onDelete...)
	s.hookMu.Unlock()
	for _, fn := range hooks {
		fn(ctx, slug)
	}
}

// OnDeleted registers fn to run after a project has been deleted completely
// (containers, checkout and record gone). Hooks run in registration order
// with the Delete context and the project lock released; registration is safe
// at any time from any goroutine.
func (s *Service) OnDeleted(fn func(ctx context.Context, slug string)) {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	s.onDeleted = append(s.onDeleted, fn)
}

func (s *Service) runOnDeleted(ctx context.Context, slug string) {
	s.hookMu.Lock()
	hooks := append([]func(ctx context.Context, slug string){}, s.onDeleted...)
	s.hookMu.Unlock()
	for _, fn := range hooks {
		fn(ctx, slug)
	}
}
