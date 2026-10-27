package projects

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/open-southeners/ki/internal/store"
)

// Project sources.
const (
	SourceGit     = "git"
	SourceCompose = "compose"
)

// KeepComposeVersions is how many compose versions a project keeps; the
// version of the latest successful deploy is kept regardless.
const KeepComposeVersions = 50

var errNotGit = invalidf("not a git project")

// ErrVersionNotFound is returned for a compose version that does not exist (or
// was pruned) on an existing project.
var ErrVersionNotFound = errors.New("compose version not found")

// SetOnComposeSet installs the hook projects.compose.set with deploy:true
// calls (see Options.OnComposeSet). Call it before serving requests.
func (s *Service) SetOnComposeSet(fn func(ctx context.Context, slug string, version int) (any, error)) {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	s.onComposeSet = fn
}

// ComposeVersionInfo is one stored compose version (projects.compose.get).
type ComposeVersionInfo struct {
	Version   int    `json:"version"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
	CreatedBy string `json:"created_by"`
	Note      string `json:"note,omitempty"`
}

// ComposeHistoryEntry is one row of projects.compose.history.
type ComposeHistoryEntry struct {
	Version   int    `json:"version"`
	CreatedAt string `json:"created_at"`
	CreatedBy string `json:"created_by"`
	Note      string `json:"note,omitempty"`
	// Deployed is true when a succeeded deploy ran this version.
	Deployed bool `json:"deployed"`
}

// validate runs the configured compose validator. A rejection comes back as
// the *compose.ValidationError itself.
func (s *Service) validate(ctx context.Context, slug, content string) error {
	if s.validateCompose == nil {
		return errors.New("compose validation is not configured")
	}
	ve, err := s.validateCompose(ctx, slug, content)
	if err != nil {
		return err
	}
	if ve != nil {
		return ve
	}
	return nil
}

// createCompose registers a compose project: validated content as version 1,
// an empty project directory, no repository, trigger, credentials or reachable
// webhook (the webhook id is random and never published).
func (s *Service) createCompose(ctx context.Context, in CreateParams) (*Result, error) {
	if in.Slug == "" {
		return nil, invalidf("slug is required for compose projects")
	}
	if !slugRe.MatchString(in.Slug) {
		return nil, invalidf("slug must match %s", slugRe)
	}
	if in.Branch != "" || in.TagPattern != "" || in.ComposeFile != "" || in.Token != "" || in.TokenUser != "" {
		return nil, invalidf("branch, tag_pattern, compose_file, token and token_user apply only to git projects")
	}
	if err := s.validate(ctx, in.Slug, in.Compose); err != nil {
		return nil, err
	}

	unlock := s.lock(in.Slug)
	defer unlock()
	if _, err := s.st.GetProject(ctx, in.Slug); err == nil {
		return nil, store.ErrExists
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	whID, err := randHex(16)
	if err != nil {
		return nil, err
	}
	whSecret, err := randHex(32) // never revealed: compose projects have no webhook
	if err != nil {
		return nil, err
	}
	p := &store.Project{
		Slug: in.Slug, Source: SourceCompose, AuthMethod: AuthNone, Status: StatusReady,
		WebhookID:        whID,
		WebhookSecretEnc: s.box.Seal([]byte(whSecret), aad(in.Slug, "webhook_secret")),
	}
	dir := s.checkoutDir(in.Slug)
	if err := os.RemoveAll(dir); err != nil { // leftover of a project that no longer exists
		return nil, fmt.Errorf("clear %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create project dir: %w", err)
	}
	if err := s.st.CreateProject(ctx, p); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	version, err := s.st.AddComposeVersion(ctx, in.Slug, in.Compose, in.CreatedBy, in.note)
	if err != nil {
		_ = s.st.DeleteProject(ctx, in.Slug)
		_ = os.RemoveAll(dir)
		return nil, err
	}
	s.log.Info("project created", "slug", in.Slug, "source", SourceCompose, "version", version)
	v := view(p)
	v.ComposeVersion = version
	return &Result{Project: v}, nil
}

// composeProject loads the project and requires it to be a compose project.
func (s *Service) composeProject(ctx context.Context, slug string) error {
	p, err := s.st.GetProject(ctx, slug)
	if err != nil {
		return err
	}
	if p.Source != SourceCompose {
		return invalidf("not a compose project")
	}
	return nil
}

// ComposeGet returns a stored compose version (0 = latest).
func (s *Service) ComposeGet(ctx context.Context, slug string, version int) (*ComposeVersionInfo, error) {
	if version < 0 {
		return nil, invalidf("version must not be negative")
	}
	if err := s.composeProject(ctx, slug); err != nil {
		return nil, err
	}
	cv, err := s.st.GetComposeVersion(ctx, slug, version)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrVersionNotFound // the project exists: composeProject checked
	}
	if err != nil {
		return nil, err
	}
	return &ComposeVersionInfo{
		Version: cv.Version, Content: cv.Content, CreatedBy: cv.CreatedBy, Note: cv.Note,
		CreatedAt: cv.CreatedAt.UTC().Format(time.RFC3339),
	}, nil
}

// ComposeSet validates content and stores it as the project's next version,
// then prunes old versions. It returns the new version.
func (s *Service) ComposeSet(ctx context.Context, slug, content, note, createdBy string) (int, error) {
	if err := s.composeProject(ctx, slug); err != nil {
		return 0, err
	}
	if err := s.validate(ctx, slug, content); err != nil {
		return 0, err
	}
	unlock := s.lock(slug)
	defer unlock()
	version, err := s.st.AddComposeVersion(ctx, slug, content, createdBy, note)
	if err != nil {
		return 0, err
	}
	protect, err := s.st.LatestDeployedComposeVersion(ctx, slug)
	if err == nil {
		_, err = s.st.PruneComposeVersions(ctx, slug, KeepComposeVersions, protect)
	}
	if err != nil {
		s.log.Warn("prune compose versions", "slug", slug, "err", err)
	}
	s.log.Info("compose file updated", "slug", slug, "version", version, "by", createdBy)
	return version, nil
}

// ComposeHistory lists the stored versions newest first; limit <= 0 means all.
func (s *Service) ComposeHistory(ctx context.Context, slug string, limit int) ([]ComposeHistoryEntry, error) {
	if err := s.composeProject(ctx, slug); err != nil {
		return nil, err
	}
	vs, err := s.st.ListComposeVersions(ctx, slug, limit)
	if err != nil {
		return nil, err
	}
	deployed, err := s.st.DeployedComposeVersions(ctx, slug)
	if err != nil {
		return nil, err
	}
	out := make([]ComposeHistoryEntry, 0, len(vs))
	for _, v := range vs {
		out = append(out, ComposeHistoryEntry{
			Version: v.Version, CreatedBy: v.CreatedBy, Note: v.Note, Deployed: deployed[v.Version],
			CreatedAt: v.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// ComposeSetResult is the result of projects.compose.set.
type ComposeSetResult struct {
	Version int `json:"version"`
	// Deploy is what the OnComposeSet hook returned (set when deploy was
	// requested).
	Deploy any `json:"deploy,omitempty"`
}

// ComposeSetAndDeploy is ComposeSet plus, when deploy is true, the
// OnComposeSet hook for the new version. If the hook fails the version stays
// stored and the error is returned.
func (s *Service) ComposeSetAndDeploy(ctx context.Context, slug, content, note, createdBy string, deploy bool) (*ComposeSetResult, error) {
	s.hookMu.Lock()
	hook := s.onComposeSet
	s.hookMu.Unlock()
	if deploy && hook == nil {
		return nil, errors.New("deploys are not available")
	}
	version, err := s.ComposeSet(ctx, slug, content, note, createdBy)
	if err != nil {
		return nil, err
	}
	res := &ComposeSetResult{Version: version}
	if deploy {
		if res.Deploy, err = hook(ctx, slug, version); err != nil {
			return nil, err
		}
	}
	return res, nil
}

type viaCLIKey struct{}

// ViaCLI reports whether ctx belongs to a request made over the local CLI
// socket (set for projects.compose.set), so the deploy it triggers is
// attributed to the CLI.
func ViaCLI(ctx context.Context) bool {
	v, _ := ctx.Value(viaCLIKey{}).(bool)
	return v
}
