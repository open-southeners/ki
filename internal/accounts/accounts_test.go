package accounts

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-southeners/ki/internal/auth"
	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/rpc/rpctest"
	"github.com/open-southeners/ki/internal/secret"
	"github.com/open-southeners/ki/internal/store"
)

const goodPassword = "correct horse battery"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	svc   *Service
	st    *store.Store
	clock *clock
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, err := secret.LoadOrCreate(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	svc := New(Options{Store: st, Secret: box, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: clk.Now,
		SetupBase: func() (string, string) { return "https://ki.example.com/", "AB:CD" }})
	return &env{svc: svc, st: st, clock: clk}
}

// totpCode computes the RFC 6238 code for secret at t.
func totpCode(t *testing.T, sec string, at time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(sec)
	if err != nil {
		t.Fatal(err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(at.Unix())/30)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", bin%1000000)
}

func tokenOf(t *testing.T, setupURL string) string {
	t.Helper()
	_, frag, ok := strings.Cut(setupURL, "#")
	if !ok || frag == "" {
		t.Fatalf("setup url %q has no token", setupURL)
	}
	return frag
}

// activate creates username with role and completes its setup. It returns the
// TOTP secret.
func (e *env) activate(t *testing.T, username, role string) string {
	t.Helper()
	ctx := context.Background()
	link, err := e.svc.CreateUser(ctx, username, role)
	if err != nil {
		t.Fatalf("create %s: %v", username, err)
	}
	return e.finishSetup(t, link, goodPassword)
}

func (e *env) finishSetup(t *testing.T, link *SetupLink, password string) string {
	t.Helper()
	ctx := context.Background()
	tok := tokenOf(t, link.SetupURL)
	info, err := e.svc.SetupBegin(ctx, tok)
	if err != nil {
		t.Fatalf("setup begin: %v", err)
	}
	if _, _, err := e.svc.SetupFinish(ctx, SetupFinishRequest{Token: tok, Password: password,
		Code: totpCode(t, info.TOTPSecret, e.clock.Now()), ClientIP: "10.0.0.1"}); err != nil {
		t.Fatalf("setup finish: %v", err)
	}
	return info.TOTPSecret
}

func (e *env) login(t *testing.T, username, password, sec, ip string) (string, error) {
	t.Helper()
	code := "000000"
	if sec != "" {
		code = totpCode(t, sec, e.clock.Now())
	}
	id, _, err := e.svc.Login(context.Background(), LoginRequest{Username: username, Password: password, Code: code, ClientIP: ip})
	return id, err
}

func (e *env) auditActions(t *testing.T) []string {
	t.Helper()
	es, err := e.svc.ListAudit(context.Background(), 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(es) - 1; i >= 0; i-- {
		out = append(out, es[i].Action+":"+es[i].Result)
	}
	return out
}

func TestSetupFlow(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		password string
		code     func(sec string, e *env) string
		token    func(tok string) string
		want     error
	}{
		{"ok", goodPassword, nil, nil, nil},
		{"wrong code", goodPassword, func(string, *env) string { return "000000" }, nil, ErrInvalidSetup},
		{"short password", "short", nil, nil, ErrWeakPassword},
		{"password equals username", "alice-the-user", nil, nil, ErrWeakPassword},
		{"unknown token", goodPassword, nil, func(string) string { return "nope" }, ErrInvalidSetup},
		{"empty token", goodPassword, nil, func(string) string { return "" }, ErrInvalidSetup},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			name := "alice"
			if tt.password == "alice-the-user" {
				name = "alice-the-user"
			}
			link, err := e.svc.CreateUser(ctx, name, "operator")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(link.SetupURL, "https://ki.example.com/setup#") || link.FingerprintSHA256 != "AB:CD" {
				t.Fatalf("link = %+v", link)
			}
			if want := e.clock.Now().Add(SetupTTL); !link.ExpiresAt.Equal(want) {
				t.Fatalf("expires_at = %v, want %v", link.ExpiresAt, want)
			}
			tok := tokenOf(t, link.SetupURL)
			info, err := e.svc.SetupBegin(ctx, tok)
			if err != nil {
				t.Fatal(err)
			}
			if info.Username != name || info.Role != "operator" || !strings.HasPrefix(info.OTPAuthURI, "otpauth://totp/") {
				t.Fatalf("info = %+v", info)
			}
			if u, _ := url.Parse(info.OTPAuthURI); u.Query().Get("secret") != info.TOTPSecret {
				t.Fatal("otpauth uri does not carry the secret")
			}
			code := totpCode(t, info.TOTPSecret, e.clock.Now())
			if tt.code != nil {
				code = tt.code(info.TOTPSecret, e)
			}
			use := tok
			if tt.token != nil {
				use = tt.token(tok)
			}
			sid, pr, err := e.svc.SetupFinish(ctx, SetupFinishRequest{Token: use, Password: tt.password, Code: code, ClientIP: "10.0.0.1"})
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("err = %v, want %v", err, tt.want)
				}
				if sid != "" {
					t.Fatal("session returned on failure")
				}
				if u, _ := e.st.GetUser(ctx, name); u.Status != store.UserPending {
					t.Fatalf("status = %s", u.Status)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if pr.Kind != auth.KindUser || pr.Name != name || pr.Role != auth.RoleOperator {
				t.Fatalf("principal = %+v", pr)
			}
			if got, err := e.svc.Session(ctx, sid); err != nil || got.ID != pr.ID {
				t.Fatalf("session = %+v, %v", got, err)
			}
			u, _ := e.st.GetUser(ctx, name)
			if u.Status != store.UserActive || len(u.TOTPSecretEnc) == 0 || u.TOTPPendingEnc != nil || u.PasswordHash == "" {
				t.Fatalf("user = %+v", u)
			}
			// The token is single use.
			if _, _, err := e.svc.SetupFinish(ctx, SetupFinishRequest{Token: tok, Password: tt.password, Code: code, ClientIP: "10.0.0.1"}); !errors.Is(err, ErrInvalidSetup) {
				t.Fatalf("reuse err = %v", err)
			}
			if _, err := e.svc.SetupBegin(ctx, tok); !errors.Is(err, ErrInvalidSetup) {
				t.Fatalf("begin after use err = %v", err)
			}
		})
	}
}

func TestSetupBeginRegeneratesAndExpires(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	link, _ := e.svc.CreateUser(ctx, "bob", "viewer")
	tok := tokenOf(t, link.SetupURL)
	a, err := e.svc.SetupBegin(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := e.svc.SetupBegin(ctx, tok)
	if a.TOTPSecret == b.TOTPSecret {
		t.Fatal("secret not regenerated")
	}
	// The first secret no longer confirms the setup.
	if _, _, err := e.svc.SetupFinish(ctx, SetupFinishRequest{Token: tok, Password: goodPassword,
		Code: totpCode(t, a.TOTPSecret, e.clock.Now())}); !errors.Is(err, ErrInvalidSetup) {
		t.Fatalf("old secret err = %v", err)
	}
	e.clock.Advance(SetupTTL + time.Second)
	if _, err := e.svc.SetupBegin(ctx, tok); !errors.Is(err, ErrInvalidSetup) {
		t.Fatalf("expired err = %v", err)
	}
	// A new link replaces the old one.
	link2, _ := e.svc.ResetUser(ctx, "bob")
	if _, err := e.svc.SetupBegin(ctx, tokenOf(t, link2.SetupURL)); err != nil {
		t.Fatal(err)
	}
}

func TestSetupConcurrentFinish(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	link, _ := e.svc.CreateUser(ctx, "carol", "admin")
	tok := tokenOf(t, link.SetupURL)
	info, _ := e.svc.SetupBegin(ctx, tok)
	code := totpCode(t, info.TOTPSecret, e.clock.Now())
	var ok, invalid atomic.Int32
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := e.svc.SetupFinish(ctx, SetupFinishRequest{Token: tok, Password: goodPassword, Code: code, ClientIP: "10.0.0.1"})
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrInvalidSetup):
				invalid.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || invalid.Load() != 3 {
		t.Fatalf("ok=%d invalid=%d", ok.Load(), invalid.Load())
	}
}

func TestLogin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sec := e.activate(t, "alice", "admin")
	e.activate(t, "dave", "viewer")
	e.st.GetUser(ctx, "dave")
	if _, err := e.svc.UpdateUser(ctx, "dave", nil, ptr(true)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateUser(ctx, "erin", "viewer"); err != nil {
		t.Fatal(err)
	}
	// Setup consumed the code of the current step; move on.
	e.clock.Advance(30 * time.Second)

	tests := []struct {
		name, user, pass, code string
		want                   error
	}{
		{"wrong password", "alice", "nope-nope-nope!", "", ErrInvalidCredentials},
		{"wrong code", "alice", goodPassword, "000000", ErrInvalidCredentials},
		{"unknown user", "ghost", goodPassword, "123456", ErrInvalidCredentials},
		{"disabled", "dave", goodPassword, "123456", ErrInvalidCredentials},
		{"pending", "erin", goodPassword, "123456", ErrInvalidCredentials},
		{"ok", "alice", goodPassword, "", nil},
		{"replayed code", "alice", goodPassword, "", ErrInvalidCredentials},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code := tt.code
			if code == "" {
				code = totpCode(t, sec, e.clock.Now())
			}
			id, pr, err := e.svc.Login(ctx, LoginRequest{Username: tt.user, Password: tt.pass, Code: code, ClientIP: "10.0.0.2", UserAgent: "ua"})
			if tt.want != nil {
				if !errors.Is(err, tt.want) || id != "" {
					t.Fatalf("err = %v id = %q", err, id)
				}
				return
			}
			if err != nil || id == "" || pr.Name != "alice" || pr.Role != auth.RoleAdmin {
				t.Fatalf("login = %q %+v %v", id, pr, err)
			}
			if p, err := e.svc.Session(ctx, id); err != nil || p.Name != "alice" {
				t.Fatalf("session = %+v %v", p, err)
			}
			u, _ := e.st.GetUser(ctx, "alice")
			if u.LastLoginAt == nil {
				t.Fatal("last_login_at not set")
			}
		})
	}
	got := e.auditActions(t)
	var oks, fails int
	for _, a := range got {
		switch a {
		case "auth.login:ok":
			oks++
		case "auth.login:fail", "auth.login:replay":
			fails++
		}
	}
	if oks != 1 || fails != 6 {
		t.Fatalf("audit = %v (ok=%d fail=%d)", got, oks, fails)
	}
	// Logout kills the session and is audited.
	id, _ := e.loginFresh(t, "alice", sec)
	if err := e.svc.Logout(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Session(ctx, id); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("after logout err = %v", err)
	}
	if err := e.svc.Logout(ctx, "unknown"); err != nil {
		t.Fatal(err)
	}
	if !contains(e.auditActions(t), "auth.logout:ok") {
		t.Fatalf("no logout audit: %v", e.auditActions(t))
	}
}

func (e *env) loginFresh(t *testing.T, user, sec string) (string, error) {
	t.Helper()
	e.clock.Advance(30 * time.Second)
	return e.login(t, user, goodPassword, sec, "10.0.0.3")
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

func ptr[T any](v T) *T { return &v }

func TestLockout(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sec := e.activate(t, "alice", "admin")
	e.clock.Advance(30 * time.Second)
	for i := range 5 {
		// A distinct client each time so only the account lockout can trigger.
		if _, err := e.login(t, "alice", "wrong-password-1", sec, fmt.Sprintf("10.1.0.%d", i)); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	u, _ := e.st.GetUser(ctx, "alice")
	if u.LockedUntil == nil || !u.LockedUntil.After(e.clock.Now()) {
		t.Fatalf("locked_until = %v", u.LockedUntil)
	}
	if !contains(e.auditActions(t), "auth.lockout:locked") {
		t.Fatalf("no lockout audit: %v", e.auditActions(t))
	}
	// Correct credentials are refused while locked.
	if _, err := e.login(t, "alice", goodPassword, sec, "10.1.0.9"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("locked login err = %v", err)
	}
	e.clock.Advance(16 * time.Minute)
	if _, err := e.login(t, "alice", goodPassword, sec, "10.1.0.9"); err != nil {
		t.Fatalf("after lock: %v", err)
	}
	u, _ = e.st.GetUser(ctx, "alice")
	if u.FailedLogins != 0 || u.LockedUntil != nil {
		t.Fatalf("state not reset: %+v", u)
	}
}

func TestIPRateLimit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sec := e.activate(t, "alice", "admin")
	for i := range 10 {
		// Unknown users: no account lockout, only the IP counter grows.
		if _, err := e.login(t, fmt.Sprintf("ghost%d", i), "whatever-whatever", "", "9.9.9.9"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	_, _, err := e.svc.Login(ctx, LoginRequest{Username: "alice", Password: goodPassword, Code: "000000", ClientIP: "9.9.9.9"})
	var rl *RateLimitedError
	if !errors.Is(err, ErrRateLimited) || !errors.As(err, &rl) || rl.RetryAfter <= 0 || rl.RetryAfter > auth.DefaultIPWindow {
		t.Fatalf("err = %v", err)
	}
	// Other clients are unaffected; the window slides.
	e.clock.Advance(30 * time.Second)
	if _, err := e.login(t, "alice", goodPassword, sec, "8.8.8.8"); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(auth.DefaultIPWindow + time.Second)
	if _, err := e.login(t, "alice", goodPassword, sec, "9.9.9.9"); err != nil {
		t.Fatalf("after window: %v", err)
	}
}

func TestSessionExpiry(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		steps []time.Duration // clock advances, Session checked after each
		want  []bool          // true = authenticated
	}{
		{"within idle", []time.Duration{time.Hour}, []bool{true}},
		{"idle expiry", []time.Duration{13 * time.Hour}, []bool{false}},
		{"activity keeps alive", []time.Duration{10 * time.Hour, 10 * time.Hour, 10 * time.Hour}, []bool{true, true, true}},
		{"absolute expiry", []time.Duration{10 * time.Hour, 10 * time.Hour, 10 * time.Hour, 10 * time.Hour, 10 * time.Hour, 10 * time.Hour,
			10 * time.Hour, 10 * time.Hour, 10 * time.Hour, 10 * time.Hour, 10 * time.Hour, 10 * time.Hour, 10 * time.Hour, 10 * time.Hour,
			10 * time.Hour, 10 * time.Hour, 10 * time.Hour}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			sec := e.activate(t, "alice", "admin")
			id, err := e.loginFresh(t, "alice", sec)
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == nil { // absolute: keep it active until past 7 days
				for i := 0; i < 16; i++ {
					e.clock.Advance(10 * time.Hour)
					if _, err := e.svc.Session(ctx, id); err != nil {
						t.Fatalf("step %d: %v", i, err)
					}
				}
				// 160 h < 168 h; one more step crosses the absolute limit.
				e.clock.Advance(10 * time.Hour)
				if _, err := e.svc.Session(ctx, id); !errors.Is(err, ErrUnauthenticated) {
					t.Fatalf("past absolute limit: %v", err)
				}
				return
			}
			for i, d := range tt.steps {
				e.clock.Advance(d)
				_, err := e.svc.Session(ctx, id)
				if (err == nil) != tt.want[i] {
					t.Fatalf("step %d: err = %v, want auth=%v", i, err, tt.want[i])
				}
				if err != nil && !errors.Is(err, ErrUnauthenticated) {
					t.Fatalf("err = %v", err)
				}
			}
		})
	}
}

func TestSessionTouchAndUserState(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sec := e.activate(t, "alice", "admin")
	e.activate(t, "bob", "viewer")
	bsec := ""
	_ = bsec
	id, _ := e.loginFresh(t, "alice", sec)
	hash := auth.HashSessionID(id)
	se, _ := e.st.GetSession(ctx, hash)
	start := se.LastSeen

	e.clock.Advance(30 * time.Second)
	e.svc.Session(ctx, id)
	if se, _ = e.st.GetSession(ctx, hash); !se.LastSeen.Equal(start) {
		t.Fatal("touched within a minute")
	}
	e.clock.Advance(40 * time.Second)
	e.svc.Session(ctx, id)
	if se, _ = e.st.GetSession(ctx, hash); !se.LastSeen.After(start) {
		t.Fatal("not touched after a minute")
	}

	// Disabling a user kills its session.
	if _, err := e.svc.UpdateUser(ctx, "alice", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateUser(ctx, "bob", nil, ptr(true)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Session(ctx, "garbage"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal(err)
	}
	if _, err := e.svc.Session(ctx, ""); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal(err)
	}
}

func TestTokens(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var revoked []int64
	e.svc.OnRevokeToken(func(id int64) { revoked = append(revoked, id) })

	if _, err := e.svc.CreateToken(ctx, "tester", "bad name", "admin", 0); err == nil {
		t.Fatal("bad name accepted")
	}
	if _, err := e.svc.CreateToken(ctx, "tester", "ci", "root", 0); err == nil {
		t.Fatal("bad role accepted")
	}
	if _, err := e.svc.CreateToken(ctx, "tester", "ci", "admin", MaxTokenLifetime+time.Hour); err == nil {
		t.Fatal("too long expiry accepted")
	}
	forever, err := e.svc.CreateToken(ctx, "tester", "forever", "operator", 0)
	if err != nil {
		t.Fatal(err)
	}
	short, err := e.svc.CreateToken(ctx, "tester", "short", "viewer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateToken(ctx, "tester", "short", "viewer", 0); !errors.Is(err, store.ErrExists) {
		t.Fatalf("duplicate err = %v", err)
	}
	if !strings.HasPrefix(forever.Token, "ki_") || forever.ExpiresAt != nil || short.ExpiresAt == nil {
		t.Fatalf("tokens = %+v %+v", forever, short)
	}

	pr, err := e.svc.Token(ctx, forever.Token)
	if err != nil || pr.Kind != auth.KindToken || pr.Name != "forever" || pr.Role != auth.RoleOperator || pr.ID == 0 {
		t.Fatalf("principal = %+v %v", pr, err)
	}
	for _, bad := range []string{"", "ki_", "ki_unknown", "Bearer " + forever.Token, "x" + forever.Token} {
		if _, err := e.svc.Token(ctx, bad); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("token %q err = %v", bad, err)
		}
	}

	// last_used is touched at most once a minute.
	list, _ := e.svc.ListTokens(ctx)
	var first *time.Time
	for _, ti := range list {
		if ti.Name == "forever" {
			first = ti.LastUsedAt
		}
	}
	if first == nil {
		t.Fatal("last_used not set")
	}
	e.clock.Advance(10 * time.Second)
	e.svc.Token(ctx, forever.Token)
	list, _ = e.svc.ListTokens(ctx)
	for _, ti := range list {
		if ti.Name == "forever" && !ti.LastUsedAt.Equal(*first) {
			t.Fatal("touched within a minute")
		}
	}

	// Expiry.
	e.clock.Advance(time.Hour)
	if _, err := e.svc.Token(ctx, short.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired err = %v", err)
	}
	if _, err := e.svc.Token(ctx, forever.Token); err != nil {
		t.Fatal(err)
	}

	// Revocation.
	if err := e.svc.RevokeToken(ctx, "forever"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Token(ctx, forever.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked err = %v", err)
	}
	if len(revoked) != 1 || revoked[0] != pr.ID {
		t.Fatalf("callbacks = %v, want [%d]", revoked, pr.ID)
	}
	if err := e.svc.RevokeToken(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if len(revoked) != 1 {
		t.Fatal("callback fired for a missing token")
	}
}

func TestUserRules(t *testing.T) {
	ctx := context.Background()
	t.Run("username validation", func(t *testing.T) {
		e := newEnv(t)
		for _, bad := range []string{"", "A", "Admin", "1abc", "a", "has space", strings.Repeat("a", 33)} {
			_, err := e.svc.CreateUser(ctx, bad, "viewer")
			var pe *paramError
			if !errors.As(err, &pe) {
				t.Errorf("%q: err = %v", bad, err)
			}
		}
		if _, err := e.svc.CreateUser(ctx, "ok.user-1_x", "bogus"); err == nil {
			t.Error("bad role accepted")
		}
		if _, err := e.svc.CreateUser(ctx, "dup", "viewer"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.CreateUser(ctx, "dup", "viewer"); !errors.Is(err, store.ErrExists) {
			t.Errorf("dup err = %v", err)
		}
	})

	t.Run("last admin protected", func(t *testing.T) {
		e := newEnv(t)
		e.activate(t, "root", "admin")
		tests := []struct {
			name string
			op   func() error
		}{
			{"delete", func() error { return e.svc.DeleteUser(ctx, "root") }},
			{"disable", func() error { _, err := e.svc.UpdateUser(ctx, "root", nil, ptr(true)); return err }},
			{"demote", func() error { _, err := e.svc.UpdateUser(ctx, "root", ptr("viewer"), nil); return err }},
		}
		for _, tt := range tests {
			var pe *paramError
			if err := tt.op(); !errors.As(err, &pe) {
				t.Errorf("%s: err = %v", tt.name, err)
			}
		}
		// With a second admin the first may go.
		e.activate(t, "root2", "admin")
		if _, err := e.svc.UpdateUser(ctx, "root", ptr("viewer"), nil); err != nil {
			t.Fatal(err)
		}
		var pe *paramError
		if err := e.svc.DeleteUser(ctx, "root2"); !errors.As(err, &pe) {
			t.Fatalf("delete last: %v", err)
		}
		if err := e.svc.DeleteUser(ctx, "root"); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.DeleteUser(ctx, "root"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("revocation callbacks", func(t *testing.T) {
		e := newEnv(t)
		var got []int64
		e.svc.OnRevokeUser(func(id int64) { got = append(got, id) })
		e.activate(t, "root", "admin")
		bsec := e.activate(t, "bob", "viewer")
		bob, _ := e.st.GetUser(ctx, "bob")
		id, err := e.loginFresh(t, "bob", bsec)
		if err != nil {
			t.Fatal(err)
		}

		steps := []struct {
			name string
			op   func() error
		}{
			{"role change", func() error { _, err := e.svc.UpdateUser(ctx, "bob", ptr("operator"), nil); return err }},
			{"same role is a no-op", func() error { _, err := e.svc.UpdateUser(ctx, "bob", ptr("operator"), nil); return err }},
			{"disable", func() error { _, err := e.svc.UpdateUser(ctx, "bob", nil, ptr(true)); return err }},
			{"reset", func() error { _, err := e.svc.ResetUser(ctx, "bob"); return err }},
			{"delete", func() error { return e.svc.DeleteUser(ctx, "bob") }},
		}
		wantCount := []int{1, 1, 2, 3, 4}
		for i, s := range steps {
			if err := s.op(); err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			if len(got) != wantCount[i] {
				t.Fatalf("%s: callbacks = %v, want %d", s.name, got, wantCount[i])
			}
		}
		for _, g := range got {
			if g != bob.ID {
				t.Fatalf("callback id = %d, want %d", g, bob.ID)
			}
		}
		if _, err := e.svc.Session(ctx, id); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("session survived: %v", err)
		}
	})

	t.Run("reset clears credentials", func(t *testing.T) {
		e := newEnv(t)
		sec := e.activate(t, "root", "admin")
		id, _ := e.loginFresh(t, "root", sec)
		link, err := e.svc.ResetUser(ctx, "root")
		if err != nil {
			t.Fatal(err)
		}
		u, _ := e.st.GetUser(ctx, "root")
		if u.Status != store.UserPending || u.PasswordHash != "" || u.TOTPSecretEnc != nil || u.FailedLogins != 0 {
			t.Fatalf("user = %+v", u)
		}
		if _, err := e.svc.Session(ctx, id); !errors.Is(err, ErrUnauthenticated) {
			t.Fatal("session survived reset")
		}
		e.finishSetup(t, link, "another good password")
		if _, err := e.login(t, "root", goodPassword, "", "1.1.1.1"); err == nil {
			t.Fatal("old password works")
		}
	})

	t.Run("re-enable", func(t *testing.T) {
		e := newEnv(t)
		e.activate(t, "bob", "viewer")
		e.svc.UpdateUser(ctx, "bob", nil, ptr(true))
		u, err := e.svc.UpdateUser(ctx, "bob", nil, ptr(false))
		if err != nil || u.Status != store.UserActive {
			t.Fatalf("u = %+v err = %v", u, err)
		}
	})
}

func TestPanelSetup(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	link, err := e.svc.PanelSetup(ctx, "")
	if err != nil || link.User.Username != "admin" || link.User.Role != "admin" || link.User.Status != store.UserPending {
		t.Fatalf("link = %+v err = %v", link, err)
	}
	// Reuse of the pending admin issues a new link and invalidates the old one.
	link2, err := e.svc.PanelSetup(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetupBegin(ctx, tokenOf(t, link.SetupURL)); !errors.Is(err, ErrInvalidSetup) {
		t.Fatalf("old link err = %v", err)
	}
	e.finishSetup(t, link2, goodPassword)
	var pe *paramError
	if _, err := e.svc.PanelSetup(ctx, "other"); !errors.As(err, &pe) || !strings.Contains(err.Error(), "users reset") {
		t.Fatalf("err = %v", err)
	}
	if _, err := newEnv(t).svc.PanelSetup(ctx, "Bad Name"); !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
}

func TestStartCleansUp(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	sec := e.activate(t, "alice", "admin")
	id, _ := e.loginFresh(t, "alice", sec)
	for range 3 {
		e.svc.audit(ctx, "user", "x", "", "m", "", "ok")
	}
	e.clock.Advance(8 * 24 * time.Hour)
	done := make(chan struct{})
	go func() { e.svc.Start(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := e.st.GetSession(context.Background(), auth.HashSessionID(id)); errors.Is(err, store.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired session not deleted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestAuditMiddleware(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mw := e.svc.AuditMiddleware()
	p := rpc.NewPeer(nil, rpc.NewRouter())
	auth.SetPrincipal(p, auth.Principal{Kind: auth.KindToken, ID: 3, Name: "ci", Role: auth.RoleAdmin})
	auth.SetClientIP(p, "203.0.113.5")

	ok := func(context.Context, *rpc.Peer, json.RawMessage) (any, error) { return nil, nil }
	notFound := func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
		return nil, fmt.Errorf("wrapped: %w", rpc.NewError(rpc.CodeNotFound, "x"))
	}
	boom := func(context.Context, *rpc.Peer, json.RawMessage) (any, error) { return nil, errors.New("db down") }

	tests := []struct {
		method string
		h      rpc.Handler
		params string
		target string
		result string
		logged bool
	}{
		{"projects.delete", ok, `{"slug":"blog","force":true}`, "blog", "ok", true},
		{"projects.env.set", ok, `{"project":"blog","env":{"SECRET":"hunter2"}}`, "blog", "ok", true},
		{"users.reset", notFound, `{"username":"zed"}`, "zed", "-32006", true},
		{"tokens.create", ok, `{"name":"ci","role":"admin"}`, "ci", "ok", true},
		{"deploys.trigger", boom, `{"id":42}`, "42", "-32603", true},
		{"containers.restart", ok, `{"values":["a","b"]}`, "", "ok", true},
		{"containers.list", ok, `{"slug":"x"}`, "", "", false},
		{"unknown.method", ok, `{"slug":"x"}`, "", "", false},
	}
	for _, tt := range tests {
		if _, err := mw(tt.method, tt.h)(ctx, p, json.RawMessage(tt.params)); (err != nil) != (tt.result != "ok" && tt.logged) {
			t.Fatalf("%s: err = %v", tt.method, err)
		}
	}
	es, err := e.svc.ListAudit(ctx, 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, tt := range tests {
		if tt.logged {
			want = append(want, tt.method+"|"+tt.target+"|"+tt.result)
		}
	}
	var got []string
	for i := len(es) - 1; i >= 0; i-- {
		x := es[i]
		if x.PrincipalKind != "token" || x.PrincipalName != "ci" || x.IP != "203.0.113.5" {
			t.Fatalf("entry = %+v", x)
		}
		got = append(got, x.Action+"|"+x.Target+"|"+x.Result)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit = %v\nwant    %v", got, want)
	}
	raw, _ := json.Marshal(es[0])
	for _, k := range []string{`"id"`, `"time"`, `"principal_kind"`, `"principal_name"`, `"ip"`, `"action"`, `"target"`, `"result"`} {
		if !strings.Contains(string(raw), k) {
			t.Fatalf("audit json %s lacks %s", raw, k)
		}
	}
}

// rpcClient serves svc over an in-memory pipe as principal pr.
func rpcClient(t *testing.T, svc *Service, pr auth.Principal) *rpc.Peer {
	t.Helper()
	r := rpc.NewRouter()
	r.Use(svc.AuditMiddleware())
	svc.Register(r)
	a, b := rpctest.Pipe()
	srv := rpc.NewPeer(a, r)
	auth.SetPrincipal(srv, pr)
	cli := rpc.NewPeer(b, nil)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, p := range []*rpc.Peer{srv, cli} {
		wg.Add(1)
		go func() { defer wg.Done(); p.Run(ctx) }()
	}
	t.Cleanup(func() { cancel(); srv.Close(); cli.Close(); wg.Wait() })
	return cli
}

func TestRPC(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cli := rpcClient(t, e.svc, auth.Principal{Kind: auth.KindLocal, Name: "local", Role: auth.RoleAdmin})

	var who struct{ Kind, Name, Role string }
	if err := cli.Call(ctx, "auth.whoami", nil, &who); err != nil || who.Kind != "local" || who.Role != "admin" {
		t.Fatalf("whoami = %+v %v", who, err)
	}

	var setup SetupLink
	if err := cli.Call(ctx, "panel.setup", map[string]any{"username": "boss"}, &setup); err != nil {
		t.Fatal(err)
	}
	if setup.User.Username != "boss" || setup.FingerprintSHA256 != "AB:CD" || setup.SetupURL == "" || setup.ExpiresAt.IsZero() {
		t.Fatalf("setup = %+v", setup)
	}
	e.finishSetup(t, &setup, goodPassword)

	rpcCode := func(err error) int {
		var re *rpc.Error
		if !errors.As(err, &re) {
			t.Fatalf("not an rpc error: %v", err)
		}
		return re.Code
	}
	var res json.RawMessage
	tests := []struct {
		method string
		params any
		code   int
	}{
		{"panel.setup", map[string]any{}, rpc.CodeInvalidParams},
		{"users.create", map[string]any{"username": "BAD", "role": "viewer"}, rpc.CodeInvalidParams},
		{"users.create", map[string]any{"username": "bob", "role": "viewer"}, 0},
		{"users.create", map[string]any{"username": "bob", "role": "viewer"}, rpc.CodeAlreadyExists},
		{"users.update", map[string]any{"username": "nobody", "role": "admin"}, rpc.CodeNotFound},
		{"users.update", map[string]any{"username": "bob", "role": "operator"}, 0},
		{"users.update", map[string]any{"username": "boss", "disabled": true}, rpc.CodeInvalidParams},
		{"users.reset", map[string]any{"username": "nobody"}, rpc.CodeNotFound},
		{"users.delete", map[string]any{"username": "boss"}, rpc.CodeInvalidParams},
		{"users.delete", map[string]any{"username": "bob"}, 0},
		{"users.create", []string{"x"}, rpc.CodeInvalidParams},
		{"tokens.create", map[string]any{"name": "ci", "role": "viewer", "expires_in": "720h"}, 0},
		{"tokens.create", map[string]any{"name": "ci2", "role": "viewer", "expires_in": "9999h"}, rpc.CodeInvalidParams},
		{"tokens.create", map[string]any{"name": "ci3", "role": "viewer", "expires_in": "soon"}, rpc.CodeInvalidParams},
		{"tokens.create", map[string]any{"name": "ci", "role": "viewer"}, rpc.CodeAlreadyExists},
		{"tokens.revoke", map[string]any{"name": "nope"}, rpc.CodeNotFound},
		{"tokens.revoke", map[string]any{"name": "ci"}, 0},
		{"audit.list", map[string]any{"limit": -1}, rpc.CodeInvalidParams},
	}
	for _, tt := range tests {
		err := cli.Call(ctx, tt.method, tt.params, &res)
		if tt.code == 0 {
			if err != nil {
				t.Fatalf("%s %v: %v", tt.method, tt.params, err)
			}
			continue
		}
		if err == nil || rpcCode(err) != tt.code {
			t.Fatalf("%s %v: err = %v, want code %d", tt.method, tt.params, err, tt.code)
		}
	}

	var users []map[string]any
	if err := cli.Call(ctx, "users.list", nil, &users); err != nil || len(users) != 1 {
		t.Fatalf("users = %v %v", users, err)
	}
	if users[0]["username"] != "boss" || users[0]["totp_enrolled"] != true || users[0]["status"] != "active" {
		t.Fatalf("user = %v", users[0])
	}
	var toks []map[string]any
	if err := cli.Call(ctx, "tokens.list", nil, &toks); err != nil || len(toks) != 1 || toks[0]["created_by"] != "local" {
		t.Fatalf("tokens = %v %v", toks, err)
	}
	var created CreatedToken
	if err := cli.Call(ctx, "tokens.create", map[string]any{"name": "fresh", "role": "admin"}, &created); err != nil || created.Token == "" {
		t.Fatalf("created = %+v %v", created, err)
	}
	var del map[string]any
	if err := cli.Call(ctx, "tokens.revoke", map[string]any{"name": "fresh"}, &del); err != nil || del == nil {
		t.Fatalf("revoke result = %v %v", del, err)
	}
	var entries []AuditEntry
	if err := cli.Call(ctx, "audit.list", map[string]any{"limit": 2}, &entries); err != nil || len(entries) != 2 || entries[0].ID < entries[1].ID {
		t.Fatalf("entries = %+v %v", entries, err)
	}
}

// The agent installs the role check first (server.New) and the audit
// middleware after it, so a call the role check denies never reaches the audit
// log while an allowed one does.
func TestAuditRunsAfterRoleCheck(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	serve := func(pr auth.Principal) *rpc.Peer {
		r := rpc.NewRouter()
		r.Use(auth.RequireRoles(auth.MethodRoles))
		r.Use(e.svc.AuditMiddleware())
		e.svc.Register(r)
		a, b := rpctest.Pipe()
		srv := rpc.NewPeer(a, r)
		auth.SetPrincipal(srv, pr)
		cli := rpc.NewPeer(b, nil)
		cctx, stop := context.WithCancel(ctx)
		var wg sync.WaitGroup
		for _, p := range []*rpc.Peer{srv, cli} {
			wg.Add(1)
			go func() { defer wg.Done(); p.Run(cctx) }()
		}
		t.Cleanup(func() { stop(); srv.Close(); cli.Close(); wg.Wait() })
		return cli
	}

	viewer := serve(auth.Principal{Kind: auth.KindToken, ID: 1, Name: "ro", Role: auth.RoleViewer})
	err := viewer.Call(ctx, "tokens.create", map[string]any{"name": "x", "role": "viewer"}, nil)
	var re *rpc.Error
	if !errors.As(err, &re) || re.Code != rpc.CodeUnauthorized {
		t.Fatalf("viewer tokens.create = %v, want forbidden", err)
	}
	if es, err := e.svc.ListAudit(ctx, 10, 0); err != nil || len(es) != 0 {
		t.Fatalf("denied call was audited: %+v %v", es, err)
	}

	admin := serve(auth.Principal{Kind: auth.KindToken, ID: 2, Name: "root", Role: auth.RoleAdmin})
	if err := admin.Call(ctx, "tokens.create", map[string]any{"name": "x", "role": "viewer"}, nil); err != nil {
		t.Fatal(err)
	}
	es, err := e.svc.ListAudit(ctx, 10, 0)
	if err != nil || len(es) != 1 || es[0].Action != "tokens.create" || es[0].PrincipalName != "root" || es[0].Result != "ok" {
		t.Fatalf("audit = %+v %v", es, err)
	}
}
