package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/ki/internal/containers"
	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/runtime"
	"github.com/open-southeners/ki/internal/runtime/docker"
)

// engineAPI is a minimal raw Docker Engine API client for test setup, so that
// only internal/runtime/docker needs the moby client.
type engineAPI struct {
	base string
	hc   *http.Client
}

func (e *engineAPI) do(t *testing.T, method, path string, body any) []byte {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.base+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := e.hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		t.Fatalf("%s %s: %s: %s", method, path, res.Status, out)
	}
	return out
}

// TestDockerLogsOverUnixSocket serves the container methods backed by a real
// engine (the dind sidecar) and streams a container's logs through the Unix
// socket. Gated on KI_TEST_DOCKER_HOST; run with `docker compose run --rm test`.
func TestDockerLogsOverUnixSocket(t *testing.T) {
	host := os.Getenv("KI_TEST_DOCKER_HOST")
	if host == "" {
		t.Skip("KI_TEST_DOCKER_HOST not set; run via `docker compose run --rm test`")
	}
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "tcp" {
		t.Fatalf("KI_TEST_DOCKER_HOST must be a tcp:// endpoint, got %q", host)
	}
	eng := &engineAPI{base: "http://" + u.Host, hc: &http.Client{Timeout: 3 * time.Minute}}

	eng.do(t, "POST", "/images/create?fromImage=busybox&tag=latest", nil) // body streams progress until done

	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	name := "ki-srv-it-" + hex.EncodeToString(rnd[:])
	var created struct {
		ID string `json:"Id"`
	}
	out := eng.do(t, "POST", "/containers/create?name="+name, map[string]any{
		"Image":  "busybox:latest",
		"Cmd":    []string{"sh", "-c", "echo line-one; echo line-two >&2; sleep 2; echo line-three"},
		"Labels": map[string]string{runtime.LabelManaged: "true", runtime.LabelProject: "srvit", runtime.LabelService: "app"},
	})
	if err := json.Unmarshal(out, &created); err != nil || created.ID == "" {
		t.Fatalf("create: %s, %v", out, err)
	}
	t.Cleanup(func() { eng.do(t, "DELETE", "/containers/"+created.ID+"?force=true", nil) })
	eng.do(t, "POST", "/containers/"+created.ID+"/start", nil)

	rt, err := docker.New(host)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h := startWith(t, func(r *rpc.Router) { containers.New(rt, nil).Register(r) })
	c := dialLogClient(t, h.sock)
	if err := c.call(t, "agent.hello", map[string]any{"protocol": "1.0.0"}, nil); err != nil {
		t.Fatal(err)
	}

	var info runtime.Info
	if err := c.call(t, "runtime.info", nil, &info); err != nil || info.Name != "docker" {
		t.Fatalf("runtime.info = %+v, %v", info, err)
	}
	var list []runtime.Container
	if err := c.call(t, "containers.list", map[string]any{"project": "srvit"}, &list); err != nil || len(list) != 1 || list[0].Name != name {
		t.Fatalf("list = %+v, %v", list, err)
	}

	var sub struct {
		Subscription string `json:"subscription"`
	}
	if err := c.call(t, "containers.logs.subscribe", map[string]any{"id": name, "timestamps": true}, &sub); err != nil {
		t.Fatal(err)
	}

	// line-one, line-two, line-three, then eof once the container exits.
	ups := c.wait(t, 4)
	var texts []string
	for _, up := range ups[:3] {
		var d struct {
			Stream string    `json:"stream"`
			Time   time.Time `json:"time"`
			Text   string    `json:"text"`
		}
		if err := json.Unmarshal(up.Data, &d); err != nil || d.Time.IsZero() {
			t.Fatalf("bad log update %s: %v", up.Data, err)
		}
		texts = append(texts, d.Stream+":"+d.Text)
	}
	if got, want := strings.Join(texts, ","), "stdout:line-one,stderr:line-two,stdout:line-three"; got != want {
		// stdout and stderr are separate pipes, so only the per-stream order is guaranteed.
		for _, w := range strings.Split(want, ",") {
			if !strings.Contains(got, w) {
				t.Fatalf("log lines = %s, want %s", got, want)
			}
		}
	}
	if last := string(ups[3].Data); last != `{"eof":true}` {
		t.Fatalf("last update = %s, want eof", last)
	}

	// Unmanaged or unknown ids look the same to the caller.
	err = c.call(t, "containers.inspect", map[string]any{"id": "ki-no-such-" + name}, nil)
	if re, ok := err.(*rpc.Error); !ok || re.Code != rpc.CodeNotFound {
		t.Fatalf("inspect unknown = %v", err)
	}
}
