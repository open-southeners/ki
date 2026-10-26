package accounts

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/open-southeners/ki-agent/internal/auth"
	"github.com/open-southeners/ki-agent/internal/store"
)

// Password policy bounds (characters).
const (
	minPasswordLen = 12
	maxPasswordLen = 1024
)

// SetupInfo is what the setup page shows to enrol TOTP.
type SetupInfo struct {
	Username   string `json:"username"`
	Role       string `json:"role"`
	TOTPSecret string `json:"totp_secret"`
	OTPAuthURI string `json:"otpauth_uri"`
}

// SetupFinishRequest completes an account setup.
type SetupFinishRequest struct {
	Token, Password, Code string
	ClientIP, UserAgent   string
}

// setupUser resolves a setup token to its pending user without consuming it.
func (s *Service) setupUser(ctx context.Context, token string) (*store.User, error) {
	if token == "" {
		return nil, ErrInvalidSetup
	}
	uid, err := s.st.LookupSetupToken(ctx, auth.HashSessionID(token), s.now())
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("lookup setup token", "err", err)
		}
		return nil, ErrInvalidSetup
	}
	u, err := s.st.GetUserByID(ctx, uid)
	if err != nil || u.Status != store.UserPending {
		return nil, ErrInvalidSetup
	}
	return u, nil
}

// SetupBegin returns the TOTP enrolment data for a setup token. A fresh pending
// secret is generated (and stored sealed) on every call.
func (s *Service) SetupBegin(ctx context.Context, token string) (*SetupInfo, error) {
	u, err := s.setupUser(ctx, token)
	if err != nil {
		return nil, err
	}
	sec, err := auth.NewSecret()
	if err != nil {
		return nil, err
	}
	u.TOTPPendingEnc = s.box.Seal([]byte(sec), totpPendingAAD(u.ID))
	if err := s.st.UpdateUser(ctx, u); err != nil {
		return nil, ErrInvalidSetup
	}
	return &SetupInfo{Username: u.Username, Role: u.Role, TOTPSecret: sec,
		OTPAuthURI: auth.URI(issuer, u.Username, sec)}, nil
}

type weakError struct{ msg string }

func (e *weakError) Error() string { return e.msg }
func (e *weakError) Unwrap() error { return ErrWeakPassword }

func checkPassword(pw, username string) error {
	n := utf8.RuneCountInString(pw)
	switch {
	case n < minPasswordLen:
		return &weakError{"password must be at least 12 characters"}
	case n > maxPasswordLen:
		return &weakError{"password must be at most 1024 characters"}
	case pw == username:
		return &weakError{"password must not equal the username"}
	}
	return nil
}

// SetupFinish sets the password, confirms TOTP with a code from the pending
// secret, activates the account and logs it in. All token and code problems
// return ErrInvalidSetup; a rejected password returns an error matching
// ErrWeakPassword whose message is safe to show.
func (s *Service) SetupFinish(ctx context.Context, req SetupFinishRequest) (string, auth.Principal, error) {
	now := s.now()
	limKey := "setup:" + req.ClientIP
	if ok, retry := s.limiter.Allow(limKey, now); !ok {
		return "", auth.Principal{}, &RateLimitedError{RetryAfter: retry}
	}
	bad := func(u *store.User) (string, auth.Principal, error) {
		s.limiter.Fail(limKey, now)
		name := ""
		if u != nil {
			name = u.Username
		}
		s.audit(ctx, auth.KindUser, name, req.ClientIP, "auth.setup", "", "fail")
		return "", auth.Principal{}, ErrInvalidSetup
	}
	u, err := s.setupUser(ctx, req.Token)
	if err != nil {
		return bad(nil)
	}
	if err := checkPassword(req.Password, u.Username); err != nil {
		return "", auth.Principal{}, err
	}
	if len(u.TOTPPendingEnc) == 0 {
		return bad(u)
	}
	sec, err := s.box.Open(u.TOTPPendingEnc, totpPendingAAD(u.ID))
	if err != nil {
		s.log.Error("open pending totp secret", "user", u.Username, "err", err)
		return bad(u)
	}
	counter, ok := auth.Verify(string(sec), req.Code, now, 0)
	if !ok {
		return bad(u)
	}
	hash, err := auth.HashPassword(ctx, req.Password)
	if err != nil {
		return "", auth.Principal{}, err
	}
	// Consuming the token is the commit point: of several concurrent finishes
	// exactly one proceeds.
	uid, err := s.st.ConsumeSetupToken(ctx, auth.HashSessionID(req.Token), now)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return bad(u)
		}
		return "", auth.Principal{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, err = s.st.GetUserByID(ctx, uid)
	if err != nil {
		return "", auth.Principal{}, err
	}
	u.PasswordHash = hash
	u.TOTPSecretEnc = s.box.Seal(sec, totpAAD(u.ID))
	u.TOTPPendingEnc = nil
	u.TOTPLastCounter = int64(counter)
	u.Status = store.UserActive
	if err := s.st.UpdateUser(ctx, u); err != nil {
		return "", auth.Principal{}, err
	}
	if err := s.st.RecordLoginSuccess(ctx, u.ID, now); err != nil {
		return "", auth.Principal{}, err
	}
	id, err := s.openSession(ctx, u, req.ClientIP, req.UserAgent, now)
	if err != nil {
		return "", auth.Principal{}, err
	}
	s.audit(ctx, auth.KindUser, u.Username, req.ClientIP, "auth.setup", "", "ok")
	return id, userPrincipal(u), nil
}
