package deploys

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/compose"
	"github.com/open-southeners/ki-agent/internal/runtime/docker"
)

const healthyCompose = `services:
  web:
    image: alpine:3
    command: ["sleep", "300"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 2s
      retries: 3
`

// TestIntegrationRealDeploy runs a whole deploy of a fixture repository on the
// test engine (KI_TEST_DOCKER_HOST, the dind sidecar; never the host daemon).
func TestIntegrationRealDeploy(t *testing.T) {
	host := os.Getenv("KI_TEST_DOCKER_HOST")
	if host == "" {
		t.Skip("KI_TEST_DOCKER_HOST not set; run via `docker compose --profile test run --rm test`")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not installed")
	}
	rt, err := docker.New(host)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, fixtureOpts{
		files: map[string]string{"compose.yaml": healthyCompose},
		tweak: func(o *Options) {
			o.Compose = &compose.Runner{DockerHost: host, Home: t.TempDir()}
			o.Runtime = rt
			o.PollInterval = time.Second
			o.HealthTimeout = 2 * time.Minute
		},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = rt.PruneProject(ctx, "app", true)
	})
	if _, err := f.svc.SetEnv(context.Background(), "app", map[string]string{"GREETING": "hello"}); err != nil {
		t.Fatal(err)
	}

	d := f.trigger(t, "")
	var got Deploy
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		x, err := f.mgr.Get(context.Background(), d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if terminal(x.Status) {
			got = *x
			break
		}
		time.Sleep(time.Second)
	}
	if got.Status != StatusSucceeded {
		logs, _ := f.st.DeployLogs(context.Background(), d.ID, 0)
		var b strings.Builder
		for _, l := range logs {
			b.WriteString(l.Stream + ": " + l.Text + "\n")
		}
		t.Fatalf("deploy = %+v (error %+v)\n%s", got, got.Error, b.String())
	}
	cs, err := rt.List(context.Background(), "app")
	if err != nil || len(cs) != 1 || cs[0].Service != "web" || cs[0].State != "running" {
		t.Fatalf("containers = %+v, %v", cs, err)
	}
	if p, _ := f.svc.Get(context.Background(), "app"); p.Commit != got.Commit {
		t.Errorf("project commit = %s, want %s", p.Commit, got.Commit)
	}
}
