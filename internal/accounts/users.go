package accounts

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/open-southeners/ki/internal/auth"
	"github.com/open-southeners/ki/internal/store"
)

var (
	usernameRE  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{1,31}$`)
	tokenNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
)

// User is the public view of an account.
type User struct {
	Username     string     `json:"username"`
	Role         string     `json:"role"`
	Status       string     `json:"status"`
	TOTPEnrolled bool       `json:"totp_enrolled"`
	CreatedAt    time.Time  `json:"created_at"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
}

func toUser(u *store.User) User {
	return User{Username: u.Username, Role: u.Role, Status: u.Status, TOTPEnrolled: len(u.TOTPSecretEnc) > 0,
		CreatedAt: u.CreatedAt, LastLoginAt: u.LastLoginAt}
}

// SetupLink is the result of issuing a setup token for a user.
type SetupLink struct {
	User              User      `json:"user"`
	SetupURL          string    `json:"setup_url"`
	FingerprintSHA256 string    `json:"fingerprint_sha256,omitempty"`
	ExpiresAt         time.Time `json:"expires_at"`
}

func (s *Service) issueSetup(ctx context.Context, u *store.User) (*SetupLink, error) {
	tok, hash, err := auth.NewSessionID()
	if err != nil {
		return nil, err
	}
	exp := s.now().Add(SetupTTL).UTC()
	if err := s.st.CreateSetupToken(ctx, u.ID, hash, exp); err != nil {
		return nil, err
	}
	base, fp := s.setupBase()
	return &SetupLink{User: toUser(u), SetupURL: strings.TrimRight(base, "/") + "/setup#" + tok,
		FingerprintSHA256: fp, ExpiresAt: exp}, nil
}

func parseUserRole(v string) (auth.Role, error) {
	r, err := auth.ParseRole(v)
	if err != nil {
		return 0, invalidf("role must be viewer, operator or admin")
	}
	return r, nil
}

func checkUsername(name string) error {
	if !usernameRE.MatchString(name) {
		return invalidf("username must match %s", usernameRE)
	}
	return nil
}

// ListUsers returns all accounts.
func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	us, err := s.st.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]User, 0, len(us))
	for _, u := range us {
		out = append(out, toUser(u))
	}
	return out, nil
}

// CreateUser creates a pending account and returns its setup link.
func (s *Service) CreateUser(ctx context.Context, username, role string) (*SetupLink, error) {
	if err := checkUsername(username); err != nil {
		return nil, err
	}
	r, err := parseUserRole(role)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u := &store.User{Username: username, Role: r.String(), Status: store.UserPending}
	if err := s.st.CreateUser(ctx, u); err != nil {
		return nil, err
	}
	return s.issueSetup(ctx, u)
}

// lastAdmin reports whether u is the only active admin.
func (s *Service) lastAdmin(ctx context.Context, u *store.User) (bool, error) {
	if u.Role != auth.RoleAdmin.String() || u.Status != store.UserActive {
		return false, nil
	}
	n, err := s.st.CountActiveAdmins(ctx)
	return n <= 1, err
}

// UpdateUser changes the role and/or the disabled flag of a user. The last
// active admin cannot be demoted or disabled.
func (s *Service) UpdateUser(ctx context.Context, username string, role *string, disabled *bool) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, err := s.st.GetUser(ctx, username)
	if err != nil {
		return nil, err
	}
	revoke, sessions := false, false
	if role != nil {
		r, err := parseUserRole(*role)
		if err != nil {
			return nil, err
		}
		if r.String() != u.Role {
			if last, err := s.lastAdmin(ctx, u); err != nil {
				return nil, err
			} else if last {
				return nil, invalidf("cannot demote the last active admin")
			}
			u.Role = r.String()
			revoke = true
		}
	}
	if disabled != nil {
		switch {
		case *disabled && u.Status != store.UserDisabled:
			if last, err := s.lastAdmin(ctx, u); err != nil {
				return nil, err
			} else if last {
				return nil, invalidf("cannot disable the last active admin")
			}
			u.Status = store.UserDisabled
			revoke, sessions = true, true
		case !*disabled && u.Status == store.UserDisabled:
			u.Status = store.UserActive
			if u.PasswordHash == "" || len(u.TOTPSecretEnc) == 0 {
				u.Status = store.UserPending
			}
		}
	}
	if err := s.st.UpdateUser(ctx, u); err != nil {
		return nil, err
	}
	if sessions {
		if _, err := s.st.DeleteSessionsForUser(ctx, u.ID); err != nil {
			return nil, err
		}
	}
	if revoke {
		s.fireUser(u.ID)
	}
	v := toUser(u)
	return &v, nil
}

// ResetUser clears the password, TOTP and login state of a user, revokes its
// sessions and returns a fresh setup link. It is also the recovery path for the
// only admin, so it is allowed on the last active admin.
func (s *Service) ResetUser(ctx context.Context, username string) (*SetupLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, err := s.st.GetUser(ctx, username)
	if err != nil {
		return nil, err
	}
	u.PasswordHash, u.TOTPSecretEnc, u.TOTPPendingEnc, u.TOTPLastCounter = "", nil, nil, 0
	u.Status = store.UserPending
	if err := s.st.UpdateUser(ctx, u); err != nil {
		return nil, err
	}
	if err := s.st.ResetLoginState(ctx, u.ID); err != nil {
		return nil, err
	}
	if _, err := s.st.DeleteSessionsForUser(ctx, u.ID); err != nil {
		return nil, err
	}
	s.fireUser(u.ID)
	return s.issueSetup(ctx, u)
}

// DeleteUser removes a user. The last active admin cannot be deleted.
func (s *Service) DeleteUser(ctx context.Context, username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, err := s.st.GetUser(ctx, username)
	if err != nil {
		return err
	}
	if last, err := s.lastAdmin(ctx, u); err != nil {
		return err
	} else if last {
		return invalidf("cannot delete the last active admin")
	}
	if err := s.st.DeleteUser(ctx, u.ID); err != nil {
		return err
	}
	s.fireUser(u.ID)
	return nil
}

// PanelSetup bootstraps the first admin: when no active admin exists it creates
// (or reuses the pending) account username (default "admin") and returns its
// setup link.
func (s *Service) PanelSetup(ctx context.Context, username string) (*SetupLink, error) {
	if username == "" {
		username = "admin"
	}
	if err := checkUsername(username); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.st.CountActiveAdmins(ctx)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, invalidf("an admin exists; use `ki agent users reset <name>`")
	}
	u, err := s.st.GetUser(ctx, username)
	switch {
	case errors.Is(err, store.ErrNotFound):
		u = &store.User{Username: username, Role: auth.RoleAdmin.String(), Status: store.UserPending}
		if err := s.st.CreateUser(ctx, u); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case u.Status != store.UserPending || u.Role != auth.RoleAdmin.String():
		return nil, fmt.Errorf("user %q is not a pending admin: %w", username, store.ErrExists)
	}
	return s.issueSetup(ctx, u)
}
