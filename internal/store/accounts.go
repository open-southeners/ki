package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// User statuses.
const (
	UserPending  = "pending"
	UserActive   = "active"
	UserDisabled = "disabled"
)

// User is a persisted panel account. The store never hashes or seals anything:
// PasswordHash is an argon2id PHC string and the TOTP fields are blobs sealed
// by the caller (aad "user/<id>/totp").
type User struct {
	ID       int64
	Username string
	// Role is "admin", "operator" or "viewer"; validated by the caller.
	Role string
	// Status is UserPending, UserActive or UserDisabled.
	Status       string
	PasswordHash string
	// TOTPSecretEnc is the enrolled secret; TOTPPendingEnc the secret generated
	// by setup/begin and not yet confirmed.
	TOTPSecretEnc, TOTPPendingEnc []byte
	// TOTPLastCounter is the last accepted TOTP time step (replay protection).
	TOTPLastCounter int64
	FailedLogins    int
	LockedUntil     *time.Time
	// CreatedAt and UpdatedAt are UTC and set by the store.
	CreatedAt, UpdatedAt time.Time
	LastLoginAt          *time.Time
}

// SetupToken is a single-use link token bound to a user. Only the SHA-256 of
// the token is stored.
type SetupToken struct {
	Hash      []byte
	UserID    int64
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
}

// Session is a login session keyed by the SHA-256 of its cookie value.
type Session struct {
	Hash      []byte
	UserID    int64
	CreatedAt time.Time
	LastSeen  time.Time
	IP        string
	UserAgent string
}

// APIToken is a bearer token keyed by the SHA-256 of its value. CreatedBy is a
// plain name and survives the deletion of that user.
type APIToken struct {
	ID        int64
	Name      string
	Hash      []byte
	Role      string
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt *time.Time
	LastUsed  *time.Time
	Revoked   bool
}

// AuditEntry is one audit log row.
type AuditEntry struct {
	ID            int64
	Time          time.Time
	PrincipalKind string
	PrincipalName string
	IP            string
	Event         string
	Target        string
	Result        string
}

func parseStamp(v string, what string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: parse %s: %w", what, err)
	}
	return t.UTC(), nil
}

const userColumns = `id, username, role, status, password_hash, totp_secret_enc, totp_pending_enc,
	totp_last_counter, failed_logins, locked_until, created_at, updated_at, last_login_at`

func scanUser(sc scanner) (*User, error) {
	var u User
	var locked, last sql.NullString
	var created, updated string
	err := sc.Scan(&u.ID, &u.Username, &u.Role, &u.Status, &u.PasswordHash, &u.TOTPSecretEnc,
		&u.TOTPPendingEnc, &u.TOTPLastCounter, &u.FailedLogins, &locked, &created, &updated, &last)
	if err != nil {
		return nil, err
	}
	if u.CreatedAt, err = parseStamp(created, "created_at"); err != nil {
		return nil, err
	}
	if u.UpdatedAt, err = parseStamp(updated, "updated_at"); err != nil {
		return nil, err
	}
	if u.LockedUntil, err = parseNullStamp(locked, "locked_until"); err != nil {
		return nil, err
	}
	if u.LastLoginAt, err = parseNullStamp(last, "last_login_at"); err != nil {
		return nil, err
	}
	return &u, nil
}

// CreateUser inserts u, setting its ID and timestamps. It returns ErrExists when
// the username is taken.
func (s *Store) CreateUser(ctx context.Context, u *User) error {
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO users
		(username, role, status, password_hash, totp_secret_enc, totp_pending_enc, totp_last_counter,
		 failed_logins, locked_until, created_at, updated_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.Username, u.Role, u.Status, u.PasswordHash, blob(u.TOTPSecretEnc), blob(u.TOTPPendingEnc),
		u.TOTPLastCounter, u.FailedLogins, nullStamp(u.LockedUntil), stamp(t), stamp(t), nullStamp(u.LastLoginAt))
	if err != nil {
		if err = mapErr(err); err == ErrExists {
			return ErrExists
		}
		return fmt.Errorf("store: create user: %w", err)
	}
	if u.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("store: create user: %w", err)
	}
	u.CreatedAt, u.UpdatedAt = t, t
	return nil
}

func (s *Store) getUserBy(ctx context.Context, column string, value any) (*User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE `+column+` = ?`, value))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get user: %w", err)
	}
	return u, nil
}

// GetUser returns the user with the given username, or ErrNotFound.
func (s *Store) GetUser(ctx context.Context, username string) (*User, error) {
	return s.getUserBy(ctx, "username", username)
}

// GetUserByID returns the user with the given id, or ErrNotFound.
func (s *Store) GetUserByID(ctx context.Context, id int64) (*User, error) {
	return s.getUserBy(ctx, "id", id)
}

// ListUsers returns all users ordered by username.
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list users: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	return out, nil
}

// UpdateUser overwrites the role, status, credentials (password hash, sealed
// TOTP secrets) and TOTP counter of the user with u.ID and bumps UpdatedAt (also
// on u). The login-failure counters and last_login_at are left alone (see
// RecordLoginFailure, RecordLoginSuccess, ResetLoginState). It returns
// ErrNotFound if there is no such user.
func (s *Store) UpdateUser(ctx context.Context, u *User) error {
	t := now()
	res, err := s.db.ExecContext(ctx, `UPDATE users SET role = ?, status = ?, password_hash = ?,
		totp_secret_enc = ?, totp_pending_enc = ?, totp_last_counter = ?, updated_at = ? WHERE id = ?`,
		u.Role, u.Status, u.PasswordHash, blob(u.TOTPSecretEnc), blob(u.TOTPPendingEnc),
		u.TOTPLastCounter, stamp(t), u.ID)
	if err != nil {
		return fmt.Errorf("store: update user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	u.UpdatedAt = t
	return nil
}

// DeleteUser removes the user and, by cascade, its setup tokens and sessions.
// It returns ErrNotFound if there is no such user.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountActiveAdmins returns the number of users with role "admin" and status
// "active".
func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin' AND status = 'active'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count admins: %w", err)
	}
	return n, nil
}

// RecordLoginFailure increments the user's consecutive-failure counter and, if
// lockUntil is non-zero, locks the account until then. It returns the new
// counter, or ErrNotFound for an unknown user.
func (s *Store) RecordLoginFailure(ctx context.Context, userID int64, lockUntil time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: login failure: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE users SET failed_logins = failed_logins + 1 WHERE id = ?`, userID)
	if err != nil {
		return 0, fmt.Errorf("store: login failure: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	if !lockUntil.IsZero() {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET locked_until = ? WHERE id = ?`, stamp(lockUntil), userID); err != nil {
			return 0, fmt.Errorf("store: login failure: %w", err)
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT failed_logins FROM users WHERE id = ?`, userID).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: login failure: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: login failure: %w", err)
	}
	return count, nil
}

// RecordLoginSuccess sets last_login_at to at and clears the failure counter and
// lock. It returns ErrNotFound for an unknown user.
func (s *Store) RecordLoginSuccess(ctx context.Context, userID int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET last_login_at = ?, failed_logins = 0, locked_until = NULL WHERE id = ?`,
		stamp(at), userID)
	if err != nil {
		return fmt.Errorf("store: login success: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ResetLoginState clears the failure counter and lock without touching
// last_login_at (used when an admin resets an account). It returns ErrNotFound
// for an unknown user.
func (s *Store) ResetLoginState(ctx context.Context, userID int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET failed_logins = 0, locked_until = NULL WHERE id = ?`, userID)
	if err != nil {
		return fmt.Errorf("store: reset login state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AdvanceTOTPCounter records counter as the user's last accepted TOTP step, only
// if it is greater than the stored one (compare-and-set, safe against concurrent
// replays). It reports whether the counter was accepted and returns ErrNotFound
// for an unknown user.
func (s *Store) AdvanceTOTPCounter(ctx context.Context, userID, counter int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET totp_last_counter = ? WHERE id = ? AND totp_last_counter < ?`,
		counter, userID, counter)
	if err != nil {
		return false, fmt.Errorf("store: totp counter: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	var one int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: totp counter: %w", err)
	}
	return false, nil
}

// CreateSetupToken stores a setup token for the user, deleting the user's older
// ones first. It returns ErrNotFound for an unknown user and ErrExists on a hash
// collision.
func (s *Store) CreateSetupToken(ctx context.Context, userID int64, hash []byte, expiresAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: create setup token: %w", err)
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, userID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("store: create setup token: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM setup_tokens WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("store: create setup token: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO setup_tokens (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hash, userID, stamp(now()), stamp(expiresAt)); err != nil {
		if err = mapErr(err); err == ErrExists {
			return ErrExists
		}
		return fmt.Errorf("store: create setup token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: create setup token: %w", err)
	}
	return nil
}

// validSetupToken returns the owner of an unused, unexpired token inside tx.
func validSetupToken(ctx context.Context, tx *sql.Tx, hash []byte, at time.Time) (int64, error) {
	var uid int64
	var expires string
	var used sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT user_id, expires_at, used_at FROM setup_tokens WHERE token_hash = ?`, hash).
		Scan(&uid, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: setup token: %w", err)
	}
	if used.Valid {
		return 0, ErrNotFound
	}
	exp, err := parseStamp(expires, "expires_at")
	if err != nil {
		return 0, err
	}
	if !at.Before(exp) {
		return 0, ErrNotFound
	}
	return uid, nil
}

// LookupSetupToken returns the user id a valid (unused, unexpired at at) setup
// token belongs to, without consuming it. Unknown, used or expired tokens give
// ErrNotFound.
func (s *Store) LookupSetupToken(ctx context.Context, hash []byte, at time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: setup token: %w", err)
	}
	defer tx.Rollback()
	return validSetupToken(ctx, tx, hash, at)
}

// ConsumeSetupToken atomically marks a valid setup token used and returns its
// user id. Unknown, already used or expired tokens give ErrNotFound; of several
// concurrent callers exactly one succeeds.
func (s *Store) ConsumeSetupToken(ctx context.Context, hash []byte, at time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: consume setup token: %w", err)
	}
	defer tx.Rollback()
	uid, err := validSetupToken(ctx, tx, hash, at)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE setup_tokens SET used_at = ? WHERE token_hash = ?`, stamp(at), hash); err != nil {
		return 0, fmt.Errorf("store: consume setup token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: consume setup token: %w", err)
	}
	return uid, nil
}

// CreateSession inserts se, setting CreatedAt and LastSeen when zero. It
// returns ErrNotFound for an unknown user and ErrExists on a hash collision.
func (s *Store) CreateSession(ctx context.Context, se *Session) error {
	if se.CreatedAt.IsZero() {
		se.CreatedAt = now()
	}
	if se.LastSeen.IsZero() {
		se.LastSeen = se.CreatedAt
	}
	se.CreatedAt, se.LastSeen = se.CreatedAt.UTC(), se.LastSeen.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, se.UserID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, last_seen, ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?)`,
		se.Hash, se.UserID, stamp(se.CreatedAt), stamp(se.LastSeen), se.IP, se.UserAgent); err != nil {
		if err = mapErr(err); err == ErrExists {
			return ErrExists
		}
		return fmt.Errorf("store: create session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

// GetSession returns the session with the given token hash, or ErrNotFound.
func (s *Store) GetSession(ctx context.Context, hash []byte) (*Session, error) {
	var se Session
	var created, seen string
	err := s.db.QueryRowContext(ctx, `SELECT token_hash, user_id, created_at, last_seen, ip, user_agent
		FROM sessions WHERE token_hash = ?`, hash).Scan(&se.Hash, &se.UserID, &created, &seen, &se.IP, &se.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get session: %w", err)
	}
	if se.CreatedAt, err = parseStamp(created, "created_at"); err != nil {
		return nil, err
	}
	if se.LastSeen, err = parseStamp(seen, "last_seen"); err != nil {
		return nil, err
	}
	return &se, nil
}

// TouchSession sets the session's last_seen to at. It returns ErrNotFound if
// the session is gone. Rate limiting is the caller's job.
func (s *Store) TouchSession(ctx context.Context, hash []byte, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen = ? WHERE token_hash = ?`, stamp(at), hash)
	if err != nil {
		return fmt.Errorf("store: touch session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSession removes the session (a no-op when it does not exist).
func (s *Store) DeleteSession(ctx context.Context, hash []byte) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hash); err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

// DeleteSessionsForUser removes all of the user's sessions and returns how many
// there were.
func (s *Store) DeleteSessionsForUser(ctx context.Context, userID int64) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, fmt.Errorf("store: delete user sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteExpiredSessions removes sessions idle since before idleBefore or created
// before createdBefore, and returns how many it removed.
func (s *Store) DeleteExpiredSessions(ctx context.Context, idleBefore, createdBefore time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE julianday(last_seen) < julianday(?)
		OR julianday(created_at) < julianday(?)`, stamp(idleBefore), stamp(createdBefore))
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

const apiTokenColumns = `id, name, token_hash, role, created_by, created_at, expires_at, last_used, revoked`

func scanAPIToken(sc scanner) (*APIToken, error) {
	var t APIToken
	var created string
	var expires, used sql.NullString
	err := sc.Scan(&t.ID, &t.Name, &t.Hash, &t.Role, &t.CreatedBy, &created, &expires, &used, &t.Revoked)
	if err != nil {
		return nil, err
	}
	if t.CreatedAt, err = parseStamp(created, "created_at"); err != nil {
		return nil, err
	}
	if t.ExpiresAt, err = parseNullStamp(expires, "expires_at"); err != nil {
		return nil, err
	}
	if t.LastUsed, err = parseNullStamp(used, "last_used"); err != nil {
		return nil, err
	}
	return &t, nil
}

// CreateAPIToken inserts t, setting its ID and CreatedAt. It returns ErrExists
// when the name or hash is taken (revoked tokens keep their name).
func (s *Store) CreateAPIToken(ctx context.Context, t *APIToken) error {
	at := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens (name, token_hash, role, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, t.Name, t.Hash, t.Role, t.CreatedBy, stamp(at), nullStamp(t.ExpiresAt))
	if err != nil {
		if err = mapErr(err); err == ErrExists {
			return ErrExists
		}
		return fmt.Errorf("store: create api token: %w", err)
	}
	if t.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("store: create api token: %w", err)
	}
	t.CreatedAt, t.Revoked = at, false
	return nil
}

// GetAPITokenByHash returns the token with the given hash (revoked or expired
// ones included; the caller checks), or ErrNotFound.
func (s *Store) GetAPITokenByHash(ctx context.Context, hash []byte) (*APIToken, error) {
	t, err := scanAPIToken(s.db.QueryRowContext(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens WHERE token_hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get api token: %w", err)
	}
	return t, nil
}

// ListAPITokens returns all tokens (revoked included) ordered by name.
func (s *Store) ListAPITokens(ctx context.Context) ([]*APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list api tokens: %w", err)
	}
	defer rows.Close()
	var out []*APIToken
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list api tokens: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list api tokens: %w", err)
	}
	return out, nil
}

// RevokeAPIToken marks the named token revoked (idempotent) and returns its id.
// It returns ErrNotFound for an unknown name.
func (s *Store) RevokeAPIToken(ctx context.Context, name string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: revoke api token: %w", err)
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM api_tokens WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: revoke api token: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked = 1 WHERE id = ?`, id); err != nil {
		return 0, fmt.Errorf("store: revoke api token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: revoke api token: %w", err)
	}
	return id, nil
}

// TouchAPIToken sets last_used to at. Rate limiting is the caller's job. It
// returns ErrNotFound for an unknown id.
func (s *Store) TouchAPIToken(ctx context.Context, id int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used = ? WHERE id = ?`, stamp(at), id)
	if err != nil {
		return fmt.Errorf("store: touch api token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AppendAudit inserts e, setting its ID, and its Time to now when zero.
func (s *Store) AppendAudit(ctx context.Context, e *AuditEntry) error {
	if e.Time.IsZero() {
		e.Time = now()
	}
	e.Time = e.Time.UTC()
	res, err := s.db.ExecContext(ctx, `INSERT INTO audit_log (at, principal_kind, principal_name, ip, event, target, result)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, stamp(e.Time), e.PrincipalKind, e.PrincipalName, e.IP, e.Event, e.Target, e.Result)
	if err != nil {
		return fmt.Errorf("store: append audit: %w", err)
	}
	if e.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("store: append audit: %w", err)
	}
	return nil
}

// ListAudit returns up to limit entries, newest first. A beforeID above zero
// restricts the result to entries with a smaller id (pagination).
func (s *Store) ListAudit(ctx context.Context, limit int, beforeID int64) ([]*AuditEntry, error) {
	q := `SELECT id, at, principal_kind, principal_name, ip, event, target, result FROM audit_log`
	var args []any
	if beforeID > 0 {
		q += ` WHERE id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at string
		if err := rows.Scan(&e.ID, &at, &e.PrincipalKind, &e.PrincipalName, &e.IP, &e.Event, &e.Target, &e.Result); err != nil {
			return nil, fmt.Errorf("store: list audit: %w", err)
		}
		if e.Time, err = parseStamp(at, "at"); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	return out, nil
}

// PruneAudit deletes all but the newest keep entries and returns how many it
// removed.
func (s *Store) PruneAudit(ctx context.Context, keep int) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM audit_log WHERE id NOT IN
		(SELECT id FROM audit_log ORDER BY id DESC LIMIT ?)`, keep)
	if err != nil {
		return 0, fmt.Errorf("store: prune audit: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
