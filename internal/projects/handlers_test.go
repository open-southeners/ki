package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/open-southeners/ki/internal/git"
	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/runtime"
	"github.com/open-southeners/ki/internal/store"
)

func TestMapError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantData string
	}{
		{"invalid", invalidf("bad"), rpc.CodeInvalidParams, ""},
		{"git", fmt.Errorf("wrapped: %w", &git.Error{Reason: git.ReasonAuthFailed, Detail: "denied"}),
			rpc.CodeRepositoryError, `{"detail":"denied","reason":"auth_failed"}`},
		{"not found", fmt.Errorf("x: %w", store.ErrNotFound), rpc.CodeNotFound, ""},
		{"exists", store.ErrExists, rpc.CodeAlreadyExists, ""},
		{"domain conflict", fmt.Errorf("x: %w", &store.DomainConflictError{Domain: "a.example.com", Project: "other", Service: "web"}),
			rpc.CodeAlreadyExists, `{"domain":"a.example.com","project":"other"}`},
		{"unavailable", runtime.ErrUnavailable, rpc.CodeRuntimeUnavailable, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var re *rpc.Error
			if !errors.As(mapError(tt.err), &re) || re.Code != tt.wantCode {
				t.Fatalf("mapError = %v, want code %d", mapError(tt.err), tt.wantCode)
			}
			if tt.wantData != "" && string(re.Data) != tt.wantData {
				t.Errorf("data = %s, want %s", re.Data, tt.wantData)
			}
		})
	}
	other := errors.New("boom")
	if mapError(other) != other {
		t.Error("unknown errors must pass through")
	}
	if mapError(nil) != nil {
		t.Error("nil must stay nil")
	}
}

func TestEnvHandlers(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedProject(t, "app")
	h := NewHandlers(e.svc)
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	marshal := func(v any) string { b, _ := json.Marshal(v); return string(b) }

	res, err := h.envSet(ctx, nil, raw(`{"slug":"app","vars":{"B":"secretval","A":"1"}}`))
	if err != nil || marshal(res) != `{"names":["A","B"]}` {
		t.Fatalf("set = %s, %v", marshal(res), err)
	}
	res, err = h.envList(ctx, nil, raw(`{"slug":"app"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out := marshal(res); strings.Contains(out, "secretval") || !strings.Contains(out, `"vars":[{"name":"A"`) {
		t.Errorf("list = %s", out)
	}
	res, err = h.envUnset(ctx, nil, raw(`{"slug":"app","names":["A"]}`))
	if err != nil || marshal(res) != `{"names":["B"]}` {
		t.Fatalf("unset = %s, %v", marshal(res), err)
	}

	var re *rpc.Error
	if _, err := h.envSet(ctx, nil, raw(`{"slug":"app","vars":{"1x":"v"}}`)); !errors.As(err, &re) || re.Code != rpc.CodeInvalidParams {
		t.Errorf("bad name err = %v", err)
	}
	if _, err := h.envList(ctx, nil, raw(`{"slug":"nope"}`)); !errors.As(err, &re) || re.Code != rpc.CodeNotFound {
		t.Errorf("unknown err = %v", err)
	}
	if _, err := h.envList(ctx, nil, raw(`{}`)); !errors.As(err, &re) || re.Code != rpc.CodeInvalidParams {
		t.Errorf("missing slug err = %v", err)
	}
}

func TestProjectViewOmitsSecrets(t *testing.T) {
	p := &store.Project{
		Slug: "a", AuthMethod: AuthHTTPSToken, TokenUser: "u", PublicKey: "pub",
		PrivateKeyEnc: []byte("priv"), TokenEnc: []byte("tok"), WebhookSecretEnc: []byte("sec"), WebhookID: "id",
	}
	b, err := json.Marshal(view(p))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	auth := m["auth"].(map[string]any)
	if _, ok := auth["public_key"]; ok {
		t.Error("public_key must only appear for ssh_key")
	}
	if auth["token_user"] != "u" || m["webhook"].(map[string]any)["path"] != "/hooks/id" {
		t.Errorf("view = %s", b)
	}
	for _, leak := range []string{"priv", "tok\"", "sec"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("view leaks %q: %s", leak, b)
		}
	}
}
