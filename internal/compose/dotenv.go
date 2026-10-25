package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// WriteDotenv atomically writes vars to path as a dotenv file with mode 0600,
// one NAME=value line per variable in sorted order. Values are quoted so that
// Compose reads them back verbatim: single quotes when the value has no
// single quote or backslash (nothing, `$` included, is interpolated), and
// double quotes with escapes otherwise. Names must match
// [A-Za-z_][A-Za-z0-9_]* and values must not contain NUL.
func WriteDotenv(path string, vars map[string]string) error {
	names := make([]string, 0, len(vars))
	for n, v := range vars {
		if !envName.MatchString(n) {
			return fmt.Errorf("compose: invalid variable name %q", n)
		}
		if strings.ContainsRune(v, 0) {
			return fmt.Errorf("compose: variable %s: value contains NUL", n)
		}
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte('=')
		b.WriteString(quoteDotenv(vars[n]))
		b.WriteByte('\n')
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".env.tmp-*") // created 0600
	if err != nil {
		return fmt.Errorf("compose: write dotenv: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("compose: write dotenv: %w", err)
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("compose: write dotenv: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("compose: write dotenv: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("compose: write dotenv: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("compose: write dotenv: %w", err)
	}
	return nil
}

// quoteDotenv renders v as a dotenv value.
func quoteDotenv(v string) string {
	if !strings.ContainsAny(v, "'\\") {
		return "'" + v + "'"
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '$':
			b.WriteString(`\$`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
