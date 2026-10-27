package compose

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/open-southeners/ki/internal/policy"
)

const okModel = `{"name":"p","services":{"web":{"image":"nginx"}}}`

const buildModel = `{"name":"p","services":{"web":{"image":"x","build":{"context":"/nowhere","dockerfile":"Dockerfile"}}}}`

const privModel = `{"name":"p","services":{"web":{"image":"x","privileged":true}}}`

// modelCLI is a docker stand-in that prints model for `compose config`.
func modelCLI(t *testing.T, model string) *Runner {
	t.Helper()
	return &Runner{CLI: fakeCLI(t, "cat <<'EOF'\n"+model+"\nEOF")}
}

func TestValidate(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	tests := []struct {
		name     string
		model    string
		content  string
		wantRule string // expected first violation; "" with wantText "" means ok
		wantText string // expected substring of the reason
	}{
		{"ok", okModel, "services:\n  web:\n    image: nginx\n", "", ""},
		{"build rejected", buildModel, "services:\n  web:\n    image: x\n    build: .\n", policy.RuleBuildNotAllowed, ""},
		{"policy violation", privModel, "services:\n  web:\n    image: x\n    privileged: true\n", policy.RulePrivileged, ""},
		{"include escape", okModel, "include:\n  - ../other.yaml\nservices: {}\n", "", "other.yaml"},
		{"empty", okModel, "  \n", "", "empty"},
		{"too big", okModel, strings.Repeat("#", MaxComposeSize+1), "", "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ve, err := Validate(ctx, modelCLI(t, tt.model), parent, tt.content, "p", policy.Options{})
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tt.wantRule == "" && tt.wantText == "":
				if ve != nil {
					t.Fatalf("unexpected rejection: %v", ve)
				}
			case ve == nil:
				t.Fatal("expected a rejection")
			case tt.wantRule != "":
				if len(ve.Violations) == 0 || ve.Violations[0].Rule != tt.wantRule {
					t.Fatalf("violations = %+v, want rule %s", ve.Violations, tt.wantRule)
				}
			default:
				if !strings.Contains(ve.Reason, tt.wantText) {
					t.Fatalf("reason = %q, want %q", ve.Reason, tt.wantText)
				}
			}
		})
	}
	if left, _ := os.ReadDir(parent); len(left) != 0 {
		t.Errorf("temp dirs left behind: %v", left)
	}
}

func TestValidateComposeError(t *testing.T) {
	r := &Runner{CLI: fakeCLI(t, "echo 'services.web.image must be a string' >&2; exit 1")}
	ve, err := Validate(context.Background(), r, t.TempDir(), "services: {}\n", "p", policy.Options{})
	if err != nil || ve == nil || !strings.Contains(ve.Reason, "must be a string") {
		t.Fatalf("ve = %v, err = %v", ve, err)
	}
}
