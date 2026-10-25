package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/policy"
)

// noDaemon makes any accidental engine access fail: `compose config` never
// needs a daemon, and these tests must never reach the host's.
const noDaemon = "unix:///nonexistent/docker.sock"

// configRunner returns a daemon-free Runner, skipping when the docker CLI or
// its compose plugin is missing.
func configRunner(t *testing.T) *Runner {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not installed")
	}
	cmd := exec.Command("docker", "compose", "version")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "DOCKER_HOST=" + noDaemon, "HOME=" + t.TempDir()}
	if err := cmd.Run(); err != nil {
		t.Skipf("docker compose plugin not available: %v", err)
	}
	return &Runner{DockerHost: noDaemon, Home: t.TempDir()}
}

// engineRunner returns a Runner on the test engine (KI_TEST_DOCKER_HOST, the
// dind sidecar; never the host daemon).
func engineRunner(t *testing.T) *Runner {
	t.Helper()
	host := os.Getenv("KI_TEST_DOCKER_HOST")
	if host == "" {
		t.Skip("KI_TEST_DOCKER_HOST not set; run via `docker compose --profile test run --rm test`")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not installed")
	}
	return &Runner{DockerHost: host, Home: t.TempDir()}
}

func TestConfigEnvFileAndInclude(t *testing.T) {
	r := configRunner(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "compose.yaml"), `name: ignored
include:
  - sub/inc.yaml
services:
  a:
    image: x
    env_file:
      - path: ./a.env
        required: false
      - /etc/passwd
    labels: ["x=1", "y"]
    extends:
      file: sub/ext.yaml
      service: base
`)
	write(t, filepath.Join(root, "sub/inc.yaml"), "services: {inc: {image: y}}\n")
	write(t, filepath.Join(root, "sub/ext.yaml"), "services: {base: {image: z, environment: {A: b}}}\n")

	// The pre-scan already refuses the /etc/passwd env_file; Config and the
	// policy are the second line of defence for Compose versions that keep
	// the path (v5.1.0) in the model.
	var ie *IncludeError
	if err := CheckIncludes(root, "compose.yaml"); !errors.As(err, &ie) {
		t.Fatalf("CheckIncludes err = %v, want *IncludeError", err)
	}
	out, err := r.Config(context.Background(), root, "compose.yaml", "slug")
	if err != nil {
		t.Fatal(err)
	}
	var model struct {
		Name     string `json:"name"`
		Services map[string]struct {
			EnvFile []map[string]any  `json:"env_file"`
			Labels  map[string]string `json:"labels"`
			Env     map[string]string `json:"environment"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &model); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if model.Name != "slug" {
		t.Errorf("name = %q, want the -p project", model.Name)
	}
	a := model.Services["a"]
	want := []map[string]any{
		{"path": filepath.Join(root, "a.env"), "required": false},
		{"path": "/etc/passwd"},
	}
	if !reflect.DeepEqual(a.EnvFile, want) {
		t.Errorf("env_file = %v, want %v", a.EnvFile, want)
	}
	if a.Env["A"] != "b" || a.Labels["x"] != "1" {
		t.Errorf("service a = %+v", a)
	}
	if _, ok := model.Services["inc"]; !ok {
		t.Error("included service missing")
	}

	// The unresolved /etc/passwd stays in the model, so policy rejects it.
	p, err := policy.ParseCompose(out)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, v := range policy.Check(root, p, policy.Options{}) {
		if v.Rule == policy.RuleEnvFile && v.Service == "a" && strings.Contains(v.Detail, "/etc/passwd") {
			found = true
		}
	}
	if !found {
		t.Errorf("env_file /etc/passwd not reported: %+v", policy.Check(root, p, policy.Options{}))
	}
}

func TestConfigIgnoresAgentEnvironment(t *testing.T) {
	r := configRunner(t)
	t.Setenv("KI_TEST_SECRET", "s3cret")
	t.Setenv("COMPOSE_PROJECT_NAME", "hijacked")
	root := t.TempDir()
	write(t, filepath.Join(root, "compose.yaml"), "services:\n  a:\n    image: ${KI_TEST_SECRET:-unset}\n")
	out, err := r.Config(context.Background(), root, "compose.yaml", "slug")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "s3cret") || strings.Contains(string(out), "hijacked") || !strings.Contains(string(out), `"image": "unset"`) {
		t.Fatalf("agent environment reached compose:\n%s", out)
	}
}

func TestConfigError(t *testing.T) {
	r := configRunner(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "compose.yaml"), "services: [\n")
	_, err := r.Config(context.Background(), root, "compose.yaml", "slug")
	if err == nil {
		t.Fatal("want error")
	}
	if _, ok := err.(*Error); !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
}

func TestLabelRoundTripThroughConfig(t *testing.T) {
	r := configRunner(t)
	root, _ := filepath.EvalSymlinks(t.TempDir())
	write(t, filepath.Join(root, "compose.yaml"), `services:
  web:
    image: nginx
    labels: ["traefik.enable=true"]
    ports: ["8080:80"]
    command: ["echo", "$$HOME", "a b"]
    environment: {PRICE: "$$5"}
  db:
    image: postgres
    volumes: [data:/var/lib/postgresql/data]
networks:
  front: {}
volumes:
  data: {}
`)
	model, err := r.Config(context.Background(), root, "compose.yaml", "app")
	if err != nil {
		t.Fatal(err)
	}
	labelled, err := Label(model, "app")
	if err != nil {
		t.Fatal(err)
	}
	// Run the labelled model through compose again, as Build and Up will.
	deployDir := t.TempDir()
	file := filepath.Join(deployDir, "compose.json")
	if err := os.WriteFile(file, labelled, 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := r.Config(context.Background(), deployDir, file, "app")
	if err != nil {
		t.Fatalf("labelled model rejected by compose: %v\n%s", err, labelled)
	}
	var got, orig struct {
		Services map[string]struct {
			Labels      map[string]string `json:"labels"`
			Command     []string          `json:"command"`
			Environment map[string]string `json:"environment"`
			Ports       []map[string]any  `json:"ports"`
		} `json:"services"`
		Networks map[string]struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"networks"`
		Volumes map[string]struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"volumes"`
	}
	if err := json.Unmarshal(again, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(model, &orig); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"web", "db"} {
		l := got.Services[n].Labels
		if l["ki.managed"] != "true" || l["ki.project"] != "app" || l["ki.service"] != n {
			t.Errorf("service %s labels = %v", n, l)
		}
	}
	if got.Services["web"].Labels["traefik.enable"] != "true" {
		t.Errorf("user label lost: %v", got.Services["web"].Labels)
	}
	if !reflect.DeepEqual(got.Services["web"].Command, orig.Services["web"].Command) ||
		!reflect.DeepEqual(got.Services["web"].Environment, orig.Services["web"].Environment) ||
		!reflect.DeepEqual(got.Services["web"].Ports, orig.Services["web"].Ports) {
		t.Errorf("service fields changed:\n got %+v\nwant %+v", got.Services["web"], orig.Services["web"])
	}
	for n, net := range got.Networks {
		if net.Labels["ki.managed"] != "true" || net.Labels["ki.project"] != "app" || net.Name != orig.Networks[n].Name {
			t.Errorf("network %s = %+v", n, net)
		}
	}
	if len(got.Networks) != len(orig.Networks) || len(got.Volumes) != 1 {
		t.Errorf("networks/volumes = %d/%d", len(got.Networks), len(got.Volumes))
	}
	if v := got.Volumes["data"]; v.Labels["ki.managed"] != "true" || v.Name != orig.Volumes["data"].Name {
		t.Errorf("volume = %+v", v)
	}
}

// TestDotenvRoundTrip writes awkward values with WriteDotenv and reads them
// back through the real `docker compose config` (env_file, no daemon).
func TestDotenvRoundTrip(t *testing.T) {
	r := configRunner(t)
	vars := map[string]string{
		"PLAIN":       "value",
		"EMPTY":       "",
		"SPACES":      "two words  and   more",
		"LEADING":     "  padded  ",
		"SINGLE":      "it's",
		"DOUBLE":      `say "hi"`,
		"BOTH":        `it's "both"`,
		"DOLLAR":      "$HOME ${PATH} $$ $",
		"DOLLAR_QUOT": `it's $HOME ${PATH} \$x`,
		"HASH":        "a # not a comment",
		"HASH_QUOT":   "it's # not a comment",
		"BACKSLASH":   `C:\dir\n \\ \`,
		"BACK_QUOT":   `it's C:\dir\n \\ \`,
		"EQUALS":      "a=b=c",
		"NEWLINE":     "line1\nline2\n",
		"NEWLINE_Q":   "it's\nmulti\n\"line\"\n",
		"CR":          "a\rb",
		"CRLF_Q":      "it's\r\nx",
		"TAB":         "a\tb",
		"UNICODE":     "héllo wörld ✓",
		"TRAILING_NL": "x\n",
		"SEMICOLON":   "a;b&c|d`e`",
		"BRACES":      "{{ .X }} ${} $(cmd)",
		"LONG":        strings.Repeat("ab'c\n", 2000),
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "compose.yaml"), "services:\n  a:\n    image: x\n    env_file: [.env]\n")
	readBack := func(vars map[string]string) (map[string]string, error) {
		if err := WriteDotenv(filepath.Join(root, ".env"), vars); err != nil {
			return nil, err
		}
		// Plain `config` resolves env_file into environment; the agent's
		// no-env-resolution mode would leave it unread, so call the CLI directly.
		out, err := r.rawConfig(t, root)
		if err != nil {
			return nil, fmt.Errorf("%w\n%s", err, out)
		}
		var model struct {
			Services map[string]struct {
				Environment map[string]string `json:"environment"`
			} `json:"services"`
		}
		if err := json.Unmarshal(out, &model); err != nil {
			return nil, err
		}
		env := model.Services["a"].Environment
		for k, v := range env {
			// config prints every literal $ as $$ so the model survives
			// another interpolation round.
			env[k] = strings.ReplaceAll(v, "$$", "$")
		}
		return env, nil
	}
	for _, n := range sortedNames(vars) {
		t.Run(n, func(t *testing.T) {
			got, err := readBack(map[string]string{n: vars[n]})
			if err != nil {
				t.Fatal(err)
			}
			if g, ok := got[n]; !ok || g != vars[n] {
				t.Errorf("got %q (present=%v), want %q", g, ok, vars[n])
			}
		})
	}
	t.Run("all together", func(t *testing.T) {
		got, err := readBack(vars)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, vars) {
			for n, want := range vars {
				if got[n] != want {
					t.Errorf("%s: got %q, want %q", n, got[n], want)
				}
			}
			t.Errorf("got %d variables, want %d", len(got), len(vars))
		}
	})
}

func sortedNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// rawConfig runs a plain `compose config --format json` (env_file resolved)
// in dir with the same minimal environment.
func (r *Runner) rawConfig(t *testing.T, dir string) ([]byte, error) {
	t.Helper()
	cmd := r.command(context.Background(), dir, "-f", "compose.yaml", "-p", "t", "config", "--format", "json")
	return cmd.CombinedOutput()
}

func TestBuildUpHealthy(t *testing.T) {
	r := engineRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	root, _ := filepath.EvalSymlinks(t.TempDir())
	slug := "kicompose" + time.Now().Format("150405")
	write(t, filepath.Join(root, "Dockerfile"), "FROM alpine:3.20\nRUN echo built > /built\n")
	write(t, filepath.Join(root, "compose.yaml"), `services:
  app:
    build: .
    command: ["sh", "-c", "touch /tmp/ok; sleep 600"]
    environment:
      FROM_DOTENV: ${FROM_DOTENV:-missing}
    healthcheck:
      test: ["CMD", "test", "-f", "/tmp/ok"]
      interval: 1s
      retries: 30
`)
	if err := WriteDotenv(filepath.Join(root, ".env"), map[string]string{"FROM_DOTENV": "it's here"}); err != nil {
		t.Fatal(err)
	}
	if err := CheckIncludes(root, "compose.yaml"); err != nil {
		t.Fatal(err)
	}
	model, err := r.Config(ctx, root, "compose.yaml", slug)
	if err != nil {
		t.Fatal(err)
	}
	p, err := policy.ParseCompose(model)
	if err != nil {
		t.Fatal(err)
	}
	if vs := policy.Check(root, p, policy.Options{}); len(vs) != 0 {
		t.Fatalf("violations: %+v", vs)
	}
	labelled, err := Label(model, slug)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "compose.json")
	if err := os.WriteFile(file, labelled, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd := r.command(context.Background(), filepath.Dir(file), "-f", file, "-p", slug, "down", "--volumes", "--remove-orphans", "--rmi", "local")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("cleanup: %v\n%s", err, out)
		}
	})

	var lines []string
	collect := func(stream, line string) { lines = append(lines, stream+": "+line) }
	if err := r.Build(ctx, file, slug, collect); err != nil {
		t.Fatalf("build: %v\n%s", err, strings.Join(lines, "\n"))
	}
	if len(lines) == 0 {
		t.Error("build produced no output")
	}
	lines = nil
	if err := r.Up(ctx, file, slug, collect); err != nil {
		t.Fatalf("up: %v\n%s", err, strings.Join(lines, "\n"))
	}

	// The container carries the labels and the dotenv value verbatim, and
	// reaches healthy.
	inspect := func(format string) string {
		t.Helper()
		ids := r.command(ctx, root, "-f", file, "-p", slug, "ps", "-q")
		idOut, err := ids.Output()
		if err != nil {
			t.Fatal(err)
		}
		id := strings.TrimSpace(string(idOut))
		if id == "" {
			t.Fatal("no container")
		}
		cmd := exec.CommandContext(ctx, "docker", "inspect", "--format", format, id)
		cmd.Env = r.env()
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	if got := inspect(`{{index .Config.Labels "ki.managed"}}/{{index .Config.Labels "ki.project"}}/{{index .Config.Labels "ki.service"}}`); got != "true/"+slug+"/app" {
		t.Errorf("labels = %q", got)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		status := inspect(`{{.State.Health.Status}}`)
		if status == "healthy" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("health = %q", status)
		}
		time.Sleep(time.Second)
	}
	if got := inspect(`{{range .Config.Env}}{{println .}}{{end}}`); !strings.Contains(got, "FROM_DOTENV=it's here") {
		t.Errorf("env = %q", got)
	}
}

func TestBuildFailureReported(t *testing.T) {
	r := engineRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	write(t, filepath.Join(root, "Dockerfile"), "FROM alpine:3.20\nRUN exit 7\n")
	write(t, filepath.Join(root, "compose.yaml"), "services:\n  app:\n    build: .\n")
	model, err := r.Config(ctx, root, "compose.yaml", "kicomposefail")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "compose.json")
	if err := os.WriteFile(file, model, 0o600); err != nil {
		t.Fatal(err)
	}
	err = r.Build(ctx, file, "kicomposefail", nil)
	ce, ok := err.(*Error)
	if !ok || ce.Op != "build" {
		t.Fatalf("err = %v, want *Error for build", err)
	}
}
