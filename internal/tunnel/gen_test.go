package tunnel

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(got) {
		t.Errorf("%s differs (run with -update):\n%s", name, got)
	}
}

func TestComposeGolden(t *testing.T) {
	b, err := Compose(ComposeOptions{
		Image: DefaultImage, SecretDir: "/var/lib/ki-agent/tunnel/secret", MetricsPort: 20241,
		Generation: "0123456789abcdef", UID: 996, GID: 995,
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "compose.json", b)
	s := string(b)
	for _, bad := range []string{"TUNNEL_TOKEN", "environment", "env_file", "--token ", "cap_add"} {
		if strings.Contains(s, bad) {
			t.Errorf("compose file contains %q", bad)
		}
	}
}

func TestComposeInvalid(t *testing.T) {
	ok := ComposeOptions{Image: DefaultImage, SecretDir: "/x", MetricsPort: 1}
	for name, mut := range map[string]func(*ComposeOptions){
		"no image":       func(o *ComposeOptions) { o.Image = "" },
		"relative dir":   func(o *ComposeOptions) { o.SecretDir = "x" },
		"bad port":       func(o *ComposeOptions) { o.MetricsPort = 0 },
		"negative uid":   func(o *ComposeOptions) { o.UID = -1 },
		"newline in dir": func(o *ComposeOptions) { o.SecretDir = "/x\ny" },
	} {
		o := ok
		mut(&o)
		if _, err := Compose(o); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := Compose(ok); err != nil {
		t.Fatal(err)
	}
}

func TestCleanToken(t *testing.T) {
	good := "eyJhIjoiYWJjIiwidCI6Ii0tXy8rPT0ifQ=="
	if got, err := CleanToken("  " + good + "\n"); err != nil || got != good {
		t.Fatalf("CleanToken = %q, %v", got, err)
	}
	for _, bad := range []string{"", "  ", "has space", "semi;colon", "quote\"d", strings.Repeat("a", 4097), "new\nline"} {
		_, err := CleanToken(bad)
		if err == nil {
			t.Errorf("CleanToken(%.20q) accepted", bad)
		} else if bad != "" && strings.TrimSpace(bad) != "" && strings.Contains(err.Error(), strings.TrimSpace(bad)) {
			t.Errorf("error echoes the token: %v", err)
		}
	}
}
