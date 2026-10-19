// Package projects implements the projects.* JSON-RPC methods: registering
// Git repositories as projects, keeping their checkouts, and tearing them
// down. Service holds the logic and is usable without the RPC layer;
// Handlers maps it onto the wire.
package projects

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/open-southeners/ki-agent/internal/git"
	"github.com/open-southeners/ki-agent/internal/runtime"
	"github.com/open-southeners/ki-agent/internal/secret"
	"github.com/open-southeners/ki-agent/internal/store"
)

// Project statuses.
const (
	// StatusPending means the agent cannot read the repository yet (a new
	// deploy key still has to be authorised).
	StatusPending = "pending_access"
	// StatusReady means a checkout exists and a compose file was found.
	StatusReady = "ready"
)

// Authentication methods.
const (
	AuthNone       = "none"
	AuthSSHKey     = "ssh_key"
	AuthHTTPSToken = "https_token"
)

// DefaultGitTimeout bounds one create or verify (all its git calls together).
const DefaultGitTimeout = 5 * time.Minute

// composeCandidates are tried in order when no compose file is given.
var composeCandidates = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

var (
	slugRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	invalidSlugRe = regexp.MustCompile(`[^a-z0-9-]+`)
)

// InvalidError is a validation failure of the caller's input.
type InvalidError struct{ Msg string }

// Error implements error.
func (e *InvalidError) Error() string { return e.Msg }

func invalidf(format string, args ...any) error {
	return &InvalidError{Msg: fmt.Sprintf(format, args...)}
}

// Options configures a Service.
type Options struct {
	Store   *store.Store
	Secret  *secret.Box
	Git     *git.Client
	Runtime runtime.Runtime
	// DataDir holds projects/ and tmp/.
	DataDir string
	// Protocols are the URL schemes accepted for repositories; it must match
	// the git client's. Default: https and ssh.
	Protocols []string
	// GitTimeout bounds create and verify (default DefaultGitTimeout).
	GitTimeout time.Duration
	Logger     *slog.Logger
}

// Service creates, inspects, updates and deletes projects.
type Service struct {
	st        *store.Store
	box       *secret.Box
	git       *git.Client
	rt        runtime.Runtime
	projects  string
	tmp       string
	protocols []string
	timeout   time.Duration
	log       *slog.Logger

	mu    sync.Mutex
	locks map[string]*slugLock
}

type slugLock struct {
	mu   sync.Mutex
	refs int
}

// New returns a Service and creates its directories under DataDir.
func New(opts Options) (*Service, error) {
	if opts.Store == nil || opts.Secret == nil || opts.Git == nil || opts.Runtime == nil || opts.DataDir == "" {
		return nil, errors.New("projects: store, secret, git, runtime and data dir are required")
	}
	s := &Service{
		st:        opts.Store,
		box:       opts.Secret,
		git:       opts.Git,
		rt:        opts.Runtime,
		projects:  filepath.Join(opts.DataDir, "projects"),
		tmp:       filepath.Join(opts.DataDir, "tmp"),
		protocols: opts.Protocols,
		timeout:   opts.GitTimeout,
		log:       opts.Logger,
		locks:     make(map[string]*slugLock),
	}
	if len(s.protocols) == 0 {
		s.protocols = []string{"https", "ssh"}
	}
	if s.timeout <= 0 {
		s.timeout = DefaultGitTimeout
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	for _, d := range []string{s.projects, s.tmp} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("projects: create %s: %w", d, err)
		}
	}
	return s, nil
}

// Trigger says what a push must match to deploy: exactly one field is set
// once the trigger is known.
type Trigger struct {
	Branch     string `json:"branch,omitempty"`
	TagPattern string `json:"tag_pattern,omitempty"`
}

// Auth describes how the agent authenticates to the repository. Secrets are
// never included.
type Auth struct {
	Method    string `json:"method"`
	PublicKey string `json:"public_key,omitempty"`
	TokenUser string `json:"token_user,omitempty"`
}

// Webhook locates the project's webhook endpoint.
type Webhook struct {
	Path string `json:"path"`
}

// Project is the wire view of a project.
type Project struct {
	Slug          string  `json:"slug"`
	RepoURL       string  `json:"repo_url"`
	Trigger       Trigger `json:"trigger"`
	DefaultBranch string  `json:"default_branch"`
	ComposeFile   string  `json:"compose_file"`
	Auth          Auth    `json:"auth"`
	Webhook       Webhook `json:"webhook"`
	Status        string  `json:"status"`
	StatusDetail  string  `json:"status_detail"`
	Commit        string  `json:"commit"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

func view(p *store.Project) Project {
	v := Project{
		Slug:          p.Slug,
		RepoURL:       p.RepoURL,
		Trigger:       Trigger{Branch: p.Branch, TagPattern: p.TagPattern},
		DefaultBranch: p.DefaultBranch,
		ComposeFile:   p.ComposeFile,
		Auth:          Auth{Method: p.AuthMethod},
		Webhook:       Webhook{Path: "/hooks/" + p.WebhookID},
		Status:        p.Status,
		StatusDetail:  p.StatusDetail,
		Commit:        p.Commit,
		CreatedAt:     p.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:     p.UpdatedAt.UTC().Format(time.RFC3339),
	}
	switch p.AuthMethod {
	case AuthSSHKey:
		v.Auth.PublicKey = p.PublicKey
	case AuthHTTPSToken:
		v.Auth.TokenUser = p.TokenUser
	}
	return v
}

// CreateParams are the arguments of projects.create.
type CreateParams struct {
	RepoURL     string `json:"repo_url"`
	Slug        string `json:"slug"`
	Branch      string `json:"branch"`
	TagPattern  string `json:"tag_pattern"`
	ComposeFile string `json:"compose_file"`
	Token       string `json:"token"`
	TokenUser   string `json:"token_user"`
}

// UpdateParams are the arguments of projects.update.
type UpdateParams struct {
	Slug                string `json:"slug"`
	Branch              string `json:"branch"`
	TagPattern          string `json:"tag_pattern"`
	ComposeFile         string `json:"compose_file"`
	Token               string `json:"token"`
	TokenUser           string `json:"token_user"`
	RotateWebhookSecret bool   `json:"rotate_webhook_secret"`
	RotateDeployKey     bool   `json:"rotate_deploy_key"`
}

// Result is returned by create and update. WebhookSecret is set only when it
// was just generated.
type Result struct {
	Project       Project `json:"project"`
	WebhookSecret string  `json:"webhook_secret,omitempty"`
}

// lock serialises operations on one project and returns the unlock func.
func (s *Service) lock(slug string) func() {
	s.mu.Lock()
	l := s.locks[slug]
	if l == nil {
		l = &slugLock{}
		s.locks[slug] = l
	}
	l.refs++
	s.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(s.locks, slug)
		}
		s.mu.Unlock()
	}
}

func (s *Service) checkoutDir(slug string) string { return filepath.Join(s.projects, slug) }

// DeriveSlug turns a repository name into a valid slug ("" if nothing usable
// remains).
func DeriveSlug(name string) string {
	slug := invalidSlugRe.ReplaceAllString(strings.ToLower(name), "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > 63 {
		slug = strings.Trim(slug[:63], "-")
	}
	return slug
}

func validateTrigger(branch, tagPattern string) error {
	if branch != "" && tagPattern != "" {
		return invalidf("branch and tag_pattern are mutually exclusive")
	}
	if tagPattern != "" {
		if _, err := path.Match(tagPattern, "v1"); err != nil {
			return invalidf("tag_pattern is not a valid glob: %v", err)
		}
	}
	return nil
}

// cleanComposePath validates a compose file path lexically and returns it
// normalised with forward slashes.
func cleanComposePath(p string) (string, error) {
	if strings.ContainsRune(p, 0) || strings.Contains(p, "\\") {
		return "", invalidf("compose_file contains invalid characters")
	}
	if path.IsAbs(p) {
		return "", invalidf("compose_file must be relative to the repository")
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", invalidf("compose_file must stay inside the repository")
	}
	if c == ".git" || strings.HasPrefix(c, ".git/") {
		return "", invalidf("compose_file must not point into .git")
	}
	return c, nil
}

// resolveCompose finds the compose file in the checkout at root: the explicit
// path if given, otherwise the first candidate present. Symlinks must resolve
// inside root.
func resolveCompose(root, explicit string) (string, error) {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve checkout: %w", err)
	}
	probe := func(rel string) (bool, error) {
		real, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("resolve %s: %w", rel, err)
		}
		if r, err := filepath.Rel(rootReal, real); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return false, invalidf("compose_file %q resolves outside the repository", rel)
		}
		fi, err := os.Stat(real)
		if err != nil || !fi.Mode().IsRegular() {
			return false, nil
		}
		return true, nil
	}
	if explicit != "" {
		rel, err := cleanComposePath(explicit)
		if err != nil {
			return "", err
		}
		ok, err := probe(rel)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", &git.Error{Reason: git.ReasonComposeNotFound, Detail: fmt.Sprintf("compose file %q not found in the repository", rel)}
		}
		return rel, nil
	}
	for _, c := range composeCandidates {
		if ok, err := probe(c); err == nil && ok {
			return c, nil
		}
	}
	return "", &git.Error{Reason: git.ReasonComposeNotFound, Detail: "no compose.yaml, compose.yml, docker-compose.yaml or docker-compose.yml in the repository"}
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func aad(slug, field string) []byte { return []byte(slug + "/" + field) }

// gitCtx applies the git operation timeout.
func (s *Service) gitCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.timeout)
}

// timedOut converts a git-operation deadline into a repository error; other
// errors (including caller cancellation) pass through.
func (s *Service) timedOut(parent context.Context, err error) error {
	if err != nil && errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil {
		return &git.Error{Reason: git.ReasonFailed, Detail: "timed out after " + s.timeout.String()}
	}
	return err
}

// authFor decrypts the stored credentials of p.
func (s *Service) authFor(p *store.Project) (git.Auth, error) {
	switch p.AuthMethod {
	case AuthSSHKey:
		key, err := s.box.Open(p.PrivateKeyEnc, aad(p.Slug, "private_key"))
		if err != nil {
			return git.Auth{}, fmt.Errorf("decrypt deploy key: %w", err)
		}
		return git.Auth{SSHPrivateKey: key}, nil
	case AuthHTTPSToken:
		tok, err := s.box.Open(p.TokenEnc, aad(p.Slug, "token"))
		if err != nil {
			return git.Auth{}, fmt.Errorf("decrypt token: %w", err)
		}
		return git.Auth{Token: string(tok), TokenUser: p.TokenUser}, nil
	}
	return git.Auth{}, nil
}

// staged is a fresh clone waiting to be moved into place.
type staged struct {
	parent, root, commit, compose string
}

// discard removes the staging directory.
func (g *staged) discard() { _ = os.RemoveAll(g.parent) }

// stage clones branch into a staging directory under tmp/ and locates the
// compose file. On error nothing is left behind.
func (s *Service) stage(ctx context.Context, repoURL string, auth git.Auth, branch, composeFile string) (*staged, error) {
	parent, err := os.MkdirTemp(s.tmp, "clone-*")
	if err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	g := &staged{parent: parent, root: filepath.Join(parent, "checkout")}
	if g.commit, err = s.git.Sync(ctx, repoURL, auth, branch, g.root); err == nil {
		g.compose, err = resolveCompose(g.root, composeFile)
	}
	if err != nil {
		g.discard()
		return nil, err
	}
	return g, nil
}

// install moves the staged checkout to projects/<slug>, replacing any
// leftover directory (a project row never exists for it: callers hold the
// slug lock and have checked).
func (s *Service) install(g *staged, slug string) error {
	dst := s.checkoutDir(slug)
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("clear %s: %w", dst, err)
	}
	if err := os.Rename(g.root, dst); err != nil {
		return fmt.Errorf("install checkout: %w", err)
	}
	g.discard()
	return nil
}

// Create registers a project. For none and https_token auth the repository is
// cloned first and nothing is persisted on failure; for ssh_key a deploy key
// is generated and the project waits in pending_access.
func (s *Service) Create(ctx context.Context, in CreateParams) (*Result, error) {
	u, err := git.ParseURL(in.RepoURL, s.protocols)
	if err != nil {
		return nil, err
	}
	slug := in.Slug
	if slug == "" {
		if slug = DeriveSlug(u.Name); slug == "" {
			return nil, invalidf("cannot derive a slug from the repository name; pass slug")
		}
	}
	if !slugRe.MatchString(slug) {
		return nil, invalidf("slug must match %s", slugRe)
	}
	if err := validateTrigger(in.Branch, in.TagPattern); err != nil {
		return nil, err
	}
	compose := ""
	if in.ComposeFile != "" {
		if compose, err = cleanComposePath(in.ComposeFile); err != nil {
			return nil, err
		}
	}
	if in.TokenUser != "" && in.Token == "" {
		return nil, invalidf("token_user requires token")
	}

	p := &store.Project{
		Slug: slug, RepoURL: in.RepoURL, Branch: in.Branch, TagPattern: in.TagPattern,
		ComposeFile: compose,
	}
	switch {
	case in.Token != "":
		if u.Scheme != "https" {
			return nil, invalidf("token authentication requires an https repository URL")
		}
		p.AuthMethod = AuthHTTPSToken
		p.TokenUser = in.TokenUser
		if p.TokenUser == "" {
			p.TokenUser = git.DefaultTokenUser
		}
		p.TokenEnc = s.box.Seal([]byte(in.Token), aad(slug, "token"))
	case u.Scheme == "ssh":
		p.AuthMethod = AuthSSHKey
	default:
		p.AuthMethod = AuthNone
	}

	unlock := s.lock(slug)
	defer unlock()
	if _, err := s.st.GetProject(ctx, slug); err == nil {
		return nil, store.ErrExists
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	if p.WebhookID, err = randHex(16); err != nil {
		return nil, err
	}
	whSecret, err := randHex(32)
	if err != nil {
		return nil, err
	}
	p.WebhookSecretEnc = s.box.Seal([]byte(whSecret), aad(slug, "webhook_secret"))

	var checkout *staged
	if p.AuthMethod == AuthSSHKey {
		pemKey, pub, err := git.GenerateDeployKey("ki-agent:" + slug)
		if err != nil {
			return nil, err
		}
		p.PublicKey = pub
		p.PrivateKeyEnc = s.box.Seal(pemKey, aad(slug, "private_key"))
		p.Status = StatusPending
	} else {
		gctx, cancel := s.gitCtx(ctx)
		defer cancel()
		auth, err := s.authFor(p)
		if err != nil {
			return nil, err
		}
		if p.DefaultBranch, err = s.git.DefaultBranch(gctx, p.RepoURL, auth); err != nil {
			return nil, s.timedOut(ctx, err)
		}
		branch := p.Branch
		if branch == "" {
			branch = p.DefaultBranch
			if p.TagPattern == "" {
				p.Branch = branch
			}
		}
		if checkout, err = s.stage(gctx, p.RepoURL, auth, branch, p.ComposeFile); err != nil {
			return nil, s.timedOut(ctx, err)
		}
		defer checkout.discard()
		p.ComposeFile, p.Commit, p.Status = checkout.compose, checkout.commit, StatusReady
		if err := s.install(checkout, slug); err != nil {
			return nil, err
		}
	}
	if err := s.st.CreateProject(ctx, p); err != nil {
		if checkout != nil {
			_ = os.RemoveAll(s.checkoutDir(slug))
		}
		return nil, err
	}
	s.log.Info("project created", "slug", slug, "auth", p.AuthMethod, "status", p.Status)
	return &Result{Project: view(p), WebhookSecret: whSecret}, nil
}

// Get returns one project.
func (s *Service) Get(ctx context.Context, slug string) (*Project, error) {
	p, err := s.st.GetProject(ctx, slug)
	if err != nil {
		return nil, err
	}
	v := view(p)
	return &v, nil
}

// List returns all projects sorted by slug (never nil).
func (s *Service) List(ctx context.Context) ([]Project, error) {
	ps, err := s.st.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(ps))
	for _, p := range ps {
		out = append(out, view(p))
	}
	return out, nil
}

// Verify probes the repository with the stored credentials, brings the
// checkout up to date and marks the project ready. On a repository failure
// status_detail records it (the status itself is unchanged) and the error is
// returned.
func (s *Service) Verify(ctx context.Context, slug string) (*Project, error) {
	unlock := s.lock(slug)
	defer unlock()
	p, err := s.st.GetProject(ctx, slug)
	if err != nil {
		return nil, err
	}
	if err := s.verify(ctx, p); err != nil {
		var ge *git.Error
		if errors.As(err, &ge) {
			p.StatusDetail = ge.Reason
			if ge.Detail != "" {
				p.StatusDetail += ": " + ge.Detail
			}
			if uerr := s.st.UpdateProject(ctx, p); uerr != nil {
				s.log.Warn("record verify failure", "slug", slug, "err", uerr)
			}
		}
		return nil, err
	}
	if err := s.st.UpdateProject(ctx, p); err != nil {
		return nil, err
	}
	s.log.Info("project verified", "slug", slug, "commit", p.Commit)
	v := view(p)
	return &v, nil
}

// verify mutates p in memory to the verified state; the caller persists it.
func (s *Service) verify(ctx context.Context, p *store.Project) error {
	auth, err := s.authFor(p)
	if err != nil {
		return err
	}
	gctx, cancel := s.gitCtx(ctx)
	defer cancel()
	def, err := s.git.DefaultBranch(gctx, p.RepoURL, auth)
	if err != nil {
		return s.timedOut(ctx, err)
	}
	p.DefaultBranch = def
	branch := p.Branch
	if branch == "" {
		branch = def
		if p.TagPattern == "" {
			p.Branch = def
		}
	}
	dir := s.checkoutDir(p.Slug)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		g, err := s.stage(gctx, p.RepoURL, auth, branch, p.ComposeFile)
		if err != nil {
			return s.timedOut(ctx, err)
		}
		defer g.discard()
		if err := s.install(g, p.Slug); err != nil {
			return err
		}
		p.Commit, p.ComposeFile = g.commit, g.compose
	} else if err != nil {
		return fmt.Errorf("stat checkout: %w", err)
	} else {
		commit, err := s.git.Sync(gctx, p.RepoURL, auth, branch, dir)
		if err != nil {
			return s.timedOut(ctx, err)
		}
		p.Commit = commit
		if p.ComposeFile, err = resolveCompose(dir, p.ComposeFile); err != nil {
			return err
		}
	}
	p.Status, p.StatusDetail = StatusReady, ""
	return nil
}

// Update changes a project's trigger, compose file or credentials, and can
// rotate the webhook secret or deploy key. The repository URL is immutable.
func (s *Service) Update(ctx context.Context, in UpdateParams) (*Result, error) {
	if err := validateTrigger(in.Branch, in.TagPattern); err != nil {
		return nil, err
	}
	compose := ""
	if in.ComposeFile != "" {
		var err error
		if compose, err = cleanComposePath(in.ComposeFile); err != nil {
			return nil, err
		}
	}
	unlock := s.lock(in.Slug)
	defer unlock()
	p, err := s.st.GetProject(ctx, in.Slug)
	if err != nil {
		return nil, err
	}
	if (in.Token != "" || in.TokenUser != "") && p.AuthMethod != AuthHTTPSToken {
		return nil, invalidf("token and token_user apply only to https_token projects")
	}
	if in.RotateDeployKey && p.AuthMethod != AuthSSHKey {
		return nil, invalidf("rotate_deploy_key applies only to ssh_key projects")
	}

	if in.Branch != "" {
		p.Branch, p.TagPattern = in.Branch, ""
	} else if in.TagPattern != "" {
		p.Branch, p.TagPattern = "", in.TagPattern
	}
	if compose != "" {
		if _, err := os.Stat(s.checkoutDir(p.Slug)); err == nil {
			if compose, err = resolveCompose(s.checkoutDir(p.Slug), compose); err != nil {
				return nil, err
			}
		}
		p.ComposeFile = compose
	}
	if in.Token != "" {
		p.TokenEnc = s.box.Seal([]byte(in.Token), aad(p.Slug, "token"))
	}
	if in.TokenUser != "" {
		p.TokenUser = in.TokenUser
	}
	res := &Result{}
	if in.RotateWebhookSecret {
		if res.WebhookSecret, err = randHex(32); err != nil {
			return nil, err
		}
		p.WebhookSecretEnc = s.box.Seal([]byte(res.WebhookSecret), aad(p.Slug, "webhook_secret"))
	}
	if in.RotateDeployKey {
		pemKey, pub, err := git.GenerateDeployKey("ki-agent:" + p.Slug)
		if err != nil {
			return nil, err
		}
		p.PublicKey = pub
		p.PrivateKeyEnc = s.box.Seal(pemKey, aad(p.Slug, "private_key"))
		p.Status, p.StatusDetail = StatusPending, ""
	}
	if err := s.st.UpdateProject(ctx, p); err != nil {
		return nil, err
	}
	s.log.Info("project updated", "slug", p.Slug,
		"rotated_webhook_secret", in.RotateWebhookSecret, "rotated_deploy_key", in.RotateDeployKey)
	res.Project = view(p)
	return res, nil
}

// Delete removes the project's containers, networks and (optionally) volumes,
// then its checkout, then its record. If the runtime is unavailable nothing
// is removed.
func (s *Service) Delete(ctx context.Context, slug string, volumes bool) (runtime.PruneReport, error) {
	unlock := s.lock(slug)
	defer unlock()
	if _, err := s.st.GetProject(ctx, slug); err != nil {
		return runtime.PruneReport{}, err
	}
	rep, err := s.rt.PruneProject(ctx, slug, volumes)
	if err != nil {
		return runtime.PruneReport{}, err
	}
	if err := os.RemoveAll(s.checkoutDir(slug)); err != nil {
		return runtime.PruneReport{}, fmt.Errorf("remove checkout: %w", err)
	}
	if err := s.st.DeleteProject(ctx, slug); err != nil {
		return runtime.PruneReport{}, err
	}
	s.log.Info("project deleted", "slug", slug, "containers", rep.Containers,
		"networks", rep.Networks, "volumes", rep.Volumes)
	return rep, nil
}
