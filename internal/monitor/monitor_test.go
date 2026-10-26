package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

type fakeReader struct {
	reads []HostCounters
	err   error
	paths []string
	i     int
}

func (f *fakeReader) Read(_ context.Context, paths []string) (HostCounters, error) {
	f.paths = paths
	if f.err != nil {
		return HostCounters{}, f.err
	}
	c := f.reads[f.i]
	if f.i < len(f.reads)-1 {
		f.i++
	}
	return c, nil
}

var t0 = time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)

func near(p *float64, want float64) bool { return p != nil && math.Abs(*p-want) < 1e-9 }

func sampleN(t *testing.T, s *HostSampler, gap time.Duration, n int) []HostSample {
	t.Helper()
	var out []HostSample
	for i := 0; i < n; i++ {
		smp, err := s.Sample(context.Background(), t0.Add(time.Duration(i)*gap))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, smp)
	}
	return out
}

func TestRing(t *testing.T) {
	r := NewRing[int](time.Minute, 15*time.Second) // 4 slots
	if _, ok := r.Latest(); ok {
		t.Fatal("empty ring has latest")
	}
	r.Add(t0.Add(1*time.Second), 1)
	r.Add(t0.Add(14*time.Second), 2) // same slot replaces
	r.Add(t0.Add(16*time.Second), 3)
	if got := r.Snapshot(); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("snapshot = %v", got)
	}
	r.Add(t0.Add(5*time.Second), 99) // older slot ignored
	for i := 2; i < 8; i++ {
		r.Add(t0.Add(time.Duration(i)*15*time.Second), i*10)
	}
	got := r.Snapshot()
	if len(got) != 4 || got[0] != 40 || got[3] != 70 {
		t.Fatalf("wrapped snapshot = %v", got)
	}
	if v, ok := r.Latest(); !ok || v != 70 {
		t.Fatalf("latest = %v %v", v, ok)
	}
}

func TestRingConcurrent(t *testing.T) {
	r := NewRing[int](time.Minute, time.Second)
	done := make(chan struct{})
	for g := 0; g < 4; g++ {
		go func() {
			for i := 0; i < 200; i++ {
				r.Add(t0.Add(time.Duration(i)*time.Second), i)
				r.Snapshot()
				r.Latest()
			}
			done <- struct{}{}
		}()
	}
	for g := 0; g < 4; g++ {
		<-done
	}
	if r.Len() > 60 {
		t.Fatalf("len = %d", r.Len())
	}
}

func TestHostSamplerCPU(t *testing.T) {
	tests := []struct {
		name string
		a, b CPUTimes
		want *float64
	}{
		{"half busy", CPUTimes{100, 50, 0}, CPUTimes{200, 100, 0}, ptr(50)},
		{"iowait is not busy", CPUTimes{100, 50, 0}, CPUTimes{200, 100, 50}, ptr(0)},
		{"idle", CPUTimes{100, 100, 0}, CPUTimes{200, 200, 0}, ptr(0)},
		{"no time passed", CPUTimes{100, 50, 0}, CPUTimes{100, 50, 0}, nil},
		{"counter reset", CPUTimes{100, 50, 0}, CPUTimes{10, 5, 0}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewHostSampler(HostSamplerOptions{Reader: &fakeReader{reads: []HostCounters{{CPU: tt.a}, {CPU: tt.b}}}})
			got := sampleN(t, s, 10*time.Second, 2)
			if got[0].CPU.Percent != nil {
				t.Fatal("first sample must have nil percent")
			}
			if tt.want == nil {
				if got[1].CPU.Percent != nil {
					t.Fatalf("percent = %v, want nil", *got[1].CPU.Percent)
				}
			} else if !near(got[1].CPU.Percent, *tt.want) {
				t.Fatalf("percent = %v, want %v", got[1].CPU.Percent, *tt.want)
			}
		})
	}
}

func ptr(f float64) *float64 { return &f }

func TestHostSamplerPerCore(t *testing.T) {
	reads := []HostCounters{
		{CPU: CPUTimes{200, 100, 0}, PerCore: []CPUTimes{{100, 50, 0}, {100, 50, 0}}},
		{CPU: CPUTimes{400, 200, 0}, PerCore: []CPUTimes{{200, 50, 0}, {200, 100, 0}}},
	}
	s := NewHostSampler(HostSamplerOptions{Reader: &fakeReader{reads: reads}})
	got := sampleN(t, s, time.Second, 2)
	if got[1].CPU.Cores != 2 || got[1].CPU.PerCore != nil {
		t.Fatalf("per-core off: %+v", got[1].CPU)
	}

	s = NewHostSampler(HostSamplerOptions{Reader: &fakeReader{reads: reads}})
	s.SetPerCore(true)
	got = sampleN(t, s, time.Second, 2)
	pc := got[1].CPU.PerCore
	if len(pc) != 2 || pc[0] != 100 || pc[1] != 50 {
		t.Fatalf("per_core = %v", pc)
	}
	if got[1].WithoutPerCore().CPU.PerCore != nil {
		t.Fatal("WithoutPerCore kept per_core")
	}
}

func TestHostSamplerDiskIO(t *testing.T) {
	a := map[string]DiskCounters{
		"sda":       {ReadBytes: 1000, WriteBytes: 2000, ReadOps: 10, WriteOps: 20},
		"sda1":      {ReadBytes: 1000, WriteBytes: 2000, ReadOps: 10, WriteOps: 20},
		"nvme0n1":   {ReadBytes: 0, WriteBytes: 0},
		"nvme0n1p1": {ReadBytes: 0},
		"loop0":     {ReadBytes: 5},
		"dm-0":      {ReadBytes: 5},
		"vdb":       {ReadBytes: 100},
		"xvda":      {ReadBytes: 100},
		"mmcblk0":   {ReadBytes: 100},
		"mmcblk0p1": {ReadBytes: 100},
		"ram0":      {ReadBytes: 100},
	}
	b := map[string]DiskCounters{
		"sda":       {ReadBytes: 3000, WriteBytes: 6000, ReadOps: 30, WriteOps: 60},
		"sda1":      {ReadBytes: 9000, WriteBytes: 9000, ReadOps: 90, WriteOps: 90},
		"nvme0n1":   {ReadBytes: 1000, WriteBytes: 500, ReadOps: 1, WriteOps: 2},
		"nvme0n1p1": {ReadBytes: 9999},
		"loop0":     {ReadBytes: 9999},
		"dm-0":      {ReadBytes: 9999},
		"vdb":       {ReadBytes: 100},
		"xvda":      {ReadBytes: 100},
		"mmcblk0":   {ReadBytes: 100},
		"mmcblk0p1": {ReadBytes: 9999},
		"ram0":      {ReadBytes: 9999},
	}
	s := NewHostSampler(HostSamplerOptions{Reader: &fakeReader{reads: []HostCounters{{DiskIO: a}, {DiskIO: b}}}})
	got := sampleN(t, s, 2*time.Second, 2)
	if got[0].DiskIO.ReadBytesPerS != nil {
		t.Fatal("first sample must have nil rates")
	}
	io := got[1].DiskIO
	if !near(io.ReadBytesPerS, 1500) || !near(io.WriteBytesPerS, 2250) ||
		!near(io.ReadOpsPerS, 10.5) || !near(io.WriteOpsPerS, 21) {
		t.Fatalf("disk io = %+v", io)
	}
}

func TestHostSamplerDiskIOCounterReset(t *testing.T) {
	a := map[string]DiskCounters{"sda": {ReadBytes: 1000}, "sdb": {ReadBytes: 1000}}
	b := map[string]DiskCounters{"sda": {ReadBytes: 10}, "sdb": {ReadBytes: 3000}}
	s := NewHostSampler(HostSamplerOptions{Reader: &fakeReader{reads: []HostCounters{{DiskIO: a}, {DiskIO: b}}}})
	got := sampleN(t, s, time.Second, 2)
	if !near(got[1].DiskIO.ReadBytesPerS, 2000) {
		t.Fatalf("reset device must be skipped: %+v", got[1].DiskIO)
	}
	b = map[string]DiskCounters{"sda": {ReadBytes: 10}, "sdb": {ReadBytes: 10}}
	s = NewHostSampler(HostSamplerOptions{Reader: &fakeReader{reads: []HostCounters{{DiskIO: a}, {DiskIO: b}}}})
	got = sampleN(t, s, time.Second, 2)
	if got[1].DiskIO.ReadBytesPerS != nil {
		t.Fatalf("all reset must be nil: %+v", got[1].DiskIO)
	}
}

func TestHostSamplerNetwork(t *testing.T) {
	a := map[string]NetCounters{
		"eth0": {1000, 500}, "lo": {1, 1}, "veth1234": {1, 1}, "docker0": {1, 1},
		"br-abc": {1, 1}, "cni0": {1, 1}, "flannel.1": {1, 1}, "virbr0": {1, 1},
		"tun0": {100, 100}, "tap0": {100, 100}, "wlan0": {0, 0},
	}
	b := map[string]NetCounters{
		"eth0": {3000, 1500}, "lo": {999, 999}, "veth1234": {999, 999}, "docker0": {999, 999},
		"br-abc": {999, 999}, "cni0": {999, 999}, "flannel.1": {999, 999}, "virbr0": {999, 999},
		"tun0": {300, 100}, "tap0": {50, 100}, "wlan0": {10, 20}, "eth1": {5, 5},
	}
	s := NewHostSampler(HostSamplerOptions{Reader: &fakeReader{reads: []HostCounters{{Net: a}, {Net: b}}}})
	got := sampleN(t, s, 2*time.Second, 2)
	if got[0].Network.RxBytesPerS != nil || len(got[0].Network.Interfaces) != 4 {
		t.Fatalf("first sample: %+v", got[0].Network)
	}
	n := got[1].Network
	var names []string
	for _, i := range n.Interfaces {
		names = append(names, i.Name)
	}
	want := []string{"eth0", "eth1", "tap0", "tun0", "wlan0"}
	if len(names) != len(want) {
		t.Fatalf("interfaces = %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("interfaces = %v", names)
		}
	}
	// eth0 +2000 rx, tun0 +200 rx, tap0 reset (skipped), wlan0 +10; eth1 new (no rate).
	if !near(n.RxBytesPerS, (2000+200+10)/2.0) || !near(n.TxBytesPerS, (1000+0+20)/2.0) {
		t.Fatalf("totals = %v %v", *n.RxBytesPerS, *n.TxBytesPerS)
	}
	if n.Interfaces[1].RxBytesPerS != nil || n.Interfaces[2].RxBytesPerS != nil {
		t.Fatalf("new and reset interfaces must have nil rates: %+v", n.Interfaces)
	}
}

func TestHostSamplerDisks(t *testing.T) {
	r := &fakeReader{reads: []HostCounters{{Disks: []DiskUsage{
		{Path: "/", Device: "/dev/sda1", Fstype: "ext4", Total: 1000, Used: 550, Free: 450, InodesPercent: 3.2},
		{Path: "/var/lib/ki", Device: "/dev/sda1", Fstype: "ext4", Total: 1000, Used: 550, Free: 450},
		{Path: "/data", Device: "/dev/sdb1", Fstype: "xfs", Total: 0},
		{Path: "/x", Device: "", Total: 10, Used: 1},
		{Path: "/y", Device: "", Total: 10, Used: 1},
	}}}}
	s := NewHostSampler(HostSamplerOptions{Reader: r, DiskPaths: []string{"/var/lib/ki", "/", "/data", "", "/data"}})
	got := sampleN(t, s, time.Second, 1)
	if want := []string{"/", "/var/lib/ki", "/data"}; len(r.paths) != 3 || r.paths[0] != want[0] || r.paths[1] != want[1] || r.paths[2] != want[2] {
		t.Fatalf("paths = %v", r.paths)
	}
	d := got[0].Disks
	if len(d) != 4 || d[0].Path != "/" || d[1].Path != "/data" || d[2].Path != "/x" || d[3].Path != "/y" {
		t.Fatalf("disks = %+v", d)
	}
	if math.Abs(d[0].Percent-55) > 1e-9 || d[1].Percent != 0 || d[0].InodesPercent != 3.2 {
		t.Fatalf("disk pct = %+v", d)
	}
}

func TestHostSamplerErrorKeepsState(t *testing.T) {
	r := &fakeReader{err: errors.New("boom")}
	s := NewHostSampler(HostSamplerOptions{Reader: r})
	if _, err := s.Sample(context.Background(), t0); err == nil {
		t.Fatal("want error")
	}
	if s.prev != nil {
		t.Fatal("state advanced on error")
	}
}

func TestHostSampleJSON(t *testing.T) {
	reads := []HostCounters{
		{
			CPU: CPUTimes{100, 50, 0}, PerCore: []CPUTimes{{100, 50, 0}},
			Load:    HostLoad{0.2, 0.3, 0.25},
			Memory:  HostMemory{Total: 100, Used: 41, Available: 59, Percent: 41},
			Swap:    HostSwap{Total: 10},
			Disks:   []DiskUsage{{Path: "/", Device: "/dev/sda1", Fstype: "ext4", Total: 100, Used: 50, Free: 50, InodesPercent: 1}},
			DiskIO:  map[string]DiskCounters{"sda": {}},
			Net:     map[string]NetCounters{"eth0": {}},
			UptimeS: 7,
		},
		{
			CPU: CPUTimes{200, 100, 0}, PerCore: []CPUTimes{{200, 100, 0}},
			DiskIO: map[string]DiskCounters{"sda": {ReadBytes: 10, WriteBytes: 20, ReadOps: 1, WriteOps: 2}},
			Net:    map[string]NetCounters{"eth0": {RxBytes: 30, TxBytes: 40}},
		},
	}
	s := NewHostSampler(HostSamplerOptions{Reader: &fakeReader{reads: reads}})
	s.SetPerCore(true)
	got := sampleN(t, s, time.Second, 2)

	first, _ := json.Marshal(got[0])
	const wantFirst = `{"time":"2026-01-02T03:04:00Z","cpu":{"percent":null,"cores":1},"load":{"1":0.2,"5":0.3,"15":0.25},` +
		`"memory":{"total":100,"used":41,"available":59,"percent":41},"swap":{"total":10,"used":0,"percent":0},` +
		`"disks":[{"path":"/","device":"/dev/sda1","fstype":"ext4","total":100,"used":50,"free":50,"percent":50,"inodes_percent":1}],` +
		`"disk_io":{"read_bytes_per_s":null,"write_bytes_per_s":null,"read_ops_per_s":null,"write_ops_per_s":null},` +
		`"network":{"rx_bytes_per_s":null,"tx_bytes_per_s":null,"interfaces":[{"name":"eth0","rx_bytes_per_s":null,"tx_bytes_per_s":null}]},"uptime_s":7}`
	if string(first) != wantFirst {
		t.Fatalf("first:\n%s\nwant:\n%s", first, wantFirst)
	}

	second, _ := json.Marshal(got[1])
	const wantSecond = `{"time":"2026-01-02T03:04:01Z","cpu":{"percent":50,"cores":1,"per_core":[50]},"load":{"1":0,"5":0,"15":0},` +
		`"memory":{"total":0,"used":0,"available":0,"percent":0},"swap":{"total":0,"used":0,"percent":0},"disks":[],` +
		`"disk_io":{"read_bytes_per_s":10,"write_bytes_per_s":20,"read_ops_per_s":1,"write_ops_per_s":2},` +
		`"network":{"rx_bytes_per_s":30,"tx_bytes_per_s":40,"interfaces":[{"name":"eth0","rx_bytes_per_s":30,"tx_bytes_per_s":40}]},"uptime_s":0}`
	if string(second) != wantSecond {
		t.Fatalf("second:\n%s\nwant:\n%s", second, wantSecond)
	}
}

func TestGopsutilReaderSmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := NewGopsutilReader(nil)
	s := NewHostSampler(HostSamplerOptions{Reader: r})
	if _, err := s.Sample(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	smp, err := s.Sample(ctx, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(smp); err != nil {
		t.Fatal(err)
	}
	// Anything may be missing in a sandbox; the sample must still be well formed.
	if smp.Disks == nil || smp.Network.Interfaces == nil {
		t.Fatalf("nil lists: %+v", smp)
	}
}
