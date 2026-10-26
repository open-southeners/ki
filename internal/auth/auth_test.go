package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/rpc/rpctest"
)

func TestRole(t *testing.T) {
	tests := []struct {
		a, b Role
		want bool
	}{
		{RoleAdmin, RoleViewer, true},
		{RoleAdmin, RoleAdmin, true},
		{RoleOperator, RoleAdmin, false},
		{RoleViewer, RoleOperator, false},
		{RoleOperator, RoleViewer, true},
		{Role(0), RoleViewer, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s>=%s", tt.a, tt.b), func(t *testing.T) {
			if got := tt.a.AtLeast(tt.b); got != tt.want {
				t.Fatalf("AtLeast = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRoleJSON(t *testing.T) {
	for _, r := range []Role{RoleViewer, RoleOperator, RoleAdmin} {
		b, err := json.Marshal(r)
		if err != nil || string(b) != `"`+r.String()+`"` {
			t.Fatalf("marshal %v: %s %v", r, b, err)
		}
		var back Role
		if err := json.Unmarshal(b, &back); err != nil || back != r {
			t.Fatalf("unmarshal %s: %v %v", b, back, err)
		}
	}
	for _, bad := range []string{`"root"`, `1`, `""`} {
		var r Role
		if err := json.Unmarshal([]byte(bad), &r); err == nil {
			t.Errorf("unmarshal %s: want error", bad)
		}
	}
	if _, err := json.Marshal(Role(0)); err == nil {
		t.Error("marshal invalid role: want error")
	}
	if _, err := ParseRole("x"); err == nil {
		t.Error("ParseRole(x): want error")
	}
}

func newTestPeer(t *testing.T) *rpc.Peer {
	t.Helper()
	ca, _ := rpctest.Pipe()
	p := rpc.NewPeer(ca, rpc.NewRouter())
	t.Cleanup(func() { p.Close() })
	return p
}

func TestPrincipalRoundTrip(t *testing.T) {
	p := newTestPeer(t)
	if _, ok := PrincipalOf(p); ok {
		t.Fatal("unexpected principal")
	}
	want := Principal{Kind: KindUser, ID: 7, Name: "bob", Role: RoleOperator}
	SetPrincipal(p, want)
	if got, ok := PrincipalOf(p); !ok || got != want {
		t.Fatalf("got %+v %v", got, ok)
	}
}

func TestRequireRoles(t *testing.T) {
	table := map[string]Role{"v": RoleViewer, "o": RoleOperator, "a": RoleAdmin}
	mw := RequireRoles(table)
	next := func(context.Context, *rpc.Peer, json.RawMessage) (any, error) { return "ok", nil }

	tests := []struct {
		name   string
		method string
		role   Role // 0 = no principal
		want   string
	}{
		{"viewer ok", "v", RoleViewer, ""},
		{"operator on viewer", "v", RoleOperator, ""},
		{"viewer on operator", "o", RoleViewer, "forbidden: requires operator"},
		{"operator on admin", "a", RoleOperator, "forbidden: requires admin"},
		{"admin on admin", "a", RoleAdmin, ""},
		{"unknown method", "zzz", RoleAdmin, "forbidden: requires admin"},
		{"unauthenticated", "v", 0, "unauthenticated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPeer(t)
			if tt.role != 0 {
				SetPrincipal(p, Principal{Kind: KindToken, Name: "x", Role: tt.role})
			}
			out, err := mw(tt.method, next)(context.Background(), p, nil)
			if tt.want == "" {
				if err != nil || out != "ok" {
					t.Fatalf("out=%v err=%v", out, err)
				}
				return
			}
			var re *rpc.Error
			if !errors.As(err, &re) || re.Code != rpc.CodeUnauthorized || re.Message != tt.want {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestMethodRoles(t *testing.T) {
	want := map[string]Role{
		"rpc.discover": RoleViewer, "auth.whoami": RoleViewer, "edge.status": RoleViewer,
		"containers.restart": RoleOperator, "deploys.trigger": RoleOperator, "deploys.rollback": RoleOperator,
		"projects.services": RoleViewer, "projects.services.start": RoleOperator,
		"projects.services.stop": RoleOperator, "projects.services.restart": RoleOperator,
		"containers.remove": RoleAdmin, "users.create": RoleAdmin, "panel.setup": RoleAdmin,
		"tokens.revoke": RoleAdmin, "audit.list": RoleAdmin, "enrollment.start": RoleAdmin,
	}
	for m, r := range want {
		if MethodRoles[m] != r {
			t.Errorf("%s = %v, want %v", m, MethodRoles[m], r)
		}
	}
	for m, r := range MethodRoles {
		if !r.Valid() {
			t.Errorf("%s has invalid role", m)
		}
	}
}

func TestPasswordRoundTrip(t *testing.T) {
	ctx := context.Background()
	h, err := HashPassword(ctx, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") || strings.Count(h, "$") != 5 || strings.HasSuffix(h, "=") {
		t.Fatalf("unexpected PHC: %s", h)
	}
	h2, _ := HashPassword(ctx, "correct horse battery")
	if h == h2 {
		t.Fatal("salts must differ")
	}
	tests := []struct {
		pw   string
		want bool
	}{{"correct horse battery", true}, {"wrong", false}, {"", false}}
	for _, tt := range tests {
		ok, err := VerifyPassword(ctx, tt.pw, h)
		if err != nil || ok != tt.want {
			t.Errorf("Verify(%q) = %v, %v", tt.pw, ok, err)
		}
	}
	if ok, err := DummyVerify(ctx, "x"); ok || err != nil {
		t.Errorf("DummyVerify = %v, %v", ok, err)
	}
}

func TestVerifyUsesStoredParams(t *testing.T) {
	salt := []byte("0123456789abcdef")
	// Cheaper params than the defaults: the stored string must drive the work.
	key := argon2.IDKey([]byte("pw"), salt, 1, 8, 1, argonKeyLen)
	enc := encodePHC(8, 1, 1, salt, key)
	ok, err := VerifyPassword(context.Background(), "pw", enc)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword(context.Background(), "other", enc)
	if err != nil || ok {
		t.Fatalf("wrong pw: ok=%v err=%v", ok, err)
	}
}

func TestVerifyInvalidHash(t *testing.T) {
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=8,t=1,p=1$AAAA$AAAA",
		"$argon2id$v=18$m=8,t=1,p=1$AAAA$AAAA", "$argon2id$v=19$m=8,t=1$AAAA$AAAA",
		"$argon2id$v=19$m=8,t=1,p=0$AAAA$AAAA", "$argon2id$v=19$m=8,t=1,p=1$!!$AAAA",
		"$argon2id$v=19$m=8,t=1,p=1$AAAA$", "$argon2id$v=19$m=99999999,t=1,p=1$AAAA$AAAA"} {
		if _, err := VerifyPassword(context.Background(), "x", bad); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func TestHashBusy(t *testing.T) {
	for i := 0; i < maxConcurrentHashes; i++ {
		hashSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < maxConcurrentHashes; i++ {
			<-hashSem
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := HashPassword(ctx, "x"); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if _, err := acquire(context.Background(), 20*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("deadline err = %v, want ErrBusy", err)
	}
}

// RFC 6238 Appendix B, SHA-1 reference secret "12345678901234567890".
var rfcVectors = []struct {
	unix  int64
	code8 string
}{
	{59, "94287082"},
	{1111111109, "07081804"},
	{1111111111, "14050471"},
	{1234567890, "89005924"},
	{2000000000, "69279037"},
	{20000000000, "65353130"},
}

func TestHOTPRFC6238(t *testing.T) {
	key := []byte("12345678901234567890")
	for _, v := range rfcVectors {
		t.Run(fmt.Sprint(v.unix), func(t *testing.T) {
			if got := hotp(key, uint64(v.unix)/30, 8); got != v.code8 {
				t.Fatalf("hotp8 = %s, want %s", got, v.code8)
			}
		})
	}
}

func TestVerifyRFC6238(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	for _, v := range rfcVectors {
		t.Run(fmt.Sprint(v.unix), func(t *testing.T) {
			c, ok := Verify(secret, v.code8[2:], time.Unix(v.unix, 0), 0)
			if !ok || c != uint64(v.unix)/30 {
				t.Fatalf("counter=%d ok=%v", c, ok)
			}
		})
	}
}

func TestVerifyWindowAndReplay(t *testing.T) {
	key := []byte("12345678901234567890")
	secret := b32.EncodeToString(key)
	now := time.Unix(1111111111, 0)
	cur := uint64(now.Unix()) / 30
	code := func(c uint64) string { return hotp(key, c, 6) }
	tests := []struct {
		name string
		code string
		last uint64
		ok   bool
		ctr  uint64
	}{
		{"current", code(cur), 0, true, cur},
		{"previous step", code(cur - 1), 0, true, cur - 1},
		{"next step", code(cur + 1), 0, true, cur + 1},
		{"two steps back", code(cur - 2), 0, false, 0},
		{"two steps ahead", code(cur + 2), 0, false, 0},
		{"replay same", code(cur), cur, false, 0},
		{"older than last", code(cur - 1), cur, false, 0},
		{"newer than last", code(cur + 1), cur, true, cur + 1},
		{"wrong code", "000000", 0, false, 0},
		{"short", code(cur)[:5], 0, false, 0},
		{"long", code(cur) + "0", 0, false, 0},
		{"non-digit", "abcdef", 0, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, ok := Verify(secret, tt.code, now, tt.last)
			if ok != tt.ok || c != tt.ctr {
				t.Fatalf("got %d, %v; want %d, %v", c, ok, tt.ctr, tt.ok)
			}
		})
	}
	if _, ok := Verify("!!!notbase32", "123456", now, 0); ok {
		t.Error("bad secret accepted")
	}
}

func TestSecretAndURI(t *testing.T) {
	s, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := b32.DecodeString(s)
	if err != nil || len(raw) != 20 || strings.Contains(s, "=") {
		t.Fatalf("secret %q: %v", s, err)
	}
	s2, _ := NewSecret()
	if s == s2 {
		t.Fatal("secrets must differ")
	}
	u, err := url.Parse(URI("Ki Agent", "alice@host", s))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme != "otpauth" || u.Host != "totp" || u.Path != "/Ki Agent:alice@host" ||
		q.Get("secret") != s || q.Get("issuer") != "Ki Agent" || q.Get("digits") != "6" || q.Get("period") != "30" {
		t.Fatalf("bad URI: %s", u)
	}
}

func TestTokens(t *testing.T) {
	plain, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	// "ki_" + 43 base64url chars (32 bytes, unpadded).
	if !strings.HasPrefix(plain, "ki_") || len(plain) != 3+43 || strings.ContainsAny(plain, "+/=") {
		t.Fatalf("token %q", plain)
	}
	if len(hash) != 32 || string(hash) != string(HashToken(plain)) {
		t.Fatal("hash mismatch")
	}
	if string(HashToken(plain+"x")) == string(hash) {
		t.Fatal("hash must depend on input")
	}
	id, h, err := NewSessionID()
	if err != nil || strings.HasPrefix(id, "ki_") || len(id) != 43 || string(h) != string(HashSessionID(id)) {
		t.Fatalf("session id %q %v", id, err)
	}
	id2, _, _ := NewSessionID()
	if id == id2 {
		t.Fatal("ids must differ")
	}
}

func TestIPLimiter(t *testing.T) {
	t0 := time.Unix(1000, 0)
	l := NewIPLimiter(3, time.Minute)
	if ok, _ := l.Allow("a", t0); !ok {
		t.Fatal("fresh key blocked")
	}
	l.Fail("a", t0)
	l.Fail("a", t0.Add(10*time.Second))
	if ok, _ := l.Allow("a", t0.Add(11*time.Second)); !ok {
		t.Fatal("blocked below limit")
	}
	l.Fail("a", t0.Add(20*time.Second))
	ok, retry := l.Allow("a", t0.Add(30*time.Second))
	if ok || retry != 30*time.Second {
		t.Fatalf("ok=%v retry=%v, want blocked, 30s", ok, retry)
	}
	if ok, _ := l.Allow("b", t0.Add(30*time.Second)); !ok {
		t.Fatal("other key must be unaffected")
	}
	if ok, _ := l.Allow("a", t0.Add(61*time.Second)); !ok {
		t.Fatal("still blocked after window slid")
	}
	l.Reset("a")
	if ok, _ := l.Allow("a", t0.Add(30*time.Second)); !ok {
		t.Fatal("blocked after reset")
	}
	l.Reset("nope") // no panic
}

func TestIPLimiterEviction(t *testing.T) {
	l := NewIPLimiter(1, time.Hour)
	l.maxKeys = 3
	now := time.Unix(1, 0)
	for _, k := range []string{"a", "b", "c"} {
		l.Fail(k, now)
	}
	l.Fail("a", now) // touch a so b is the oldest
	l.Fail("d", now)
	if len(l.m) != 3 || l.lru.Len() != 3 {
		t.Fatalf("size = %d/%d", len(l.m), l.lru.Len())
	}
	if ok, _ := l.Allow("b", now); !ok {
		t.Error("b should have been evicted")
	}
	for _, k := range []string{"a", "c", "d"} {
		if ok, _ := l.Allow(k, now); ok {
			t.Errorf("%s should still be tracked", k)
		}
	}
}

func TestIPLimiterConcurrent(t *testing.T) {
	l := NewDefaultIPLimiter()
	now := time.Unix(1, 0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				k := fmt.Sprint((i + j) % 5)
				l.Allow(k, now)
				l.Fail(k, now)
				if j%50 == 0 {
					l.Reset(k)
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestNextLock(t *testing.T) {
	now := time.Unix(5000, 0)
	tests := []struct {
		fails int
		want  time.Duration // 0 = not locked
	}{
		{0, 0}, {4, 0}, {5, 15 * time.Minute}, {6, 30 * time.Minute}, {7, time.Hour},
		{8, 2 * time.Hour}, {11, 16 * time.Hour}, {12, 24 * time.Hour}, {13, 24 * time.Hour},
		{1000, 24 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.fails), func(t *testing.T) {
			got := NextLock(tt.fails, now)
			if tt.want == 0 {
				if !got.IsZero() {
					t.Fatalf("got %v, want zero", got)
				}
				return
			}
			if got != now.Add(tt.want) {
				t.Fatalf("got +%v, want +%v", got.Sub(now), tt.want)
			}
		})
	}
}
