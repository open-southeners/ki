package monitor

import (
	"context"
	"log/slog"
	"strings"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
)

// GopsutilReader is the HostReader backed by gopsutil. Every part is read
// independently: a part that fails is logged at debug and left at zero.
type GopsutilReader struct {
	log *slog.Logger
}

// NewGopsutilReader returns the production HostReader.
func NewGopsutilReader(log *slog.Logger) *GopsutilReader {
	if log == nil {
		log = slog.Default()
	}
	return &GopsutilReader{log: log}
}

func toCPUTimes(t cpu.TimesStat) CPUTimes {
	return CPUTimes{
		Total: t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq +
			t.Softirq + t.Steal,
		Idle:   t.Idle,
		Iowait: t.Iowait,
	}
}

// Read implements HostReader.
func (g *GopsutilReader) Read(ctx context.Context, diskPaths []string) (HostCounters, error) {
	var c HostCounters
	if ts, err := cpu.TimesWithContext(ctx, false); err != nil {
		g.log.Debug("host cpu times unavailable", "err", err)
	} else if len(ts) > 0 {
		c.CPU = toCPUTimes(ts[0])
	}
	if ts, err := cpu.TimesWithContext(ctx, true); err != nil {
		g.log.Debug("host per-core cpu times unavailable", "err", err)
	} else {
		for _, t := range ts {
			c.PerCore = append(c.PerCore, toCPUTimes(t))
		}
	}
	if l, err := load.AvgWithContext(ctx); err != nil {
		g.log.Debug("host load unavailable", "err", err)
	} else if l != nil {
		c.Load = HostLoad{One: l.Load1, Five: l.Load5, Fifteen: l.Load15}
	}
	if m, err := mem.VirtualMemoryWithContext(ctx); err != nil {
		g.log.Debug("host memory unavailable", "err", err)
	} else if m != nil {
		c.Memory = HostMemory{Total: m.Total, Used: m.Used, Available: m.Available, Percent: m.UsedPercent}
	}
	if sw, err := mem.SwapMemoryWithContext(ctx); err != nil {
		g.log.Debug("host swap unavailable", "err", err)
	} else if sw != nil {
		c.Swap = HostSwap{Total: sw.Total, Used: sw.Used, Percent: sw.UsedPercent}
	}
	c.Disks = g.readDisks(ctx, diskPaths)
	if io, err := disk.IOCountersWithContext(ctx); err != nil {
		g.log.Debug("host disk io unavailable", "err", err)
	} else {
		c.DiskIO = make(map[string]DiskCounters, len(io))
		for name, s := range io {
			c.DiskIO[name] = DiskCounters{
				ReadBytes: s.ReadBytes, WriteBytes: s.WriteBytes,
				ReadOps: s.ReadCount, WriteOps: s.WriteCount,
			}
		}
	}
	if io, err := net.IOCountersWithContext(ctx, true); err != nil {
		g.log.Debug("host network io unavailable", "err", err)
	} else {
		c.Net = make(map[string]NetCounters, len(io))
		for _, s := range io {
			c.Net[s.Name] = NetCounters{RxBytes: s.BytesRecv, TxBytes: s.BytesSent}
		}
	}
	if up, err := host.UptimeWithContext(ctx); err != nil {
		g.log.Debug("host uptime unavailable", "err", err)
	} else {
		c.UptimeS = up
	}
	return c, nil
}

// readDisks reports usage for each path and the device of the longest
// mountpoint containing it.
func (g *GopsutilReader) readDisks(ctx context.Context, paths []string) []DiskUsage {
	parts, err := disk.PartitionsWithContext(ctx, true)
	if err != nil {
		g.log.Debug("host partitions unavailable", "err", err)
	}
	var out []DiskUsage
	for _, p := range paths {
		u, err := disk.UsageWithContext(ctx, p)
		if err != nil || u == nil {
			g.log.Debug("host disk usage unavailable", "path", p, "err", err)
			continue
		}
		d := DiskUsage{
			Path: p, Fstype: u.Fstype, Total: u.Total, Used: u.Used, Free: u.Free,
			InodesPercent: u.InodesUsedPercent,
		}
		best := -1
		for _, part := range parts {
			mp := part.Mountpoint
			if (mp == "/" || p == mp || strings.HasPrefix(p, strings.TrimSuffix(mp, "/")+"/")) && len(mp) > best {
				best = len(mp)
				d.Device = part.Device
				if d.Fstype == "" {
					d.Fstype = part.Fstype
				}
			}
		}
		out = append(out, d)
	}
	return out
}
