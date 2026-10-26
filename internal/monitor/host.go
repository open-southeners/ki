package monitor

import (
	"context"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// CPUTimes are cumulative CPU seconds. Total is the sum of every state
// (guest time is already part of user); Idle and Iowait are reported
// separately so that busy = Total - Idle - Iowait.
type CPUTimes struct {
	Total  float64
	Idle   float64
	Iowait float64
}

// DiskUsage is the usage of the filesystem holding Path.
type DiskUsage struct {
	Path          string
	Device        string
	Fstype        string
	Total         uint64
	Used          uint64
	Free          uint64
	InodesPercent float64
}

// DiskCounters are cumulative block device counters.
type DiskCounters struct {
	ReadBytes  uint64
	WriteBytes uint64
	ReadOps    uint64
	WriteOps   uint64
}

// NetCounters are cumulative network interface counters.
type NetCounters struct {
	RxBytes uint64
	TxBytes uint64
}

// HostCounters is one raw read of the host: cumulative counters plus gauges.
// Missing data is left at its zero value.
type HostCounters struct {
	CPU     CPUTimes
	PerCore []CPUTimes
	Load    HostLoad
	Memory  HostMemory
	Swap    HostSwap
	Disks   []DiskUsage
	DiskIO  map[string]DiskCounters
	Net     map[string]NetCounters
	UptimeS uint64
}

// HostReader reads raw host counters. It must not block for long, honours ctx
// and tolerates missing data. diskPaths are the paths whose filesystems to
// report usage for.
type HostReader interface {
	Read(ctx context.Context, diskPaths []string) (HostCounters, error)
}

// HostSamplerOptions configures a HostSampler.
type HostSamplerOptions struct {
	Reader HostReader
	// DiskPaths are the paths whose filesystems are reported, in addition
	// to "/". Typically the data dir and monitor.disk_paths.
	DiskPaths []string
	Logger    *slog.Logger
}

// HostSampler turns consecutive HostCounters reads into HostSamples. It is
// not safe for concurrent Sample calls (one goroutine owns it); SetPerCore may
// be called from any goroutine.
type HostSampler struct {
	reader  HostReader
	paths   []string
	log     *slog.Logger
	perCore atomic.Bool

	prev     *HostCounters
	prevTime time.Time
}

// NewHostSampler returns a sampler reading through opts.Reader.
func NewHostSampler(opts HostSamplerOptions) *HostSampler {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	paths := []string{"/"}
	seen := map[string]bool{"/": true}
	for _, p := range opts.DiskPaths {
		if p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	return &HostSampler{reader: opts.Reader, paths: paths, log: log}
}

// SetPerCore turns the per-core CPU breakdown on or off for later samples.
func (s *HostSampler) SetPerCore(on bool) { s.perCore.Store(on) }

var (
	wholeDiskRe = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|nvme\d+n\d+|mmcblk\d+)$`)
	excludedNIC = []string{"veth", "docker", "br-", "cni", "flannel", "virbr"}
)

func countedDisk(name string) bool { return wholeDiskRe.MatchString(name) }

func countedNIC(name string) bool {
	if name == "lo" {
		return false
	}
	for _, p := range excludedNIC {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// Sample reads the host at time now and returns the sample. Rates are nil on
// the first call. A reader error is returned and leaves the previous
// counters untouched.
func (s *HostSampler) Sample(ctx context.Context, now time.Time) (HostSample, error) {
	cur, err := s.reader.Read(ctx, s.paths)
	if err != nil {
		return HostSample{}, err
	}
	out := HostSample{
		Time:    now,
		CPU:     HostCPU{Cores: len(cur.PerCore)},
		Load:    cur.Load,
		Memory:  cur.Memory,
		Swap:    cur.Swap,
		Disks:   dedupeDisks(cur.Disks),
		Network: HostNetwork{Interfaces: []HostInterface{}},
		UptimeS: cur.UptimeS,
	}
	prev := s.prev
	elapsed := now.Sub(s.prevTime).Seconds()
	if prev != nil && elapsed <= 0 {
		prev = nil
	}
	if prev != nil {
		out.CPU.Percent = cpuPercent(prev.CPU, cur.CPU)
		if s.perCore.Load() && len(prev.PerCore) == len(cur.PerCore) {
			pc := make([]float64, len(cur.PerCore))
			for i := range pc {
				if p := cpuPercent(prev.PerCore[i], cur.PerCore[i]); p != nil {
					pc[i] = *p
				}
			}
			out.CPU.PerCore = pc
		}
		s.diskRates(&out, prev, &cur, elapsed)
	}
	s.netRates(&out, prev, &cur, elapsed)
	s.prev = &cur
	s.prevTime = now
	return out, nil
}

func cpuPercent(prev, cur CPUTimes) *float64 {
	dTotal := cur.Total - prev.Total
	dBusy := (cur.Total - cur.Idle - cur.Iowait) - (prev.Total - prev.Idle - prev.Iowait)
	if dTotal <= 0 || dBusy < 0 {
		return nil
	}
	p := dBusy / dTotal * 100
	if p > 100 {
		p = 100
	}
	return &p
}

func rate(prev, cur uint64, elapsed float64) (float64, bool) {
	if cur < prev {
		return 0, false
	}
	return float64(cur-prev) / elapsed, true
}

func (s *HostSampler) diskRates(out *HostSample, prev, cur *HostCounters, elapsed float64) {
	var rb, wb, ro, wo float64
	counted := false
	for name, c := range cur.DiskIO {
		p, ok := prev.DiskIO[name]
		if !countedDisk(name) || !ok {
			continue
		}
		a, ok1 := rate(p.ReadBytes, c.ReadBytes, elapsed)
		b, ok2 := rate(p.WriteBytes, c.WriteBytes, elapsed)
		x, ok3 := rate(p.ReadOps, c.ReadOps, elapsed)
		y, ok4 := rate(p.WriteOps, c.WriteOps, elapsed)
		if !(ok1 && ok2 && ok3 && ok4) {
			s.log.Debug("host disk counter went backwards", "device", name)
			continue
		}
		rb, wb, ro, wo = rb+a, wb+b, ro+x, wo+y
		counted = true
	}
	if counted {
		out.DiskIO = HostDiskIO{&rb, &wb, &ro, &wo}
	}
}

func (s *HostSampler) netRates(out *HostSample, prev, cur *HostCounters, elapsed float64) {
	names := make([]string, 0, len(cur.Net))
	for name := range cur.Net {
		if countedNIC(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var rx, tx float64
	counted := false
	for _, name := range names {
		c := cur.Net[name]
		it := HostInterface{Name: name}
		if prev != nil {
			if p, ok := prev.Net[name]; ok {
				a, ok1 := rate(p.RxBytes, c.RxBytes, elapsed)
				b, ok2 := rate(p.TxBytes, c.TxBytes, elapsed)
				if ok1 && ok2 {
					it.RxBytesPerS, it.TxBytesPerS = &a, &b
					rx, tx = rx+a, tx+b
					counted = true
				} else {
					s.log.Debug("host net counter went backwards", "interface", name)
				}
			}
		}
		out.Network.Interfaces = append(out.Network.Interfaces, it)
	}
	if counted {
		out.Network.RxBytesPerS, out.Network.TxBytesPerS = &rx, &tx
	}
}

// dedupeDisks keeps the first entry per device (per path when the device is
// unknown).
func dedupeDisks(in []DiskUsage) []HostDisk {
	out := []HostDisk{}
	seen := map[string]bool{}
	for _, d := range in {
		key := d.Device
		if key == "" {
			key = "path:" + d.Path
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		pct := 0.0
		if d.Total > 0 {
			pct = float64(d.Used) / float64(d.Total) * 100
		}
		out = append(out, HostDisk{
			Path: d.Path, Device: d.Device, Fstype: d.Fstype,
			Total: d.Total, Used: d.Used, Free: d.Free,
			Percent: pct, InodesPercent: d.InodesPercent,
		})
	}
	return out
}
