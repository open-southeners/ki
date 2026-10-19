package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/open-southeners/ki-agent/internal/runtime"
)

// pruneEngine is a fake Engine API recording requests for PruneProject tests.
type pruneEngine struct {
	mu       sync.Mutex
	requests []string
	filters  []string // decoded "filters" query of every list call
	gone     map[string]bool
	// vols holds the volumes that exist, by name, with their labels.
	vols map[string]map[string]string
	// shared keeps anonymous volumes alive on container removal, as when
	// another container still uses them.
	shared bool
}

func newPruneFake(t *testing.T) (*pruneEngine, *Docker) {
	t.Helper()
	f := &pruneEngine{gone: map[string]bool{}, vols: map[string]map[string]string{
		"anon1": {"com.docker.volume.anonymous": ""},
		"anon2": {"com.docker.volume.anonymous": ""},
		"named": {"ki.managed": "true"},
	}}
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	d, err := New("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return f, d
}

func (f *pruneEngine) serve(w http.ResponseWriter, r *http.Request) {
	path := versionPrefix.ReplaceAllString(r.URL.Path, "")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Api-Version", "1.47")
	if path == "/_ping" {
		w.Write([]byte("OK"))
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+path+"?"+r.URL.RawQuery)
	if r.Method == http.MethodGet && (path == "/containers/json" || path == "/networks" || path == "/volumes") {
		f.filters = append(f.filters, r.URL.Query().Get("filters"))
	}
	f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && path != "/containers/json" && strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
		switch strings.Split(path, "/")[2] {
		case "c1":
			w.Write([]byte(`{"Id":"c1","Mounts":[{"Type":"volume","Name":"anon1"},{"Type":"volume","Name":"named"},{"Type":"volume","Name":"anon2"},{"Type":"bind","Source":"/x"}]}`))
		case "c2":
			w.Write([]byte(`{"Id":"c2","Mounts":[{"Type":"volume","Name":"anon1"},{"Type":"volume","Name":"missing"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/volumes/"):
		name := strings.TrimPrefix(path, "/volumes/")
		labels, ok := f.vols[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"no such volume"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"Name": name, "Labels": labels})
	case path == "/containers/json":
		w.Write([]byte(`[{"Id":"c1"},{"Id":"c2"}]`))
	case path == "/networks":
		w.Write([]byte(`[{"Id":"n1","Name":"shop_default"}]`))
	case path == "/volumes":
		w.Write([]byte(`{"Volumes":[{"Name":"v1"},{"Name":"v2"}],"Warnings":null}`))
	case r.Method == http.MethodDelete:
		id := path[strings.LastIndex(path, "/")+1:]
		if strings.HasPrefix(path, "/containers/") && !f.shared {
			if v := r.URL.Query().Get("v"); v == "1" || v == "true" {
				delete(f.vols, "anon1")
				delete(f.vols, "anon2")
			}
		}
		if f.gone[id] {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"gone"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestPruneProjectEmptyRejected(t *testing.T) {
	f, d := newPruneFake(t)
	if _, err := d.PruneProject(context.Background(), "", true); err == nil {
		t.Fatal("empty project accepted")
	}
	if len(f.requests) != 0 {
		t.Fatalf("engine called for empty project: %v", f.requests)
	}
}

func TestPruneProjectWithVolumes(t *testing.T) {
	f, d := newPruneFake(t)
	rep, err := d.PruneProject(context.Background(), "shop", true)
	if err != nil {
		t.Fatal(err)
	}
	if want := (runtime.PruneReport{Containers: 2, Networks: 1, Volumes: 4, KeptVolumes: []string{}}); !reflect.DeepEqual(rep, want) {
		t.Fatalf("report = %+v, want %+v", rep, want)
	}
	want := []string{
		"GET /containers/json?",
		"GET /containers/c1/json?",
		"GET /volumes/anon1?",
		"GET /volumes/named?",
		"GET /volumes/anon2?",
		"DELETE /containers/c1?",
		"GET /containers/c2/json?",
		"GET /volumes/anon1?",
		"GET /volumes/missing?",
		"DELETE /containers/c2?",
		"GET /networks?",
		"DELETE /networks/n1?",
		"GET /volumes/anon1?",
		"GET /volumes/anon2?",
		"GET /volumes?",
		"DELETE /volumes/v1?",
		"DELETE /volumes/v2?",
	}
	var got []string
	for _, r := range f.requests {
		q, _, _ := strings.Cut(r, "?")
		got = append(got, q+"?")
		if strings.HasPrefix(r, "DELETE /containers/") {
			v, _ := url.ParseQuery(strings.SplitN(r, "?", 2)[1])
			if v.Get("force") != "1" && v.Get("force") != "true" || v.Get("v") != "1" && v.Get("v") != "true" {
				t.Fatalf("container remove query = %q, want force and v", r)
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	for _, flt := range f.filters {
		if !strings.Contains(flt, "ki.managed=true") || !strings.Contains(flt, "ki.project=shop") {
			t.Fatalf("list filters = %q, want both ki labels", flt)
		}
	}
	if len(f.filters) != 3 {
		t.Fatalf("list calls = %d, want 3", len(f.filters))
	}
}

func TestPruneProjectWithoutVolumes(t *testing.T) {
	f, d := newPruneFake(t)
	rep, err := d.PruneProject(context.Background(), "shop", false)
	if err != nil {
		t.Fatal(err)
	}
	want := runtime.PruneReport{Containers: 2, Networks: 1, KeptVolumes: []string{"anon1", "anon2"}}
	if !reflect.DeepEqual(rep, want) {
		t.Fatalf("report = %+v, want %+v", rep, want)
	}
	for _, r := range f.requests {
		if strings.Contains(r, "/volumes") && !strings.HasPrefix(r, "GET /volumes/") {
			t.Fatalf("volume request without volumes=true: %s", r)
		}
		if strings.HasPrefix(r, "DELETE /containers/") && (strings.Contains(r, "v=1") || strings.Contains(r, "v=true")) {
			t.Fatalf("anonymous volumes removed without volumes=true: %s", r)
		}
	}
}

func TestPruneProjectIgnoresNotFound(t *testing.T) {
	f, d := newPruneFake(t)
	f.gone["c2"], f.gone["v1"] = true, true
	rep, err := d.PruneProject(context.Background(), "shop", true)
	if err != nil {
		t.Fatal(err)
	}
	if want := (runtime.PruneReport{Containers: 1, Networks: 1, Volumes: 3, KeptVolumes: []string{}}); !reflect.DeepEqual(rep, want) {
		t.Fatalf("report = %+v, want %+v", rep, want)
	}
}

func TestPruneProjectSharedAnonymousVolumeNeitherCountedNorKept(t *testing.T) {
	f, d := newPruneFake(t)
	f.shared = true
	rep, err := d.PruneProject(context.Background(), "shop", true)
	if err != nil {
		t.Fatal(err)
	}
	if want := (runtime.PruneReport{Containers: 2, Networks: 1, Volumes: 2, KeptVolumes: []string{}}); !reflect.DeepEqual(rep, want) {
		t.Fatalf("report = %+v, want %+v", rep, want)
	}
}

func TestPruneProjectUnavailable(t *testing.T) {
	d, err := New("unix://" + filepath.Join(t.TempDir(), "missing.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.PruneProject(context.Background(), "shop", true); !errors.Is(err, runtime.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}
