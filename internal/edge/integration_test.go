package edge

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/open-southeners/ki/internal/compose"
	"github.com/open-southeners/ki/internal/config"
	"github.com/open-southeners/ki/internal/edge/certs"
	"github.com/open-southeners/ki/internal/runtime"
	"github.com/open-southeners/ki/internal/runtime/docker"
	"github.com/open-southeners/ki/internal/store"
)

// TestIntegrationEdge runs the real proxy on the test engine. It needs:
//
//   - KI_TEST_DOCKER_HOST: the dind sidecar (never the host daemon).
//   - KI_TEST_SHARED_DIR: a directory mounted at the SAME path in the test
//     container and in the engine. The proxy's bind mounts are resolved by the
//     engine, so the edge data directory must exist there too.
//
// Limitation: the proxy uses host networking, so "127.0.0.1" is the engine's
// loopback, not the test process's. The webhook stub therefore runs as a second
// host-network container inside the engine, and requests reach the proxy at
// the engine's address (host of KI_TEST_DOCKER_HOST).
func TestIntegrationEdge(t *testing.T) {
	host := os.Getenv("KI_TEST_DOCKER_HOST")
	shared := os.Getenv("KI_TEST_SHARED_DIR")
	if host == "" || shared == "" {
		t.Skip("KI_TEST_DOCKER_HOST and KI_TEST_SHARED_DIR not set; run via `docker compose --profile test run --rm test`")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not installed")
	}
	u, err := url.Parse(host)
	if err != nil || u.Hostname() == "" {
		t.Skipf("KI_TEST_DOCKER_HOST %q has no network address", host)
	}
	engineAddr := u.Hostname()

	const (
		httpPort    = 18080
		httpsPort   = 18443
		hooksPort   = 18090
		projectSlug = "edgeit"
		domain      = "app.edgeit.example.com"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	raw, err := client.New(client.WithHost(host))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	pull, err := raw.ImagePull(ctx, "busybox:latest", client.ImagePullOptions{})
	if err != nil {
		t.Fatalf("pull busybox: %v", err)
	}
	if err := pull.Wait(ctx); err != nil {
		t.Fatalf("pull busybox: %v", err)
	}
	run := func(name string, labels map[string]string, hostNet bool, script string) {
		t.Helper()
		hc := &container.HostConfig{}
		if hostNet {
			hc.NetworkMode = "host"
		}
		res, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name:       name,
			Config:     &container.Config{Image: "busybox:latest", Labels: labels, Cmd: []string{"sh", "-c", script}},
			HostConfig: hc,
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() {
			_, _ = raw.ContainerRemove(context.Background(), res.ID, client.ContainerRemoveOptions{Force: true})
		})
		if _, err := raw.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
	}
	run("ki-edgeit-web", map[string]string{
		runtime.LabelManaged: "true", runtime.LabelProject: projectSlug, runtime.LabelService: "web",
		"com.docker.compose.service": "web",
	}, false, "mkdir -p /www && echo backend-ok > /www/index.html && httpd -f -p 8080 -h /www")
	run("ki-edgeit-hooks", map[string]string{runtime.LabelManaged: "true"}, true,
		"mkdir -p /www/hooks && echo hook-ok > /www/hooks/x && httpd -f -p 18090 -h /www")

	rt, err := docker.New(host)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	t.Cleanup(func() {
		list, _ := rt.List(context.Background(), "")
		for _, c := range list {
			if c.Labels[LabelComponent] == ComponentEdge {
				_ = rt.Remove(context.Background(), c.ID, true)
			}
		}
	})

	dir, err := os.MkdirTemp(shared, "edgeit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	m := New(Options{
		Config:  config.Edge{Enabled: true, HTTPPort: httpPort, HTTPSPort: httpsPort, WebhooksPort: hooksPort},
		DataDir: dir,
		Compose: &compose.Runner{DockerHost: host, Home: t.TempDir()},
		Runtime: rt,
		Exposures: staticExposures{{Project: projectSlug, Exposure: store.Exposure{
			Service: "web", Port: 8080, Domains: []string{domain},
		}}},
		interval: 5 * time.Second,
	})
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	waitUntil(t, ctx, "edge running", func() bool {
		s := m.Status()
		if s.State == StateError {
			t.Fatalf("edge error: %s", s.LastError)
		}
		return s.State == StateRunning
	})

	self, err := certs.LoadOrCreateSelfSigned(dir + "/edge")
	if err != nil {
		t.Fatal(err)
	}
	httpc := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(engineAddr, "18443"))
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed by design
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	get := func(host, path string) (string, string, int) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+path, nil)
		resp, err := httpc.Do(req)
		if err != nil {
			return "", "", 0
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		fp := ""
		if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
			fp = certs.Fingerprint(resp.TLS.PeerCertificates[0].Raw)
		}
		return strings.TrimSpace(string(body)), fp, resp.StatusCode
	}

	// Project domain reaches the backend; an unknown host gets the
	// self-signed default certificate.
	waitUntil(t, ctx, "project route", func() bool {
		body, _, code := get(domain, "/")
		return code == 200 && body == "backend-ok"
	})
	if _, fp, _ := get("192.0.2.1", "/"); fp != self.FingerprintSHA256 {
		t.Errorf("default certificate fingerprint = %q, want %q", fp, self.FingerprintSHA256)
	}
	// /hooks/ on the bare IP / unknown host reaches the webhook stub.
	waitUntil(t, ctx, "hooks route", func() bool {
		body, _, code := get("192.0.2.1", "/hooks/x")
		return code == 200 && body == "hook-ok"
	})
	// ... but not on a project domain, where it belongs to the app.
	if body, _, code := get(domain, "/hooks/x"); code == 200 && body == "hook-ok" {
		t.Error("/hooks/ on a project domain reached the webhook listener")
	}
	// Plain HTTP redirects to HTTPS.
	plain := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := plain.Get("http://" + net.JoinHostPort(engineAddr, "18080") + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently && resp.StatusCode != http.StatusPermanentRedirect {
		t.Errorf("http status = %d, want a redirect", resp.StatusCode)
	}
}

type staticExposures []store.ProjectExposure

func (s staticExposures) AllExposures(context.Context) ([]store.ProjectExposure, error) {
	return s, nil
}

func waitUntil(t *testing.T, ctx context.Context, what string, cond func() bool) {
	t.Helper()
	for ctx.Err() == nil {
		if cond() {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out waiting for %s", what)
}
