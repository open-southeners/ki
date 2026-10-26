package accounts

import (
	"context"
	"time"

	"github.com/open-southeners/ki-agent/internal/auth"
	"github.com/open-southeners/ki-agent/internal/store"
)

// CreatedToken is returned once by CreateToken.
type CreatedToken struct {
	Token     string     `json:"token"`
	Name      string     `json:"name"`
	Role      string     `json:"role"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// TokenInfo is the public view of a stored API token.
type TokenInfo struct {
	Name       string     `json:"name"`
	Role       string     `json:"role"`
	CreatedAt  time.Time  `json:"created_at"`
	CreatedBy  string     `json:"created_by"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Revoked    bool       `json:"revoked,omitempty"`
}

// CreateToken mints an API token. expiresIn of zero means it never expires; it
// may not exceed one year.
func (s *Service) CreateToken(ctx context.Context, createdBy, name, role string, expiresIn time.Duration) (*CreatedToken, error) {
	if !tokenNameRE.MatchString(name) {
		return nil, invalidf("name must match %s", tokenNameRE)
	}
	r, err := parseUserRole(role)
	if err != nil {
		return nil, err
	}
	if expiresIn < 0 || expiresIn > MaxTokenLifetime {
		return nil, invalidf("expires_in must be between 0 and %s", MaxTokenLifetime)
	}
	plain, hash, err := auth.NewToken()
	if err != nil {
		return nil, err
	}
	t := &store.APIToken{Name: name, Hash: hash, Role: r.String(), CreatedBy: createdBy}
	if expiresIn > 0 {
		e := s.now().Add(expiresIn).UTC()
		t.ExpiresAt = &e
	}
	if err := s.st.CreateAPIToken(ctx, t); err != nil {
		return nil, err
	}
	return &CreatedToken{Token: plain, Name: name, Role: t.Role, ExpiresAt: t.ExpiresAt}, nil
}

// ListTokens returns all tokens, never their secrets.
func (s *Service) ListTokens(ctx context.Context) ([]TokenInfo, error) {
	ts, err := s.st.ListAPITokens(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]TokenInfo, 0, len(ts))
	for _, t := range ts {
		out = append(out, TokenInfo{Name: t.Name, Role: t.Role, CreatedAt: t.CreatedAt, CreatedBy: t.CreatedBy,
			ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsed, Revoked: t.Revoked})
	}
	return out, nil
}

// RevokeToken revokes the named token and fires the revocation callbacks.
func (s *Service) RevokeToken(ctx context.Context, name string) error {
	id, err := s.st.RevokeAPIToken(ctx, name)
	if err != nil {
		return err
	}
	s.fireToken(id)
	return nil
}
