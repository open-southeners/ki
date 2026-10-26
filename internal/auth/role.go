// Package auth holds the authentication and authorisation primitives shared by
// the agent: roles and principals, the method-to-role table, password hashing,
// TOTP, opaque tokens and brute-force limiters.
package auth

import (
	"encoding/json"
	"fmt"
)

// Role is an authorisation level. Roles are ordered: Viewer < Operator < Admin.
type Role int

// The defined roles, in increasing privilege. The zero value is invalid.
const (
	RoleViewer Role = iota + 1
	RoleOperator
	RoleAdmin
)

// String returns the wire name of r, or "invalid" for an unknown role.
func (r Role) String() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RoleOperator:
		return "operator"
	case RoleAdmin:
		return "admin"
	}
	return "invalid"
}

// Valid reports whether r is one of the defined roles.
func (r Role) Valid() bool { return r >= RoleViewer && r <= RoleAdmin }

// AtLeast reports whether r grants at least the privileges of min. An invalid
// role never satisfies anything.
func (r Role) AtLeast(min Role) bool { return r.Valid() && r >= min }

// ParseRole parses a role name ("viewer", "operator" or "admin").
func ParseRole(s string) (Role, error) {
	switch s {
	case "viewer":
		return RoleViewer, nil
	case "operator":
		return RoleOperator, nil
	case "admin":
		return RoleAdmin, nil
	}
	return 0, fmt.Errorf("unknown role %q", s)
}

// MarshalJSON encodes r as its string name.
func (r Role) MarshalJSON() ([]byte, error) {
	if !r.Valid() {
		return nil, fmt.Errorf("invalid role %d", int(r))
	}
	return json.Marshal(r.String())
}

// UnmarshalJSON decodes a role from its string name.
func (r *Role) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("role must be a string: %w", err)
	}
	v, err := ParseRole(s)
	if err != nil {
		return err
	}
	*r = v
	return nil
}
