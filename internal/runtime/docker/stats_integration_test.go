package docker

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/open-southeners/ki/internal/runtime"
)

// TestIntegrationStats checks a CPU-burning container reports growing
// counters. Gated on KI_TEST_DOCKER_HOST like TestIntegration.
func TestIntegrationStats(t *testing.T) {
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
	res, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "ki-it-stats-" + time.Now().Format("150405"),
		Config: &container.Config{
			Image:  "busybox:latest",
			Labels: map[string]string{runtime.LabelManaged: "true"},
			Cmd:    []string{"sh", "-c", "while :; do :; done"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = raw.ContainerRemove(context.Background(), res.ID, client.ContainerRemoveOptions{Force: true})
	})

	d, err := New(host)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Start(ctx, res.ID); err != nil {
		t.Fatal(err)
	}

	first, err := d.Stats(ctx, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	second, err := d.Stats(ctx, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Read.IsZero() || !second.Read.After(first.Read) {
		t.Errorf("read did not advance: %v -> %v", first.Read, second.Read)
	}
	if second.CPUTotalNs <= first.CPUTotalNs {
		t.Errorf("container CPU did not increase: %d -> %d", first.CPUTotalNs, second.CPUTotalNs)
	}
	if second.SystemCPUNs <= first.SystemCPUNs {
		t.Errorf("system CPU did not increase: %d -> %d", first.SystemCPUNs, second.SystemCPUNs)
	}
	if second.OnlineCPUs == 0 || second.MemUsage == 0 || second.MemLimit == 0 || second.Pids == 0 {
		t.Errorf("snapshot incomplete: %+v", second)
	}
}
