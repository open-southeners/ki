// Package webhooks serves the push webhooks (GitHub and GitLab) that trigger
// deploys. Requests are authenticated against the per-project secret over the
// raw body before anything is decoded.
package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/open-southeners/ki/internal/store"
)

const (
	// DefaultMaxBodyBytes is the request body cap when none is configured.
	DefaultMaxBodyBytes int64 = 5 << 20

	// SourceWebhook is the deploy source recorded for webhook triggers.
	SourceWebhook = "webhook"

	statusPendingAccess = "pending_access"
	hooksPrefix         = "/hooks/"
	zeroSHA             = "0000000000000000000000000000000000000000"
	dedupeEntries       = 1024
	dedupeTTL           = 24 * time.Hour
)

// Hook is what the handler needs to know about the project behind a webhook
// id: its trigger, readiness and the plain webhook secret.
type Hook struct {
	Slug       string
	Branch     string // branch trigger; empty for tag triggers
	TagPattern string // tag trigger glob (path.Match); empty for branch triggers
	Status     string
	Secret     []byte
}

// Projects resolves webhook ids.
type Projects interface {
	// ByWebhookID returns the hook for id, or store.ErrNotFound when unknown.
	ByWebhookID(ctx context.Context, id string) (Hook, error)
}

// TriggerRequest asks the deployer for a deploy of Ref pinned to Commit.
type TriggerRequest struct {
	Project string
	Ref     string
	Commit  string
	Source  string
}

// Deployer starts deploys.
type Deployer interface {
	// Trigger queues a deploy and returns its id.
	Trigger(ctx context.Context, req TriggerRequest) (int64, error)
}

// Options configures a Handler.
type Options struct {
	Projects     Projects
	Deployer     Deployer
	MaxBodyBytes int64 // <= 0 uses DefaultMaxBodyBytes
	Logger       *slog.Logger
}

// Handler serves POST /hooks/<webhook_id>; everything else is a 404.
type Handler struct {
	projects Projects
	deployer Deployer
	max      int64
	log      *slog.Logger
	seen     *dedupe
}

// NewHandler builds a Handler from opts.
func NewHandler(opts Options) *Handler {
	h := &Handler{
		projects: opts.Projects,
		deployer: opts.Deployer,
		max:      opts.MaxBodyBytes,
		log:      opts.Logger,
		seen:     newDedupe(dedupeEntries, dedupeTTL),
	}
	if h.max <= 0 {
		h.max = DefaultMaxBodyBytes
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	return h
}

// push is the subset of a push payload we read. checkout_sha is GitLab's
// commit a (possibly annotated) tag resolves to.
type push struct {
	Ref         string `json:"ref"`
	After       string `json:"after"`
	CheckoutSHA string `json:"checkout_sha"`
	Deleted     bool   `json:"deleted"`
	HeadCommit  *struct {
		ID string `json:"id"`
	} `json:"head_commit"`
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutPrefix(r.URL.Path, hooksPrefix)
	if r.Method != http.MethodPost || !ok || id == "" || strings.Contains(id, "/") {
		writeJSON(w, http.StatusNotFound, "not found")
		return
	}
	hook, err := h.projects.ByWebhookID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, "not found")
			return
		}
		h.log.Error("webhook lookup failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, "internal error")
		return
	}

	github := r.Header.Get("X-GitHub-Event")
	gitlab := r.Header.Get("X-Gitlab-Event")
	if github == "" && gitlab == "" {
		writeJSON(w, http.StatusBadRequest, "unrecognized webhook source")
		return
	}

	// The signature covers the raw bytes, so read them all before decoding.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.max))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		writeJSON(w, http.StatusBadRequest, "unreadable body")
		return
	}

	var event, delivery string
	if github != "" {
		event, delivery = github, r.Header.Get("X-GitHub-Delivery")
		if !validGitHubSignature(hook.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
			h.unauthorized(hook.Slug, event)
			writeJSON(w, http.StatusUnauthorized, "invalid signature")
			return
		}
		if event == "ping" {
			h.log.Info("webhook", "project", hook.Slug, "event", event, "decision", "pong")
			writeJSON(w, http.StatusOK, "")
			return
		}
		if event != "push" {
			h.ignored(w, hook.Slug, event, "", "unsupported event")
			return
		}
	} else {
		event, delivery = gitlab, r.Header.Get("X-Gitlab-Event-UUID")
		token := r.Header.Get("X-Gitlab-Token")
		if len(hook.Secret) == 0 || subtle.ConstantTimeCompare([]byte(token), hook.Secret) != 1 {
			h.unauthorized(hook.Slug, event)
			writeJSON(w, http.StatusUnauthorized, "invalid token")
			return
		}
		if event != "Push Hook" && event != "Tag Push Hook" {
			h.ignored(w, hook.Slug, event, "", "unsupported event")
			return
		}
	}

	dkey := ""
	if delivery != "" {
		dkey = hook.Slug + "\x00" + delivery
		if h.seen.has(dkey) {
			h.log.Info("webhook", "project", hook.Slug, "event", event, "decision", "duplicate")
			writeJSON(w, http.StatusOK, "duplicate")
			return
		}
	}

	var p push
	if err := json.Unmarshal(body, &p); err != nil {
		writeJSON(w, http.StatusBadRequest, "invalid payload")
		return
	}
	commit := p.After
	if github == "" && p.CheckoutSHA != "" {
		commit = p.CheckoutSHA
	}
	if p.Deleted || p.Ref == "" || p.After == "" || isZero(p.After) || isZero(commit) {
		h.ignored(w, hook.Slug, event, p.Ref, "deletion or empty push")
		return
	}
	if github != "" && strings.HasPrefix(p.Ref, "refs/tags/") {
		// GitHub's after is the tag object for annotated tags; only
		// head_commit.id is the tagged commit. Without it the deployer
		// resolves the tag itself.
		commit = ""
		if p.HeadCommit != nil {
			commit = p.HeadCommit.ID
		}
	}
	if !matchesTrigger(hook, p.Ref) {
		h.ignored(w, hook.Slug, event, p.Ref, "ref does not match trigger")
		return
	}
	if hook.Status == statusPendingAccess {
		h.log.Info("webhook", "project", hook.Slug, "event", event, "ref", p.Ref, "decision", "project not ready")
		writeJSON(w, http.StatusConflict, "project not ready")
		return
	}

	deployID, err := h.deployer.Trigger(r.Context(), TriggerRequest{
		Project: hook.Slug, Ref: p.Ref, Commit: commit, Source: SourceWebhook,
	})
	if err != nil {
		h.log.Error("webhook trigger failed", "project", hook.Slug, "event", event, "ref", p.Ref, "err", err)
		writeJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	if dkey != "" {
		h.seen.add(dkey)
	}
	h.log.Info("webhook", "project", hook.Slug, "event", event, "ref", p.Ref, "decision", "deploy", "deploy", deployID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]int64{"deploy": deployID})
}

func (h *Handler) unauthorized(slug, event string) {
	h.log.Warn("webhook rejected", "project", slug, "event", event, "decision", "unauthorized")
}

func (h *Handler) ignored(w http.ResponseWriter, slug, event, ref, why string) {
	h.log.Info("webhook", "project", slug, "event", event, "ref", ref, "decision", "ignored", "reason", why)
	writeJSON(w, http.StatusAccepted, "ignored")
}

// writeJSON writes a small JSON body: {"ok":true} for an empty msg on 200,
// {"ignored":true} / {"duplicate":true} for those messages, else {"error":msg}.
func writeJSON(w http.ResponseWriter, code int, msg string) {
	var v any
	switch {
	case code == http.StatusOK && msg == "":
		v = map[string]bool{"ok": true}
	case msg == "ignored":
		v = map[string]bool{"ignored": true}
	case msg == "duplicate":
		v = map[string]bool{"duplicate": true}
	default:
		v = map[string]string{"error": msg}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// validGitHubSignature checks "sha256=<hex>" against the HMAC-SHA256 of body.
func validGitHubSignature(secret, body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok || len(secret) == 0 {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func isZero(sha string) bool {
	return sha != "" && strings.Trim(sha, "0") == ""
}

// matchesTrigger reports whether ref is the project's trigger branch or a tag
// matching its pattern.
func matchesTrigger(hook Hook, ref string) bool {
	if hook.Branch != "" {
		return ref == "refs/heads/"+hook.Branch
	}
	if hook.TagPattern != "" {
		tag, ok := strings.CutPrefix(ref, "refs/tags/")
		if !ok || tag == "" {
			return false
		}
		m, err := path.Match(hook.TagPattern, tag)
		return err == nil && m
	}
	return false
}
