package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mkUser(t *testing.T, s *Store, name, role, status string) *User {
	t.Helper()
	u := &User{Username: name, Role: role, Status: status}
	if err := s.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUserCRUD(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	u := mkUser(t, s, "alice", "admin", UserPending)
	if u.ID == 0 || u.CreatedAt.IsZero() {
		t.Fatalf("%+v", u)
	}
	if err := s.CreateUser(ctx, &User{Username: "alice", Role: "viewer", Status: UserActive}); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	got, err := s.GetUser(ctx, "alice")
	if err != nil || got.ID != u.ID || got.Status != UserPending || got.LockedUntil != nil || got.LastLoginAt != nil ||
		got.CreatedAt.Location().String() != "UTC" {
		t.Fatalf("%+v %v", got, err)
	}
	got.Status, got.Role, got.PasswordHash = UserActive, "operator", "$argon2id$x"
	got.TOTPSecretEnc, got.TOTPPendingEnc = []byte{1, 2}, []byte{3}
	if err := s.UpdateUser(ctx, got); err != nil {
		t.Fatal(err)
	}
	byID, err := s.GetUserByID(ctx, u.ID)
	if err != nil || byID.Role != "operator" || byID.PasswordHash != "$argon2id$x" || len(byID.TOTPSecretEnc) != 2 ||
		len(byID.TOTPPendingEnc) != 1 || byID.UpdatedAt.Before(byID.CreatedAt) {
		t.Fatalf("%+v %v", byID, err)
	}
	mkUser(t, s, "bob", "viewer", UserActive)
	list, err := s.ListUsers(ctx)
	if err != nil || len(list) != 2 || list[0].Username != "alice" {
		t.Fatalf("%v %v", list, err)
	}
	if _, err := s.GetUser(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.GetUserByID(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.UpdateUser(ctx, &User{ID: 999, Status: UserActive}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestCountActiveAdmins(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	for _, c := range []struct {
		name, role, status string
		want               int
	}{
		{"a", "admin", UserPending, 0},
		{"b", "operator", UserActive, 0},
		{"c", "admin", UserDisabled, 0},
		{"d", "admin", UserActive, 1},
		{"e", "admin", UserActive, 2},
	} {
		mkUser(t, s, c.name, c.role, c.status)
		if n, err := s.CountActiveAdmins(ctx); err != nil || n != c.want {
			t.Fatalf("%s: %d %v want %d", c.name, n, err, c.want)
		}
	}
}

func TestLoginState(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	u := mkUser(t, s, "alice", "admin", UserActive)
	lock := now().Add(time.Hour)
	for i, c := range []struct {
		lock time.Time
		want int
	}{{time.Time{}, 1}, {time.Time{}, 2}, {lock, 3}} {
		n, err := s.RecordLoginFailure(ctx, u.ID, c.lock)
		if err != nil || n != c.want {
			t.Fatalf("%d: %d %v", i, n, err)
		}
	}
	got, _ := s.GetUserByID(ctx, u.ID)
	if got.FailedLogins != 3 || got.LockedUntil == nil || !got.LockedUntil.Equal(lock) {
		t.Fatalf("%+v", got)
	}
	at := now()
	if err := s.RecordLoginSuccess(ctx, u.ID, at); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetUserByID(ctx, u.ID)
	if got.FailedLogins != 0 || got.LockedUntil != nil || got.LastLoginAt == nil || !got.LastLoginAt.Equal(at) {
		t.Fatalf("%+v", got)
	}
	s.RecordLoginFailure(ctx, u.ID, lock)
	if err := s.ResetLoginState(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetUserByID(ctx, u.ID)
	if got.FailedLogins != 0 || got.LockedUntil != nil || got.LastLoginAt == nil {
		t.Fatalf("%+v", got)
	}
	if _, err := s.RecordLoginFailure(ctx, 999, time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.RecordLoginSuccess(ctx, 999, at); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.ResetLoginState(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestAdvanceTOTPCounter(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	u := mkUser(t, s, "alice", "admin", UserActive)
	for _, c := range []struct {
		counter int64
		want    bool
	}{{10, true}, {10, false}, {9, false}, {11, true}, {5, false}, {1000, true}} {
		ok, err := s.AdvanceTOTPCounter(ctx, u.ID, c.counter)
		if err != nil || ok != c.want {
			t.Fatalf("%d: %v %v", c.counter, ok, err)
		}
	}
	got, _ := s.GetUserByID(ctx, u.ID)
	if got.TOTPLastCounter != 1000 {
		t.Fatal(got.TOTPLastCounter)
	}
	if _, err := s.AdvanceTOTPCounter(ctx, 999, 1); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// Concurrent replays of the same counter: one winner.
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := s.AdvanceTOTPCounter(ctx, u.ID, 2000); err != nil {
				t.Error(err)
			} else if ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d winners", wins.Load())
	}
}

func TestSetupTokens(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	u := mkUser(t, s, "alice", "admin", UserPending)
	other := mkUser(t, s, "bob", "viewer", UserPending)
	base := now()
	h1, h2, ho := []byte("hash-1"), []byte("hash-2"), []byte("hash-o")
	if err := s.CreateSetupToken(ctx, 999, h1, base.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.CreateSetupToken(ctx, u.ID, h1, base.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSetupToken(ctx, other.ID, ho, base.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A newer token supersedes the older one of the same user only.
	if err := s.CreateSetupToken(ctx, u.ID, h2, base.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		hash []byte
		at   time.Time
		want error
	}{
		{"superseded", h1, base, ErrNotFound},
		{"unknown", []byte("zzz"), base, ErrNotFound},
		{"expired", h2, base.Add(30 * time.Minute), ErrNotFound},
		{"valid", h2, base.Add(29 * time.Minute), nil},
	} {
		uid, err := s.LookupSetupToken(ctx, c.hash, c.at)
		if !errors.Is(err, c.want) || (err == nil && uid != u.ID) {
			t.Fatalf("lookup %s: %d %v", c.name, uid, err)
		}
	}
	// Lookup does not consume.
	if uid, err := s.ConsumeSetupToken(ctx, h2, base); err != nil || uid != u.ID {
		t.Fatalf("%d %v", uid, err)
	}
	if _, err := s.ConsumeSetupToken(ctx, h2, base); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.LookupSetupToken(ctx, h2, base); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.ConsumeSetupToken(ctx, ho, base.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired consume: %v", err)
	}
	if uid, err := s.ConsumeSetupToken(ctx, ho, base); err != nil || uid != other.ID {
		t.Fatalf("expired token must stay unused: %d %v", uid, err)
	}
}

func TestConsumeSetupTokenConcurrent(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	u := mkUser(t, s, "alice", "admin", UserPending)
	h := []byte("race")
	if err := s.CreateSetupToken(ctx, u.ID, h, now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			uid, err := s.ConsumeSetupToken(ctx, h, now())
			switch {
			case err == nil && uid == u.ID:
				wins.Add(1)
			case errors.Is(err, ErrNotFound):
			default:
				t.Errorf("%d %v", uid, err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d winners", wins.Load())
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	u := mkUser(t, s, "alice", "admin", UserActive)
	v := mkUser(t, s, "bob", "viewer", UserActive)
	base := now()
	mk := func(h string, uid int64, created, seen time.Time) {
		t.Helper()
		if err := s.CreateSession(ctx, &Session{Hash: []byte(h), UserID: uid, CreatedAt: created, LastSeen: seen, IP: "1.2.3.4", UserAgent: "ua"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateSession(ctx, &Session{Hash: []byte("x"), UserID: 999}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	mk("fresh", u.ID, base, base)
	mk("idle", u.ID, base, base.Add(-13*time.Hour))
	mk("old", v.ID, base.Add(-8*24*time.Hour), base)
	mk("keep", v.ID, base, base)
	if err := s.CreateSession(ctx, &Session{Hash: []byte("fresh"), UserID: u.ID}); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, []byte("fresh"))
	if err != nil || got.UserID != u.ID || got.IP != "1.2.3.4" || got.UserAgent != "ua" || !got.CreatedAt.Equal(base) {
		t.Fatalf("%+v %v", got, err)
	}
	later := base.Add(time.Minute)
	if err := s.TouchSession(ctx, []byte("fresh"), later); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSession(ctx, []byte("fresh"))
	if !got.LastSeen.Equal(later) {
		t.Fatal(got.LastSeen)
	}
	if err := s.TouchSession(ctx, []byte("nope"), later); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	n, err := s.DeleteExpiredSessions(ctx, base.Add(-12*time.Hour), base.Add(-7*24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	for _, h := range []string{"idle", "old"} {
		if _, err := s.GetSession(ctx, []byte(h)); !errors.Is(err, ErrNotFound) {
			t.Fatal(h, err)
		}
	}
	if err := s.DeleteSession(ctx, []byte("keep")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, []byte("keep")); err != nil {
		t.Fatal(err)
	}
	mk("fresh2", u.ID, base, base)
	if n, err := s.DeleteSessionsForUser(ctx, u.ID); err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := s.GetSession(ctx, []byte("fresh")); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestAPITokens(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	exp := now().Add(time.Hour)
	a := &APIToken{Name: "ci", Hash: []byte("h-ci"), Role: "operator", CreatedBy: "alice", ExpiresAt: &exp}
	if err := s.CreateAPIToken(ctx, a); err != nil || a.ID == 0 {
		t.Fatal(err)
	}
	b := &APIToken{Name: "ro", Hash: []byte("h-ro"), Role: "viewer"}
	if err := s.CreateAPIToken(ctx, b); err != nil {
		t.Fatal(err)
	}
	for _, dup := range []*APIToken{
		{Name: "ci", Hash: []byte("other"), Role: "viewer"},
		{Name: "new", Hash: []byte("h-ci"), Role: "viewer"},
	} {
		if err := s.CreateAPIToken(ctx, dup); !errors.Is(err, ErrExists) {
			t.Fatal(err)
		}
	}
	got, err := s.GetAPITokenByHash(ctx, []byte("h-ci"))
	if err != nil || got.Name != "ci" || got.Role != "operator" || got.CreatedBy != "alice" || got.Revoked ||
		got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) || got.LastUsed != nil {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.GetAPITokenByHash(ctx, []byte("zz")); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	at := now()
	if err := s.TouchAPIToken(ctx, a.ID, at); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchAPIToken(ctx, 999, at); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	id, err := s.RevokeAPIToken(ctx, "ci")
	if err != nil || id != a.ID {
		t.Fatalf("%d %v", id, err)
	}
	if _, err := s.RevokeAPIToken(ctx, "ci"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeAPIToken(ctx, "zz"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	list, err := s.ListAPITokens(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "ci" || !list[0].Revoked || list[0].LastUsed == nil || !list[0].LastUsed.Equal(at) || list[1].Revoked {
		t.Fatalf("%+v %v", list, err)
	}
}

func TestAudit(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	for i := 0; i < 10; i++ {
		e := &AuditEntry{PrincipalKind: "user", PrincipalName: "alice", IP: "::1", Event: "projects.create", Target: "app", Result: "ok"}
		if err := s.AppendAudit(ctx, e); err != nil || e.ID == 0 || e.Time.IsZero() {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name        string
		limit       int
		before      int64
		wantLen     int
		wantFirstID int64
		wantLastID  int64
	}{
		{"newest first", 3, 0, 3, 10, 8},
		{"page", 3, 8, 3, 7, 5},
		{"tail", 5, 3, 2, 2, 1},
		{"all", 100, 0, 10, 10, 1},
	} {
		got, err := s.ListAudit(ctx, c.limit, c.before)
		if err != nil || len(got) != c.wantLen || got[0].ID != c.wantFirstID || got[len(got)-1].ID != c.wantLastID {
			t.Fatalf("%s: %v %v", c.name, got, err)
		}
	}
	got, _ := s.ListAudit(ctx, 1, 0)
	if got[0].PrincipalName != "alice" || got[0].Result != "ok" || got[0].Time.Location().String() != "UTC" {
		t.Fatalf("%+v", got[0])
	}
	if n, err := s.PruneAudit(ctx, 4); err != nil || n != 6 {
		t.Fatalf("%d %v", n, err)
	}
	got, _ = s.ListAudit(ctx, 100, 0)
	if len(got) != 4 || got[3].ID != 7 {
		t.Fatalf("%v", got)
	}
	if n, err := s.PruneAudit(ctx, 100); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
}

func TestUserDeleteCascades(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	u := mkUser(t, s, "alice", "admin", UserActive)
	keep := mkUser(t, s, "bob", "viewer", UserActive)
	s.CreateSetupToken(ctx, u.ID, []byte("st"), now().Add(time.Hour))
	s.CreateSetupToken(ctx, keep.ID, []byte("st-keep"), now().Add(time.Hour))
	s.CreateSession(ctx, &Session{Hash: []byte("s"), UserID: u.ID})
	s.CreateSession(ctx, &Session{Hash: []byte("s-keep"), UserID: keep.ID})
	tok := &APIToken{Name: "ci", Hash: []byte("t"), Role: "viewer", CreatedBy: "alice"}
	if err := s.CreateAPIToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, []byte("s")); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.LookupSetupToken(ctx, []byte("st"), now()); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, []byte("s-keep")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupSetupToken(ctx, []byte("st-keep"), now()); err != nil {
		t.Fatal(err)
	}
	// Tokens only carry the creator's name and survive.
	if _, err := s.GetAPITokenByHash(ctx, []byte("t")); err != nil {
		t.Fatal(err)
	}
}
