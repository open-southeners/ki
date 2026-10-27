package accounts

import (
	"context"
	"errors"
	"time"

	"github.com/open-southeners/ki/internal/auth"
	"github.com/open-southeners/ki/internal/store"
)

// LoginRequest carries the credentials of a panel login.
type LoginRequest struct {
	Username, Password, Code string
	ClientIP, UserAgent      string
}

func userPrincipal(u *store.User) auth.Principal {
	r, _ := auth.ParseRole(u.Role)
	return auth.Principal{Kind: auth.KindUser, ID: u.ID, Name: u.Username, Role: r}
}

func clip(v string) string {
	if len(v) > 64 {
		return v[:64]
	}
	return v
}

func clipUA(v string) string {
	if len(v) > 256 {
		return v[:256]
	}
	return v
}

// Login authenticates a user and opens a session. Every credential failure
// returns ErrInvalidCredentials; a blocked client gets a *RateLimitedError;
// auth.ErrBusy is passed through (hash slots exhausted).
func (s *Service) Login(ctx context.Context, req LoginRequest) (string, auth.Principal, error) {
	now := s.now()
	if ok, retry := s.limiter.Allow(req.ClientIP, now); !ok {
		s.audit(ctx, auth.KindUser, clip(req.Username), req.ClientIP, "auth.login", "", "rate_limited")
		return "", auth.Principal{}, &RateLimitedError{RetryAfter: retry}
	}
	// fail records a failed attempt; u, when non-nil, also counts towards the
	// account lockout.
	fail := func(u *store.User, result string) (string, auth.Principal, error) {
		s.limiter.Fail(req.ClientIP, now)
		s.audit(ctx, auth.KindUser, clip(req.Username), req.ClientIP, "auth.login", "", result)
		if u != nil {
			lock := auth.NextLock(u.FailedLogins+1, now)
			if _, err := s.st.RecordLoginFailure(ctx, u.ID, lock); err != nil {
				s.log.Warn("record login failure", "err", err)
			} else if !lock.IsZero() {
				s.audit(ctx, auth.KindUser, u.Username, req.ClientIP, "auth.lockout", "", "locked")
				s.log.Warn("account locked", "user", u.Username)
			}
		}
		return "", auth.Principal{}, ErrInvalidCredentials
	}

	u, err := s.st.GetUser(ctx, req.Username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", auth.Principal{}, err
	}
	// Unknown, inactive, not enrolled or locked accounts burn the same hashing
	// time and do not accumulate further lockout.
	if err != nil || u.Status != store.UserActive || u.PasswordHash == "" || len(u.TOTPSecretEnc) == 0 ||
		(u.LockedUntil != nil && u.LockedUntil.After(now)) {
		if _, err := auth.DummyVerify(ctx, req.Password); err != nil {
			return "", auth.Principal{}, err
		}
		return fail(nil, "fail")
	}
	ok, err := auth.VerifyPassword(ctx, req.Password, u.PasswordHash)
	if err != nil {
		if errors.Is(err, auth.ErrBusy) {
			return "", auth.Principal{}, err
		}
		s.log.Error("verify password", "user", u.Username, "err", err)
		return fail(nil, "fail")
	}
	if !ok {
		return fail(u, "fail")
	}
	sec, err := s.box.Open(u.TOTPSecretEnc, totpAAD(u.ID))
	if err != nil {
		s.log.Error("open totp secret", "user", u.Username, "err", err)
		return fail(nil, "fail")
	}
	counter, ok := auth.Verify(string(sec), req.Code, now, uint64(u.TOTPLastCounter))
	if !ok {
		return fail(u, "fail")
	}
	if adv, err := s.st.AdvanceTOTPCounter(ctx, u.ID, int64(counter)); err != nil {
		return "", auth.Principal{}, err
	} else if !adv {
		return fail(u, "replay")
	}
	if err := s.st.RecordLoginSuccess(ctx, u.ID, now); err != nil {
		return "", auth.Principal{}, err
	}
	id, err := s.openSession(ctx, u, req.ClientIP, req.UserAgent, now)
	if err != nil {
		return "", auth.Principal{}, err
	}
	s.audit(ctx, auth.KindUser, u.Username, req.ClientIP, "auth.login", "", "ok")
	return id, userPrincipal(u), nil
}

func (s *Service) openSession(ctx context.Context, u *store.User, ip, ua string, now time.Time) (string, error) {
	id, hash, err := auth.NewSessionID()
	if err != nil {
		return "", err
	}
	se := &store.Session{Hash: hash, UserID: u.ID, CreatedAt: now.UTC(), LastSeen: now.UTC(), IP: ip, UserAgent: clipUA(ua)}
	if err := s.st.CreateSession(ctx, se); err != nil {
		return "", err
	}
	return id, nil
}

// Logout ends the session. An unknown session is not an error.
func (s *Service) Logout(ctx context.Context, sessionID string) error {
	hash := auth.HashSessionID(sessionID)
	se, err := s.st.GetSession(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.st.DeleteSession(ctx, hash); err != nil {
		return err
	}
	name := ""
	if u, err := s.st.GetUserByID(ctx, se.UserID); err == nil {
		name = u.Username
	}
	s.audit(ctx, auth.KindUser, name, se.IP, "auth.logout", "", "ok")
	return nil
}

// Session validates a session cookie value: the session must be within its
// idle and absolute lifetimes and its user active. last_seen is refreshed at
// most once a minute. Any failure returns ErrUnauthenticated.
func (s *Service) Session(ctx context.Context, sessionID string) (auth.Principal, error) {
	if sessionID == "" {
		return auth.Principal{}, ErrUnauthenticated
	}
	hash := auth.HashSessionID(sessionID)
	se, err := s.st.GetSession(ctx, hash)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("get session", "err", err)
		}
		return auth.Principal{}, ErrUnauthenticated
	}
	now := s.now()
	if now.Sub(se.LastSeen) > s.idle || now.Sub(se.CreatedAt) > s.max {
		_ = s.st.DeleteSession(ctx, hash)
		return auth.Principal{}, ErrUnauthenticated
	}
	u, err := s.st.GetUserByID(ctx, se.UserID)
	if err != nil || u.Status != store.UserActive {
		return auth.Principal{}, ErrUnauthenticated
	}
	if now.Sub(se.LastSeen) >= touchInterval {
		if err := s.st.TouchSession(ctx, hash, now); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("touch session", "err", err)
		}
	}
	return userPrincipal(u), nil
}

// Token validates a bearer API token (not revoked, not expired). last_used is
// refreshed at most once a minute. Any failure returns ErrUnauthenticated.
func (s *Service) Token(ctx context.Context, bearer string) (auth.Principal, error) {
	if len(bearer) <= len(auth.TokenPrefix) || bearer[:len(auth.TokenPrefix)] != auth.TokenPrefix {
		return auth.Principal{}, ErrUnauthenticated
	}
	t, err := s.st.GetAPITokenByHash(ctx, auth.HashToken(bearer))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("get api token", "err", err)
		}
		return auth.Principal{}, ErrUnauthenticated
	}
	now := s.now()
	if t.Revoked || (t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)) {
		return auth.Principal{}, ErrUnauthenticated
	}
	r, err := auth.ParseRole(t.Role)
	if err != nil {
		return auth.Principal{}, ErrUnauthenticated
	}
	if t.LastUsed == nil || now.Sub(*t.LastUsed) >= touchInterval {
		if err := s.st.TouchAPIToken(ctx, t.ID, now); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("touch api token", "err", err)
		}
	}
	return auth.Principal{Kind: auth.KindToken, ID: t.ID, Name: t.Name, Role: r}, nil
}
