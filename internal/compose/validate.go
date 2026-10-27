package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-southeners/ki-agent/internal/policy"
)

// MaxComposeSize is the largest compose file a compose project may store.
const MaxComposeSize = 256 << 10

// ValidationError is a compose file that cannot be stored: Reason says why in
// one line and Violations lists the policy breaches (same shape as the ones
// of a failed deploy), if that is the cause.
type ValidationError struct {
	Reason     string
	Violations []policy.Violation
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString(e.Reason)
	for _, v := range e.Violations {
		fmt.Fprintf(&b, "; %s %s: %s", v.Service, v.Rule, v.Detail)
	}
	return b.String()
}

// Validate checks the content of a compose project's compose file before it
// is stored. It writes the content to compose.yaml in a fresh temporary
// project directory below parent (with an empty .env, the only file a
// project directory has besides compose.yaml, so `env_file: .env` is fine),
// then runs [CheckIncludes], `compose config`, [policy.ParseCompose] and
// [policy.Check] with NoBuild forced on: anything that would need files that
// do not exist next to the compose file (include, extends, other env_file
// paths) or a build is rejected.
//
// A rejected file yields (*ValidationError, nil); a failure of the machinery
// itself (temp dir, I/O) yields a non-nil error.
func Validate(ctx context.Context, r *Runner, parent, content, project string, opts policy.Options) (*ValidationError, error) {
	if strings.TrimSpace(content) == "" {
		return &ValidationError{Reason: "compose content is empty"}, nil
	}
	if len(content) > MaxComposeSize {
		return &ValidationError{Reason: fmt.Sprintf("compose content exceeds %d KiB", MaxComposeSize>>10)}, nil
	}
	if strings.ContainsRune(content, 0) {
		return &ValidationError{Reason: "compose content contains a NUL byte"}, nil
	}
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return nil, fmt.Errorf("compose: validate: %w", err)
	}
	dir, err := os.MkdirTemp(parent, "validate-*")
	if err != nil {
		return nil, fmt.Errorf("compose: validate: %w", err)
	}
	defer os.RemoveAll(dir)
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return nil, fmt.Errorf("compose: validate: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(content), 0o640); err != nil {
		return nil, fmt.Errorf("compose: validate: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), nil, 0o640); err != nil {
		return nil, fmt.Errorf("compose: validate: %w", err)
	}

	invalid := func(format string, args ...any) *ValidationError {
		// Keep the throw-away directory name out of the message.
		return &ValidationError{Reason: strings.ReplaceAll(fmt.Sprintf(format, args...), dir, ".")}
	}
	if err := CheckIncludes(dir, "compose.yaml"); err != nil {
		var ie *IncludeError
		if errors.As(err, &ie) {
			return invalid("%v", ie), nil
		}
		return nil, err
	}
	model, err := r.Config(ctx, dir, "compose.yaml", project)
	if err != nil {
		var ce *Error
		if errors.As(err, &ce) && ctx.Err() == nil {
			if s := strings.TrimSpace(ce.Stderr); s != "" {
				return invalid("%s", s), nil
			}
			return invalid("%v", ce), nil
		}
		return nil, err
	}
	proj, err := policy.ParseCompose(model)
	if err != nil {
		return invalid("%v", err), nil
	}
	opts.NoBuild = true
	if vs := policy.Check(dir, proj, opts); len(vs) > 0 {
		return &ValidationError{Reason: fmt.Sprintf("%d policy violation(s)", len(vs)), Violations: vs}, nil
	}
	return nil, nil
}
