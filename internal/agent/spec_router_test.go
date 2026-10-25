package agent_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/open-southeners/ki-agent/api"
	"github.com/open-southeners/ki-agent/internal/agent"
	"github.com/open-southeners/ki-agent/internal/containers"
	"github.com/open-southeners/ki-agent/internal/deploys"
	"github.com/open-southeners/ki-agent/internal/edge"
	"github.com/open-southeners/ki-agent/internal/enrollment"
	"github.com/open-southeners/ki-agent/internal/projects"
	"github.com/open-southeners/ki-agent/internal/rpc"
)

// This test lives in an external package because the enrollment handlers
// depend (through the control-plane manager) on package agent.
func TestSpecMatchesRouter(t *testing.T) {
	var s struct {
		OpenRPC string `json:"openrpc"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Methods []struct {
			Name         string           `json:"name"`
			Notification bool             `json:"x-notification"`
			LocalOnly    bool             `json:"x-local-only"`
			Transports   []string         `json:"x-transports"`
			Params       *json.RawMessage `json:"params"`
			Result       *json.RawMessage `json:"result"`
		} `json:"methods"`
	}
	if err := json.Unmarshal(api.OpenRPC, &s); err != nil {
		t.Fatalf("embedded OpenRPC document is not valid JSON: %v", err)
	}
	if s.OpenRPC == "" || s.Info.Title == "" {
		t.Fatalf("spec missing openrpc/info.title: %+v", s)
	}
	if s.Info.Version != api.ProtocolVersion {
		t.Fatalf("info.version = %q, ProtocolVersion = %q", s.Info.Version, api.ProtocolVersion)
	}

	var names, local, restricted []string
	notifications := 0
	for _, m := range s.Methods {
		if m.Params == nil {
			t.Errorf("%s: missing params", m.Name)
		}
		if m.LocalOnly {
			local = append(local, m.Name)
		}
		if len(m.Transports) > 0 {
			restricted = append(restricted, m.Name)
			if !slices.Equal(m.Transports, []string{rpc.TransportUnix, rpc.TransportControlPlane}) {
				t.Errorf("%s: x-transports = %v", m.Name, m.Transports)
			}
		}
		if m.Notification {
			notifications++
			if m.Result != nil {
				t.Errorf("%s: notification must not define a result", m.Name)
			}
			continue
		}
		if m.Result == nil {
			t.Errorf("%s: missing result", m.Name)
		}
		names = append(names, m.Name)
	}
	if notifications != 3 {
		t.Errorf("notifications documented = %d, want 3", notifications)
	}
	slices.Sort(names)
	slices.Sort(local)
	slices.Sort(restricted)

	r := rpc.NewRouter()
	agent.New().Register(r)
	containers.New(nil, nil).Register(r)
	projects.NewHandlers(nil).Register(r)
	deploys.NewHandlers(nil).Register(r)
	edge.NewHandlers(nil, "", nil).Register(r)
	enrollment.New(enrollment.Options{}).Register(r)
	if got := r.Methods(); !slices.Equal(got, names) {
		t.Fatalf("router methods %v != spec methods %v", got, names)
	}
	want := []string{enrollment.MethodRemove, enrollment.MethodStart, enrollment.MethodWait}
	if !slices.Equal(local, want) {
		t.Errorf("x-local-only methods = %v, want %v", local, want)
	}
	wantRestricted := []string{edge.MethodCSR, edge.MethodInstall, edge.MethodRemove}
	slices.Sort(wantRestricted)
	if !slices.Equal(restricted, wantRestricted) {
		t.Errorf("x-transports methods = %v, want %v", restricted, wantRestricted)
	}
}
