// Package docker implements runtime.Runtime on the Docker Engine API.
//
// It is the only package allowed to import the (pre-1.0) moby client.
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/open-southeners/ki/internal/runtime"
)

// DefaultHost is the Docker endpoint used when none is configured.
const DefaultHost = "unix:///var/run/docker.sock"

// anonymousVolumeLabel is the only label Docker puts on anonymous volumes.
const anonymousVolumeLabel = "com.docker.volume.anonymous"

// maxLineBytes is the longest log line delivered; longer lines are truncated.
const maxLineBytes = 64 << 10

// truncMarker is appended to truncated log lines.
const truncMarker = "…[truncated]"

// Docker is a runtime.Runtime backed by a Docker Engine.
type Docker struct {
	cli *client.Client
}

var _ runtime.Runtime = (*Docker)(nil)

// New connects to the engine at host (unix:// or tcp://; empty means
// DefaultHost). The connection is lazy and the API version is negotiated on
// the first request, so New succeeds while the daemon is down.
func New(host string) (*Docker, error) {
	if host == "" {
		host = DefaultHost
	}
	cli, err := client.New(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Docker{cli: cli}, nil
}

// Close releases the underlying connections.
func (d *Docker) Close() error { return d.cli.Close() }

// Ping implements runtime.Runtime.
func (d *Docker) Ping(ctx context.Context) (runtime.Info, error) {
	v, err := d.cli.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return runtime.Info{}, mapErr(err, "")
	}
	return runtime.Info{Name: "docker", Version: v.Version, APIVersion: v.APIVersion, OS: v.Os, Arch: v.Arch}, nil
}

// List implements runtime.Runtime.
func (d *Docker) List(ctx context.Context, project string) ([]runtime.Container, error) {
	f := make(client.Filters).Add("label", runtime.LabelManaged+"=true")
	if project != "" {
		f.Add("label", runtime.LabelProject+"="+project)
	}
	res, err := d.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: f})
	if err != nil {
		return nil, mapErr(err, project)
	}
	out := make([]runtime.Container, 0, len(res.Items))
	for _, s := range res.Items {
		out = append(out, summaryToContainer(s))
	}
	return out, nil
}

func summaryToContainer(s container.Summary) runtime.Container {
	c := runtime.Container{
		ID:      s.ID,
		Image:   s.Image,
		State:   string(s.State),
		Status:  s.Status,
		Project: s.Labels[runtime.LabelProject],
		Service: s.Labels[runtime.LabelService],
		Created: time.Unix(s.Created, 0).UTC(),
		Labels:  s.Labels,
	}
	if len(s.Names) > 0 {
		c.Name = strings.TrimPrefix(s.Names[0], "/")
	}
	for _, p := range s.Ports {
		port := runtime.Port{PrivatePort: p.PrivatePort, PublicPort: p.PublicPort, Type: p.Type}
		if p.IP.IsValid() {
			port.IP = p.IP.String()
		}
		c.Ports = append(c.Ports, port)
	}
	return c
}

// inspect fetches a container and enforces the managed-label rule.
func (d *Docker) inspect(ctx context.Context, id string) (container.InspectResponse, error) {
	res, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return container.InspectResponse{}, mapErr(err, id)
	}
	c := res.Container
	if c.Config == nil || c.Config.Labels[runtime.LabelManaged] != "true" {
		return container.InspectResponse{}, fmt.Errorf("%w: %s", runtime.ErrNotManaged, id)
	}
	return c, nil
}

// Inspect implements runtime.Runtime.
func (d *Docker) Inspect(ctx context.Context, id string) (runtime.ContainerDetail, error) {
	c, err := d.inspect(ctx, id)
	if err != nil {
		return runtime.ContainerDetail{}, err
	}
	det := runtime.ContainerDetail{
		Container: runtime.Container{
			ID:      c.ID,
			Name:    strings.TrimPrefix(c.Name, "/"),
			Image:   c.Config.Image,
			Labels:  c.Config.Labels,
			Project: c.Config.Labels[runtime.LabelProject],
			Service: c.Config.Labels[runtime.LabelService],
			Created: parseTime(c.Created),
		},
		RestartCount: c.RestartCount,
	}
	if c.State != nil {
		det.State = string(c.State.Status)
		det.Status = det.State
		det.StartedAt = parseTime(c.State.StartedAt)
		det.FinishedAt = parseTime(c.State.FinishedAt)
		det.ExitCode = c.State.ExitCode
		if h := c.State.Health; h != nil && h.Status != "none" {
			det.Health = string(h.Status)
		}
	}
	for _, m := range c.Mounts {
		det.Mounts = append(det.Mounts, runtime.Mount{
			Type: string(m.Type), Source: m.Source, Destination: m.Destination, ReadOnly: !m.RW,
		})
	}
	if c.NetworkSettings != nil {
		for name, ep := range c.NetworkSettings.Networks {
			det.Networks = append(det.Networks, name)
			if ep != nil && ep.IPAddress.Is4() {
				if det.IPs == nil {
					det.IPs = make(map[string]string)
				}
				det.IPs[name] = ep.IPAddress.String()
			}
		}
		slices.Sort(det.Networks)
	}
	return det, nil
}

// Start implements runtime.Runtime.
func (d *Docker) Start(ctx context.Context, id string) error {
	if _, err := d.inspect(ctx, id); err != nil {
		return err
	}
	_, err := d.cli.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return mapErr(err, id)
}

// Stop implements runtime.Runtime.
func (d *Docker) Stop(ctx context.Context, id string, timeout time.Duration) error {
	if _, err := d.inspect(ctx, id); err != nil {
		return err
	}
	_, err := d.cli.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: seconds(timeout)})
	return mapErr(err, id)
}

// Restart implements runtime.Runtime.
func (d *Docker) Restart(ctx context.Context, id string, timeout time.Duration) error {
	if _, err := d.inspect(ctx, id); err != nil {
		return err
	}
	_, err := d.cli.ContainerRestart(ctx, id, client.ContainerRestartOptions{Timeout: seconds(timeout)})
	return mapErr(err, id)
}

// Remove implements runtime.Runtime.
func (d *Docker) Remove(ctx context.Context, id string, force bool) error {
	if _, err := d.inspect(ctx, id); err != nil {
		return err
	}
	_, err := d.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: force})
	return mapErr(err, id)
}

// Logs implements runtime.Runtime.
func (d *Docker) Logs(ctx context.Context, id string, opts runtime.LogOptions, fn func(runtime.LogLine) error) error {
	c, err := d.inspect(ctx, id)
	if err != nil {
		return err
	}
	lo := client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     opts.Follow,
		Timestamps: opts.Timestamps,
		Tail:       "all",
	}
	if opts.Tail >= 0 {
		lo.Tail = fmt.Sprint(opts.Tail)
	}
	if !opts.Since.IsZero() {
		lo.Since = fmt.Sprintf("%d.%09d", opts.Since.Unix(), opts.Since.Nanosecond())
	}
	rc, err := d.cli.ContainerLogs(ctx, id, lo)
	if err != nil {
		return mapErr(err, id)
	}
	defer rc.Close()

	if c.Config.Tty {
		// TTY containers emit a single raw stream.
		w := &lineWriter{stream: "stdout", timestamps: opts.Timestamps, fn: fn}
		if _, err := io.Copy(w, rc); err != nil {
			return err
		}
		return w.flush()
	}
	stdout := &lineWriter{stream: "stdout", timestamps: opts.Timestamps, fn: fn}
	stderr := &lineWriter{stream: "stderr", timestamps: opts.Timestamps, fn: fn}
	if _, err := stdcopy.StdCopy(stdout, stderr, rc); err != nil {
		return err
	}
	if err := stdout.flush(); err != nil {
		return err
	}
	return stderr.flush()
}

// PruneProject implements runtime.Runtime.
func (d *Docker) PruneProject(ctx context.Context, project string, volumes bool) (runtime.PruneReport, error) {
	rep := runtime.PruneReport{KeptVolumes: []string{}}
	if project == "" {
		return rep, errors.New("prune: project is required")
	}
	filters := func() client.Filters {
		return make(client.Filters).
			Add("label", runtime.LabelManaged+"=true").
			Add("label", runtime.LabelProject+"="+project)
	}

	cs, err := d.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters()})
	if err != nil {
		return rep, mapErr(err, project)
	}
	anon := map[string]bool{}
	for _, c := range cs.Items {
		vols, err := d.anonymousVolumes(ctx, c.ID)
		if err != nil {
			return rep, err
		}
		_, err = d.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: volumes})
		if gone, err := removed(err, c.ID); err != nil {
			return rep, err
		} else if !gone {
			rep.Containers++
		}
		for _, v := range vols {
			anon[v] = true
		}
	}
	names := make([]string, 0, len(anon))
	for v := range anon {
		names = append(names, v)
	}
	slices.Sort(names)
	if !volumes {
		rep.KeptVolumes = names
	}

	ns, err := d.cli.NetworkList(ctx, client.NetworkListOptions{Filters: filters()})
	if err != nil {
		return rep, mapErr(err, project)
	}
	for _, n := range ns.Items {
		_, err := d.cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{})
		if gone, err := removed(err, n.ID); err != nil {
			return rep, err
		} else if !gone {
			rep.Networks++
		}
	}

	if !volumes {
		return rep, nil
	}
	// Docker removes an anonymous volume with its container unless another
	// container still uses it; count only those that are really gone.
	for _, v := range names {
		_, err := d.cli.VolumeInspect(ctx, v, client.VolumeInspectOptions{})
		if gone, err := removed(err, v); err != nil {
			return rep, err
		} else if gone {
			rep.Volumes++
		}
	}
	vs, err := d.cli.VolumeList(ctx, client.VolumeListOptions{Filters: filters()})
	if err != nil {
		return rep, mapErr(err, project)
	}
	for _, v := range vs.Items {
		_, err := d.cli.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true})
		if gone, err := removed(err, v.Name); err != nil {
			return rep, err
		} else if !gone {
			rep.Volumes++
		}
	}
	return rep, nil
}

// anonymousVolumes returns the names of the anonymous volumes mounted into a
// container. A container or volume that no longer exists yields nothing.
func (d *Docker) anonymousVolumes(ctx context.Context, id string) ([]string, error) {
	res, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, mapErr(err, id)
	}
	var names []string
	for _, m := range res.Container.Mounts {
		if m.Type != mount.TypeVolume || m.Name == "" {
			continue
		}
		v, err := d.cli.VolumeInspect(ctx, m.Name, client.VolumeInspectOptions{})
		if cerrdefs.IsNotFound(err) {
			continue
		} else if err != nil {
			return nil, mapErr(err, m.Name)
		}
		if _, ok := v.Volume.Labels[anonymousVolumeLabel]; ok {
			names = append(names, m.Name)
		}
	}
	return names, nil
}

// removed interprets the error of a removal call: gone reports a resource
// that no longer existed (raced), which is not an error and not counted.
func removed(err error, id string) (gone bool, _ error) {
	if cerrdefs.IsNotFound(err) {
		return true, nil
	}
	return false, mapErr(err, id)
}

// Stats implements runtime.Runtime. It takes a single one-shot sample.
func (d *Docker) Stats(ctx context.Context, id string) (runtime.StatsSnapshot, error) {
	if _, err := d.inspect(ctx, id); err != nil {
		return runtime.StatsSnapshot{}, err
	}
	res, err := d.cli.ContainerStats(ctx, id, client.ContainerStatsOptions{})
	if err != nil {
		return runtime.StatsSnapshot{}, mapErr(err, id)
	}
	defer res.Body.Close()
	var raw container.StatsResponse
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		return runtime.StatsSnapshot{}, fmt.Errorf("decode stats of %s: %w", id, err)
	}
	return snapshot(raw), nil
}

// snapshot converts an engine stats record, handling the cgroup v1 and v2
// shapes.
func snapshot(raw container.StatsResponse) runtime.StatsSnapshot {
	s := runtime.StatsSnapshot{
		Read:        raw.Read,
		CPUTotalNs:  raw.CPUStats.CPUUsage.TotalUsage,
		SystemCPUNs: raw.CPUStats.SystemUsage,
		OnlineCPUs:  raw.CPUStats.OnlineCPUs,
		MemUsage:    raw.MemoryStats.Usage,
		MemLimit:    raw.MemoryStats.Limit,
		Pids:        raw.PidsStats.Current,
	}
	if s.OnlineCPUs == 0 {
		s.OnlineCPUs = uint32(len(raw.CPUStats.CPUUsage.PercpuUsage))
	}
	for _, k := range []string{"total_inactive_file", "inactive_file", "cache"} {
		if v, ok := raw.MemoryStats.Stats[k]; ok {
			s.MemInactiveFile = v
			break
		}
	}
	for _, n := range raw.Networks {
		s.NetRx += n.RxBytes
		s.NetTx += n.TxBytes
	}
	for _, e := range raw.BlkioStats.IoServiceBytesRecursive {
		switch strings.ToLower(e.Op) {
		case "read":
			s.BlkRead += e.Value
		case "write":
			s.BlkWrite += e.Value
		}
	}
	return s
}

// lineWriter splits a byte stream into lines and hands them to fn.
type lineWriter struct {
	stream     string
	timestamps bool
	fn         func(runtime.LogLine) error

	buf       []byte
	truncated bool
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := indexNL(p)
		if i < 0 {
			w.add(p)
			break
		}
		w.add(p[:i])
		if err := w.emit(); err != nil {
			return 0, err
		}
		p = p[i+1:]
	}
	return n, nil
}

func indexNL(p []byte) int {
	for i, b := range p {
		if b == '\n' {
			return i
		}
	}
	return -1
}

// add appends to the pending line, dropping bytes beyond maxLineBytes.
func (w *lineWriter) add(p []byte) {
	room := maxLineBytes - len(w.buf)
	if len(p) > room {
		p = p[:max(room, 0)]
		w.truncated = true
	}
	w.buf = append(w.buf, p...)
}

func (w *lineWriter) flush() error {
	if len(w.buf) == 0 && !w.truncated {
		return nil
	}
	return w.emit()
}

func (w *lineWriter) emit() error {
	text := strings.TrimSuffix(string(w.buf), "\r")
	if w.truncated {
		text += truncMarker
	}
	w.buf, w.truncated = w.buf[:0], false
	line := runtime.LogLine{Stream: w.stream, Text: text}
	if w.timestamps {
		if ts, rest, ok := strings.Cut(text, " "); ok {
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				line.Time, line.Text = t.UTC(), rest
			}
		}
	}
	return w.fn(line)
}

// seconds converts a stop timeout; negative means the engine default.
func seconds(d time.Duration) *int {
	if d < 0 {
		return nil
	}
	s := int(d / time.Second)
	return &s
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return time.Time{}
	}
	return t.UTC()
}

// mapErr translates engine errors into runtime sentinels (ErrNotFound,
// ErrUnavailable).
func mapErr(err error, id string) error {
	switch {
	case err == nil:
		return nil
	case cerrdefs.IsNotFound(err):
		return fmt.Errorf("%w: %s", runtime.ErrNotFound, id)
	case client.IsErrConnectionFailed(err):
		return fmt.Errorf("%w: %v", runtime.ErrUnavailable, err)
	}
	return err
}
