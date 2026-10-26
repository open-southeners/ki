// Package accounts implements users, setup links, login, sessions, API tokens
// and the audit log, and exposes them as RPC methods and as the authenticator
// used by the HTTP and WebSocket transports.
package accounts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/open-southeners/ki-agent/internal/auth"
	"github.com/open-southeners/ki-agent/internal/secret"
	"github.com/open-southeners/ki-agent/internal/store"
)

// Defaults and limits.
const (
	DefaultSessionIdle = 12 * time.Hour
	DefaultSessionMax  = 7 * 24 * time.Hour
	// SetupTTL is how long a setup link stays valid.
	SetupTTL = 30 * time.Minute
	// MaxTokenLifetime is the longest expires_in accepted by tokens.create.
	MaxTokenLifetime = 365 * 24 * time.Hour

	touchInterval = time.Minute
	cleanupEvery  = 10 * time.Minute
	auditKeep     = 10000
	issuer        = "ki-agent"
)

// Errors returned by the Service. Authentication failures carry no detail.
var (
	// ErrUnauthenticated is returned by Session and Token for any failure.
	ErrUnauthenticated = errors.New("accounts: unauthenticated")
	// ErrInvalidCredentials is returned by Login for any credential failure.
	ErrInvalidCredentials = errors.New("accounts: invalid credentials")
	// ErrRateLimited is matched (errors.Is) by *RateLimitedError.
	ErrRateLimited = errors.New("accounts: too many attempts")
	// ErrInvalidSetup is returned for any bad, expired or used setup token or code.
	ErrInvalidSetup = errors.New("accounts: invalid setup link")
	// ErrWeakPassword is matched by password policy errors; their message is
	// safe to show.
	ErrWeakPassword = errors.New("accounts: password does not meet the policy")
)

// RateLimitedError reports a client blocked by the per-IP limiter.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string { return ErrRateLimited.Error() }

// Unwrap lets errors.Is(err, ErrRateLimited) match.
func (e *RateLimitedError) Unwrap() error { return ErrRateLimited }

// paramError is a caller mistake, reported as InvalidParams.
type paramError struct{ msg string }

func (e *paramError) Error() string { return e.msg }

func invalidf(format string, args ...any) error {
	return &paramError{msg: fmt.Sprintf(format, args...)}
}

// Options configure a Service.
type Options struct {
	Store  *store.Store
	Secret *secret.Box
	Logger *slog.Logger
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
	// SetupBase returns the base URL setup links are built on and, when the host
	// is an IP literal, the TLS certificate's SHA-256 fingerprint ("" otherwise).
	SetupBase func() (base, fingerprint string)
	// SessionIdle and SessionMax default to 12 h and 7 d.
	SessionIdle, SessionMax time.Duration
}

// Service holds the account logic. It is safe for concurrent use.
type Service struct {
	st        *store.Store
	box       *secret.Box
	log       *slog.Logger
	now       func() time.Time
	setupBase func() (string, string)
	idle, max time.Duration
	limiter   *auth.IPLimiter

	// mu serialises user mutations so the last-admin checks are race free.
	mu sync.Mutex

	cbMu        sync.RWMutex
	onRevokeUsr []func(int64)
	onRevokeTok []func(int64)
}

// New returns a Service.
func New(o Options) *Service {
	s := &Service{st: o.Store, box: o.Secret, log: o.Logger, now: o.Now, setupBase: o.SetupBase,
		idle: o.SessionIdle, max: o.SessionMax, limiter: auth.NewDefaultIPLimiter()}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.setupBase == nil {
		s.setupBase = func() (string, string) { return "", "" }
	}
	if s.idle <= 0 {
		s.idle = DefaultSessionIdle
	}
	if s.max <= 0 {
		s.max = DefaultSessionMax
	}
	return s
}

// OnRevokeUser registers f, called with the user id whenever the user is
// disabled, deleted, has its role changed or is reset. Its sessions are already
// gone when f runs; f should close the user's live connections.
func (s *Service) OnRevokeUser(f func(userID int64)) {
	s.cbMu.Lock()
	defer s.cbMu.Unlock()
	s.onRevokeUsr = append(s.onRevokeUsr, f)
}

// OnRevokeToken registers f, called with the token id when a token is revoked.
func (s *Service) OnRevokeToken(f func(tokenID int64)) {
	s.cbMu.Lock()
	defer s.cbMu.Unlock()
	s.onRevokeTok = append(s.onRevokeTok, f)
}

func (s *Service) fireUser(id int64) {
	s.cbMu.RLock()
	fs := append([]func(int64){}, s.onRevokeUsr...)
	s.cbMu.RUnlock()
	for _, f := range fs {
		f(id)
	}
}

func (s *Service) fireToken(id int64) {
	s.cbMu.RLock()
	fs := append([]func(int64){}, s.onRevokeTok...)
	s.cbMu.RUnlock()
	for _, f := range fs {
		f(id)
	}
}

// Start runs the periodic maintenance (expired sessions, audit pruning) until
// ctx ends.
func (s *Service) Start(ctx context.Context) {
	s.cleanup(ctx)
	t := time.NewTicker(cleanupEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.cleanup(ctx)
		}
	}
}

func (s *Service) cleanup(ctx context.Context) {
	now := s.now()
	if n, err := s.st.DeleteExpiredSessions(ctx, now.Add(-s.idle), now.Add(-s.max)); err != nil {
		s.log.Warn("delete expired sessions", "err", err)
	} else if n > 0 {
		s.log.Debug("expired sessions deleted", "count", n)
	}
	if _, err := s.st.PruneAudit(ctx, auditKeep); err != nil {
		s.log.Warn("prune audit log", "err", err)
	}
}

func totpAAD(id int64) []byte        { return []byte(fmt.Sprintf("user/%d/totp", id)) }
func totpPendingAAD(id int64) []byte { return []byte(fmt.Sprintf("user/%d/totp-pending", id)) }
