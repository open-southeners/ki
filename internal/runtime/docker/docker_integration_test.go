package docker

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/open-southeners/ki-agent/internal/runtime"
)

// TestIntegration runs against a real engine and is gated on
// KI_TEST_DOCKER_HOST (the dind sidecar; never point it at the host daemon):
//
//	docker compose run --rm test
func TestIntegration(t *testing.T) {
	host := os.Getenv("KI_TEST_DOCKER_HOST")
	if host == "" {
		t.Skip("KI_TEST_DOCKER_HOST not set; run via `docker compose run --rm test`")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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

	create := func(name string, labels map[string]string) string {
		t.Helper()
		res, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name: name,
			Config: &container.Config{
				Image:  "busybox:latest",
				Labels: labels,
				Cmd:    []string{"sh", "-c", "echo hello-out; echo hello-err >&2; sleep 600"},
			},
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() {
			_, _ = raw.ContainerRemove(context.Background(), res.ID, client.ContainerRemoveOptions{Force: true})
		})
		return res.ID
	}
	suffix := time.Now().Format("150405")
	managed := create("ki-it-managed-"+suffix, map[string]string{
		runtime.LabelManaged: "true", runtime.LabelProject: "ittest-" + suffix, runtime.LabelService: "app",
	})
	bare := create("ki-it-bare-"+suffix, nil)

	d, err := New(host)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	info, err := d.Ping(ctx)
	if err != nil || info.Name != "docker" || info.Version == "" {
		t.Fatalf("Ping = %+v, %v", info, err)
	}

	// List shows the managed container only.
	cs, err := d.List(ctx, "ittest-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].ID != managed || cs[0].Service != "app" || cs[0].State != "created" {
		t.Fatalf("List(project) = %+v", cs)
	}
	all, err := d.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if c.ID == bare {
			t.Fatal("unmanaged container listed")
		}
	}

	// Unmanaged container is untouchable.
	if err := d.Start(ctx, bare); !errors.Is(err, runtime.ErrNotManaged) {
		t.Fatalf("Start(unmanaged) = %v", err)
	}
	if _, err := d.Inspect(ctx, bare); !errors.Is(err, runtime.ErrNotManaged) {
		t.Fatalf("Inspect(unmanaged) = %v", err)
	}
	if _, err := d.Inspect(ctx, "does-not-exist-"+suffix); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Inspect(missing) = %v", err)
	}

	if err := d.Start(ctx, managed); err != nil {
		t.Fatal(err)
	}
	det, err := d.Inspect(ctx, managed)
	if err != nil {
		t.Fatal(err)
	}
	if det.State != "running" || det.StartedAt.IsZero() || det.Image != "busybox:latest" {
		t.Fatalf("Inspect = %+v", det)
	}

	// Follow logs: both streams arrive, then stop ends the stream.
	var mu sync.Mutex
	var lines []runtime.LogLine
	done := make(chan error, 1)
	go func() {
		done <- d.Logs(ctx, managed, runtime.LogOptions{Follow: true, Tail: -1, Timestamps: true}, func(l runtime.LogLine) error {
			mu.Lock()
			lines = append(lines, l)
			mu.Unlock()
			return nil
		})
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		mu.Lock()
		n := len(lines)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for log lines, got %d", n)
		}
		time.Sleep(100 * time.Millisecond)
	}
	mu.Lock()
	seen := map[string]runtime.LogLine{}
	for _, l := range lines {
		seen[l.Stream] = l
	}
	mu.Unlock()
	if seen["stdout"].Text != "hello-out" || seen["stderr"].Text != "hello-err" || seen["stdout"].Time.IsZero() {
		t.Fatalf("log lines = %+v", seen)
	}

	if err := d.Stop(ctx, managed, 1*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Logs after stop: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("follow did not end after container stop")
	}
	det, err = d.Inspect(ctx, managed)
	if err != nil || det.State != "exited" {
		t.Fatalf("after Stop: %+v, %v", det, err)
	}

	if err := d.Remove(ctx, managed, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Inspect(ctx, managed); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Inspect(removed) = %v", err)
	}

	testPrune(t, ctx, raw, d, suffix)
	testPruneAnonymousVolumes(t, ctx, raw, d, suffix)
}

// testPrune checks PruneProject removes labelled resources and spares the rest.
func testPrune(t *testing.T, ctx context.Context, raw *client.Client, d *Docker, suffix string) {
	t.Helper()
	project := "ittest-prune-" + suffix
	both := map[string]string{runtime.LabelManaged: "true", runtime.LabelProject: project}
	onlyProject := map[string]string{runtime.LabelProject: project}

	mkContainer := func(name string, labels map[string]string) string {
		t.Helper()
		res, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name:   name,
			Config: &container.Config{Image: "busybox:latest", Labels: labels, Cmd: []string{"sleep", "600"}},
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() {
			_, _ = raw.ContainerRemove(context.Background(), res.ID, client.ContainerRemoveOptions{Force: true})
		})
		return res.ID
	}
	mkNetwork := func(name string, labels map[string]string) string {
		t.Helper()
		res, err := raw.NetworkCreate(ctx, name, client.NetworkCreateOptions{Labels: labels})
		if err != nil {
			t.Fatalf("network %s: %v", name, err)
		}
		t.Cleanup(func() { _, _ = raw.NetworkRemove(context.Background(), res.ID, client.NetworkRemoveOptions{}) })
		return res.ID
	}
	mkVolume := func(name string, labels map[string]string) {
		t.Helper()
		if _, err := raw.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels}); err != nil {
			t.Fatalf("volume %s: %v", name, err)
		}
		t.Cleanup(func() { _, _ = raw.VolumeRemove(context.Background(), name, client.VolumeRemoveOptions{Force: true}) })
	}

	mkContainer("ki-it-prune-c-"+suffix, both)
	keepC := mkContainer("ki-it-prune-keepc-"+suffix, onlyProject)
	mkNetwork("ki-it-prune-n-"+suffix, both)
	keepN := mkNetwork("ki-it-prune-keepn-"+suffix, onlyProject)
	mkVolume("ki-it-prune-v-"+suffix, both)
	mkVolume("ki-it-prune-keepv-"+suffix, onlyProject)

	rep, err := d.PruneProject(ctx, project, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := (runtime.PruneReport{Containers: 1, Networks: 1, Volumes: 1, KeptVolumes: []string{}}); !reflect.DeepEqual(rep, want) {
		t.Fatalf("PruneProject = %+v, want %+v", rep, want)
	}
	if _, err := raw.ContainerInspect(ctx, keepC, client.ContainerInspectOptions{}); err != nil {
		t.Fatalf("unmanaged container removed: %v", err)
	}
	if _, err := raw.NetworkInspect(ctx, keepN, client.NetworkInspectOptions{}); err != nil {
		t.Fatalf("unmanaged network removed: %v", err)
	}
	if _, err := raw.VolumeInspect(ctx, "ki-it-prune-keepv-"+suffix, client.VolumeInspectOptions{}); err != nil {
		t.Fatalf("unmanaged volume removed: %v", err)
	}
	if rep, err := d.PruneProject(ctx, project, true); err != nil || !reflect.DeepEqual(rep, runtime.PruneReport{KeptVolumes: []string{}}) {
		t.Fatalf("second PruneProject = %+v, %v", rep, err)
	}
}

// testPruneAnonymousVolumes checks anonymous volumes are reported and kept
// without volumes, and removed and counted with volumes.
func testPruneAnonymousVolumes(t *testing.T, ctx context.Context, raw *client.Client, d *Docker, suffix string) {
	t.Helper()
	project := "ittest-anon-" + suffix
	labels := map[string]string{runtime.LabelManaged: "true", runtime.LabelProject: project}
	mk := func(name string) string {
		t.Helper()
		res, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name: name,
			Config: &container.Config{
				Image: "busybox:latest", Labels: labels, Cmd: []string{"sleep", "600"},
				Volumes: map[string]struct{}{"/data": {}},
			},
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() {
			_, _ = raw.ContainerRemove(context.Background(), res.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		})
		in, err := raw.ContainerInspect(ctx, res.ID, client.ContainerInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range in.Container.Mounts {
			if m.Type == "volume" {
				vol := m.Name
				t.Cleanup(func() { _, _ = raw.VolumeRemove(context.Background(), vol, client.VolumeRemoveOptions{Force: true}) })
				return vol
			}
		}
		t.Fatalf("%s has no volume mount", name)
		return ""
	}

	vol := mk("ki-it-anon-a-" + suffix)
	rep, err := d.PruneProject(ctx, project, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := (runtime.PruneReport{Containers: 1, KeptVolumes: []string{vol}}); !reflect.DeepEqual(rep, want) {
		t.Fatalf("PruneProject(false) = %+v, want %+v", rep, want)
	}
	if _, err := raw.VolumeInspect(ctx, vol, client.VolumeInspectOptions{}); err != nil {
		t.Fatalf("kept anonymous volume gone: %v", err)
	}

	vol = mk("ki-it-anon-b-" + suffix)
	rep, err = d.PruneProject(ctx, project, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := (runtime.PruneReport{Containers: 1, Volumes: 1, KeptVolumes: []string{}}); !reflect.DeepEqual(rep, want) {
		t.Fatalf("PruneProject(true) = %+v, want %+v", rep, want)
	}
	if _, err := raw.VolumeInspect(ctx, vol, client.VolumeInspectOptions{}); err == nil {
		t.Fatal("anonymous volume survived volumes=true")
	}
}
