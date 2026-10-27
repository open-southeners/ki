package docker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/ki/internal/runtime"
)

var versionPrefix = regexp.MustCompile(`^/v[0-9.]+`)

// fakeEngine serves canned Docker Engine API responses on a Unix socket.
type fakeEngine struct {
	mu       sync.Mutex
	requests []string
	labels   map[string]map[string]string // id -> labels
	tty      bool
	logBody  []byte
	stats    []byte // body of GET /containers/{id}/stats
	stop     func() // stops the fake API server
}

func newFake(t *testing.T) (*fakeEngine, *Docker) {
	t.Helper()
	f := &fakeEngine{labels: map[string]map[string]string{
		"managed":   {runtime.LabelManaged: "true", runtime.LabelProject: "shop", runtime.LabelService: "web"},
		"unmanaged": {"foo": "bar"},
	}}
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	srv.Listener = l
	srv.Start()
	f.stop = srv.Close
	t.Cleanup(srv.Close)
	d, err := New("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return f, d
}

func (f *fakeEngine) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+versionPrefix.ReplaceAllString(r.URL.Path, "")+"?"+r.URL.RawQuery)
}

func (f *fakeEngine) has(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func (f *fakeEngine) serve(w http.ResponseWriter, r *http.Request) {
	path := versionPrefix.ReplaceAllString(r.URL.Path, "")
	if path != "/_ping" {
		f.record(r)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Api-Version", "1.47")
	switch {
	case path == "/_ping":
		w.Write([]byte("OK"))
	case path == "/version":
		w.Write([]byte(`{"Version":"28.0.1","ApiVersion":"1.48","Os":"linux","Arch":"amd64"}`))
	case path == "/containers/json":
		w.Write([]byte(`[
{"Id":"aaa","Names":["/shop-web-1"],"Image":"nginx","Created":1700000000,"State":"running","Status":"Up 1 minute",
 "Labels":{"ki.managed":"true","ki.project":"shop","ki.service":"web"},
 "Ports":[{"IP":"0.0.0.0","PrivatePort":80,"PublicPort":8080,"Type":"tcp"},{"PrivatePort":443,"Type":"tcp"}]},
{"Id":"bbb","Names":["/bare"],"Image":"busybox","Created":1700000001,"State":"exited","Status":"Exited (0)"}
]`))
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
		lb, ok := f.labels[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"message": "No such container: " + id})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"Id": id, "Name": "/shop-web-1", "Created": "2024-01-02T03:04:05.123456789Z",
			"RestartCount": 2,
			"State": map[string]any{
				"Status": "exited", "ExitCode": 3,
				"StartedAt": "2024-01-02T03:04:06Z", "FinishedAt": "0001-01-01T00:00:00Z",
				"Health": map[string]any{"Status": "healthy"},
			},
			"Config":          map[string]any{"Image": "nginx", "Labels": lb, "Tty": f.tty},
			"Mounts":          []map[string]any{{"Type": "bind", "Source": "/srv/data", "Destination": "/data", "RW": false}},
			"NetworkSettings": map[string]any{"Networks": map[string]any{"zeta": map[string]any{"IPAddress": "172.20.0.5"}, "alpha": map[string]any{"IPAddress": ""}}},
		})
	case strings.HasSuffix(path, "/stats"):
		w.Write(f.stats)
	case strings.HasSuffix(path, "/logs"):
		w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
		w.Write(f.logBody)
	case strings.HasSuffix(path, "/start"), strings.HasSuffix(path, "/stop"), strings.HasSuffix(path, "/restart"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestPing(t *testing.T) {
	_, d := newFake(t)
	info, err := d.Ping(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := runtime.Info{Name: "docker", Version: "28.0.1", APIVersion: "1.48", OS: "linux", Arch: "amd64"}
	if info != want {
		t.Fatalf("info = %+v, want %+v", info, want)
	}
}

func TestList(t *testing.T) {
	for _, project := range []string{"", "shop"} {
		f, d := newFake(t)
		cs, err := d.List(context.Background(), project)
		if err != nil {
			t.Fatal(err)
		}
		if len(cs) != 2 {
			t.Fatalf("len = %d", len(cs))
		}
		c := cs[0]
		if c.ID != "aaa" || c.Name != "shop-web-1" || c.Project != "shop" || c.Service != "web" || c.State != "running" {
			t.Errorf("container = %+v", c)
		}
		if !c.Created.Equal(time.Unix(1700000000, 0)) {
			t.Errorf("created = %v", c.Created)
		}
		if len(c.Ports) != 2 || c.Ports[0] != (runtime.Port{PrivatePort: 80, PublicPort: 8080, Type: "tcp", IP: "0.0.0.0"}) || c.Ports[1].IP != "" {
			t.Errorf("ports = %+v", c.Ports)
		}
		if cs[1].Project != "" || cs[1].Service != "" {
			t.Errorf("unlabelled container = %+v", cs[1])
		}
		f.mu.Lock()
		req := f.requests[len(f.requests)-1]
		f.mu.Unlock()
		if !strings.Contains(req, "all=1") || !strings.Contains(req, "ki.managed%3Dtrue") {
			t.Errorf("request lacks server-side managed filter: %s", req)
		}
		if got := strings.Contains(req, "ki.project%3Dshop"); got != (project != "") {
			t.Errorf("project filter present = %v for %q: %s", got, project, req)
		}
	}
}

func TestInspect(t *testing.T) {
	_, d := newFake(t)
	det, err := d.Inspect(context.Background(), "managed")
	if err != nil {
		t.Fatal(err)
	}
	if det.Name != "shop-web-1" || det.Image != "nginx" || det.State != "exited" || det.ExitCode != 3 ||
		det.RestartCount != 2 || det.Health != "healthy" || det.Project != "shop" {
		t.Errorf("detail = %+v", det)
	}
	if det.StartedAt.IsZero() || !det.FinishedAt.IsZero() {
		t.Errorf("times = %v %v", det.StartedAt, det.FinishedAt)
	}
	if det.Created.Nanosecond() != 123456789 {
		t.Errorf("created = %v", det.Created)
	}
	if len(det.Mounts) != 1 || det.Mounts[0] != (runtime.Mount{Type: "bind", Source: "/srv/data", Destination: "/data", ReadOnly: true}) {
		t.Errorf("mounts = %+v", det.Mounts)
	}
	if len(det.Networks) != 2 || det.Networks[0] != "alpha" {
		t.Errorf("networks = %v", det.Networks)
	}
	if want := map[string]string{"zeta": "172.20.0.5"}; !reflect.DeepEqual(det.IPs, want) {
		t.Errorf("ips = %v, want %v", det.IPs, want)
	}
}

func TestNotFoundAndNotManaged(t *testing.T) {
	f, d := newFake(t)
	ctx := context.Background()
	ops := map[string]func(id string) error{
		"inspect": func(id string) error { _, err := d.Inspect(ctx, id); return err },
		"start":   func(id string) error { return d.Start(ctx, id) },
		"stop":    func(id string) error { return d.Stop(ctx, id, time.Second) },
		"restart": func(id string) error { return d.Restart(ctx, id, time.Second) },
		"remove":  func(id string) error { return d.Remove(ctx, id, true) },
		"logs":    func(id string) error { return d.Logs(ctx, id, runtime.LogOptions{Tail: -1}, nil) },
	}
	for name, op := range ops {
		if err := op("missing"); !errors.Is(err, runtime.ErrNotFound) {
			t.Errorf("%s missing: err = %v", name, err)
		}
		if err := op("unmanaged"); !errors.Is(err, runtime.ErrNotManaged) {
			t.Errorf("%s unmanaged: err = %v", name, err)
		}
	}
	if f.has("POST /containers/unmanaged/") || f.has("DELETE") || f.has("GET /containers/unmanaged/logs") {
		t.Errorf("mutating request reached unmanaged container: %v", f.requests)
	}
}

func TestLifecycle(t *testing.T) {
	f, d := newFake(t)
	ctx := context.Background()
	if err := d.Start(ctx, "managed"); err != nil {
		t.Fatal(err)
	}
	if err := d.Stop(ctx, "managed", 7*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := d.Restart(ctx, "managed", -1); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(ctx, "managed", true); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"POST /containers/managed/start", "POST /containers/managed/stop?t=7", "POST /containers/managed/restart?", "DELETE /containers/managed?force=1"} {
		if !f.has(p) {
			t.Errorf("missing request %q in %v", p, f.requests)
		}
	}
	for _, r := range f.requests {
		if strings.Contains(r, "/restart") && strings.Contains(r, "t=") {
			t.Errorf("negative timeout must use engine default: %s", r)
		}
	}
}

func collect(t *testing.T, d *Docker, opts runtime.LogOptions) []runtime.LogLine {
	t.Helper()
	var got []runtime.LogLine
	err := d.Logs(context.Background(), "managed", opts, func(l runtime.LogLine) error {
		got = append(got, l)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestLogsMultiplexed(t *testing.T) {
	f, d := newFake(t)
	f.logBody = append(f.logBody, frame(1, "one\ntw")...)
	f.logBody = append(f.logBody, frame(2, "err\n")...)
	f.logBody = append(f.logBody, frame(1, "o\r\nlast")...)
	got := collect(t, d, runtime.LogOptions{Tail: 50})
	want := []runtime.LogLine{
		{Stream: "stdout", Text: "one"},
		{Stream: "stderr", Text: "err"},
		{Stream: "stdout", Text: "two"},
		{Stream: "stdout", Text: "last"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if !f.has("GET /containers/managed/logs?") {
		t.Fatal("no logs request")
	}
	last := f.requests[len(f.requests)-1]
	if !strings.Contains(last, "tail=50") || strings.Contains(last, "follow") {
		t.Errorf("request = %s", last)
	}
}

func TestLogsOptionsQuery(t *testing.T) {
	f, d := newFake(t)
	collect(t, d, runtime.LogOptions{Follow: true, Tail: -1, Timestamps: true, Since: time.Unix(1700000000, 5)})
	last := f.requests[len(f.requests)-1]
	if strings.Contains(last, "tail=") {
		t.Errorf("Tail -1 must not send tail: %s", last)
	}
	for _, s := range []string{"follow=1", "timestamps=1", "since=1700000000.000000005"} {
		if !strings.Contains(last, s) {
			t.Errorf("request %s lacks %s", last, s)
		}
	}
}

func TestLogsTimestamps(t *testing.T) {
	f, d := newFake(t)
	f.logBody = frame(1, "2024-01-02T03:04:05.5Z hello world\nnot-a-time x\n")
	got := collect(t, d, runtime.LogOptions{Tail: -1, Timestamps: true})
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Text != "hello world" || !got[0].Time.Equal(time.Date(2024, 1, 2, 3, 4, 5, 500000000, time.UTC)) {
		t.Errorf("line 0 = %+v", got[0])
	}
	if got[1].Text != "not-a-time x" || !got[1].Time.IsZero() {
		t.Errorf("line 1 = %+v", got[1])
	}
}

func TestLogsTTYRaw(t *testing.T) {
	f, d := newFake(t)
	f.tty = true
	f.logBody = []byte("raw1\nraw2")
	got := collect(t, d, runtime.LogOptions{Tail: -1})
	if len(got) != 2 || got[0].Text != "raw1" || got[1].Text != "raw2" || got[0].Stream != "stdout" {
		t.Fatalf("got %+v", got)
	}
}

func TestLogsTruncatesLongLines(t *testing.T) {
	f, d := newFake(t)
	long := strings.Repeat("x", maxLineBytes+500)
	f.logBody = append(frame(1, long[:40000]), frame(1, long[40000:]+"\nshort\n")...)
	got := collect(t, d, runtime.LogOptions{Tail: -1})
	if len(got) != 2 {
		t.Fatalf("got %d lines", len(got))
	}
	if len(got[0].Text) != maxLineBytes+len(truncMarker) || !strings.HasSuffix(got[0].Text, truncMarker) {
		t.Errorf("line 0 len = %d", len(got[0].Text))
	}
	if got[1].Text != "short" {
		t.Errorf("line 1 = %q", got[1].Text)
	}
}

func TestLogsCallbackError(t *testing.T) {
	f, d := newFake(t)
	f.logBody = frame(1, "a\nb\nc\n")
	stop := errors.New("stop")
	n := 0
	err := d.Logs(context.Background(), "managed", runtime.LogOptions{Tail: -1}, func(runtime.LogLine) error {
		n++
		return stop
	})
	if !errors.Is(err, stop) || n != 1 {
		t.Fatalf("err = %v, n = %d", err, n)
	}
}

func TestDaemonDown(t *testing.T) {
	d, err := New("unix://" + filepath.Join(t.TempDir(), "none.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if _, err := d.Ping(ctx); !errors.Is(err, runtime.ErrUnavailable) {
		t.Fatalf("Ping err = %v, want ErrUnavailable", err)
	}
	if _, err := d.List(ctx, ""); !errors.Is(err, runtime.ErrUnavailable) {
		t.Fatalf("List err = %v, want ErrUnavailable", err)
	}
	if err := d.Start(ctx, "x"); !errors.Is(err, runtime.ErrUnavailable) {
		t.Fatalf("Start err = %v, want ErrUnavailable", err)
	}
}

func TestDaemonStopped(t *testing.T) {
	f, d := newFake(t)
	if _, err := d.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Stop the fake API server (closes the listener); the client must map the
	// refused connection to ErrUnavailable.
	f.stop()
	if _, err := d.List(context.Background(), ""); !errors.Is(err, runtime.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}
