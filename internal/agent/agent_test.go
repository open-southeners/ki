package agent

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/open-southeners/ki-agent/api"
	"github.com/open-southeners/ki-agent/internal/rpc"
)

type spec struct {
	OpenRPC string `json:"openrpc"`
	Info    struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	Methods []struct {
		Name         string            `json:"name"`
		Notification bool              `json:"x-notification"`
		Params       *json.RawMessage  `json:"params"`
		Result       *json.RawMessage  `json:"result"`
		Errors       []json.RawMessage `json:"errors"`
	} `json:"methods"`
	Components struct {
		Schemas map[string]json.RawMessage `json:"schemas"`
		Errors  map[string]struct {
			Code int `json:"code"`
		} `json:"errors"`
	} `json:"components"`
}

func loadSpec(t *testing.T) spec {
	t.Helper()
	var s spec
	if err := json.Unmarshal(api.OpenRPC, &s); err != nil {
		t.Fatalf("embedded OpenRPC document is not valid JSON: %v", err)
	}
	return s
}

func TestSpecDocumentsEnvelopeAndErrors(t *testing.T) {
	s := loadSpec(t)

	var env struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
		Desc       string                     `json:"description"`
	}
	if err := json.Unmarshal(s.Components.Schemas["SubscriptionUpdate"], &env); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"subscription", "seq", "data", "gap"} {
		if _, ok := env.Properties[k]; !ok {
			t.Errorf("update envelope missing property %q", k)
		}
	}
	if !slices.Contains(env.Required, "seq") || env.Desc == "" {
		t.Errorf("envelope must require seq and describe gap.to semantics: %+v", env)
	}

	want := map[string]int{
		"ParseError": rpc.CodeParseError, "InvalidRequest": rpc.CodeInvalidRequest,
		"MethodNotFound": rpc.CodeMethodNotFound, "InvalidParams": rpc.CodeInvalidParams,
		"InternalError": rpc.CodeInternalError, "Unauthorized": rpc.CodeUnauthorized,
		"HandshakeRequired": rpc.CodeHandshakeRequired, "IncompatibleVersion": rpc.CodeIncompatibleVersion,
		"SubscriptionNotFound": rpc.CodeSubscriptionNotFound, "Overloaded": rpc.CodeOverloaded,
		"NotFound": rpc.CodeNotFound, "PolicyViolation": rpc.CodePolicyViolation,
		"RuntimeUnavailable": rpc.CodeRuntimeUnavailable, "AlreadyExists": rpc.CodeAlreadyExists,
		"RepositoryError": rpc.CodeRepositoryError, "EnrollmentError": rpc.CodeEnrollmentError,
	}
	for name, code := range want {
		if got, ok := s.Components.Errors[name]; !ok || got.Code != code {
			t.Errorf("error %s: got %+v, want code %d", name, got, code)
		}
	}
}

func TestParseMajor(t *testing.T) {
	tests := []struct {
		in    string
		major int
		ok    bool
	}{
		{"1.0.0", 1, true},
		{"1.4.2-rc.1+build", 1, true},
		{"2.0.0", 2, true},
		{"1.0", 0, false},
		{"", 0, false},
		{"a.b.c", 0, false},
		{"-1.0.0", 0, false},
	}
	for _, tt := range tests {
		if m, ok := parseMajor(tt.in); m != tt.major || ok != tt.ok {
			t.Errorf("parseMajor(%q) = %d,%v want %d,%v", tt.in, m, ok, tt.major, tt.ok)
		}
	}
}

func TestInfoControlPlane(t *testing.T) {
	a := New()
	if got := a.controlPlaneStatus(); !reflect.DeepEqual(got, map[string]string{"state": "disabled"}) {
		t.Fatalf("default control_plane = %v", got)
	}
	a.SetControlPlane(func() any { return map[string]string{"state": "connected"} })
	if got := a.controlPlaneStatus(); !reflect.DeepEqual(got, map[string]string{"state": "connected"}) {
		t.Fatalf("control_plane = %v", got)
	}
}
