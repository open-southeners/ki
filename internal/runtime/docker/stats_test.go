package docker

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/open-southeners/ki/internal/runtime"
)

func TestStatsFixtures(t *testing.T) {
	cases := []struct {
		file string
		want runtime.StatsSnapshot
	}{
		{"stats_cgroup_v2.json", runtime.StatsSnapshot{
			Read:       time.Date(2026, 3, 4, 10, 15, 30, 123456789, time.UTC),
			CPUTotalNs: 1234567890, SystemCPUNs: 98765432100000, OnlineCPUs: 4,
			MemUsage: 52428800, MemInactiveFile: 20971520, MemLimit: 8589934592,
			NetRx: 1250, NetTx: 625, BlkRead: 5120, BlkWrite: 10240, Pids: 7,
		}},
		// v1: no online_cpus (falls back to percpu_usage), total_inactive_file
		// wins over the per-cgroup inactive_file, op names are capitalised.
		{"stats_cgroup_v1.json", runtime.StatsSnapshot{
			Read:       time.Date(2026, 3, 4, 10, 15, 30, 500000000, time.UTC),
			CPUTotalNs: 400000000, SystemCPUNs: 55555000000000, OnlineCPUs: 4,
			MemUsage: 41943040, MemInactiveFile: 25165824, MemLimit: 2147483648,
			NetRx: 2048, NetTx: 1024, BlkRead: 65536, BlkWrite: 131072, Pids: 3,
		}},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			body, err := os.ReadFile("testdata/" + c.file)
			if err != nil {
				t.Fatal(err)
			}
			f, d := newFake(t)
			f.stats = body
			got, err := d.Stats(context.Background(), "managed")
			if err != nil {
				t.Fatal(err)
			}
			if !got.Read.Equal(c.want.Read) {
				t.Errorf("read = %v, want %v", got.Read, c.want.Read)
			}
			got.Read, c.want.Read = time.Time{}, time.Time{}
			if got != c.want {
				t.Errorf("snapshot = %+v\nwant       %+v", got, c.want)
			}
			if !f.has("GET /containers/managed/stats?") {
				t.Errorf("no stats request: %v", f.requests)
			}
		})
	}
}

func TestStatsOneShotAndGuards(t *testing.T) {
	f, d := newFake(t)
	f.stats = []byte(`{"read":"2026-03-04T10:15:30Z"}`)
	if _, err := d.Stats(context.Background(), "managed"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	req := f.requests[len(f.requests)-1]
	f.mu.Unlock()
	if req != "GET /containers/managed/stats?one-shot=true&stream=false" {
		t.Errorf("request = %s", req)
	}
	if _, err := d.Stats(context.Background(), "unmanaged"); !errors.Is(err, runtime.ErrNotManaged) {
		t.Errorf("unmanaged = %v", err)
	}
	if f.has("GET /containers/unmanaged/stats") {
		t.Error("stats requested for an unmanaged container")
	}
	if _, err := d.Stats(context.Background(), "missing"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
}
