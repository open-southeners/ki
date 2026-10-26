package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-southeners/ki-agent/internal/client"
)

// Method and notification names of the stats streams.
const (
	methodHostStats          = "host.stats"
	methodHostSubscribe      = "host.subscribe"
	notifyHostUpdate         = "host.update"
	methodContainerStats     = "containers.stats"
	methodContainerSubscribe = "containers.stats.subscribe"
	notifyContainerUpdate    = "containers.stats.update"

	topContainers = 10
	clearScreen   = "\x1b[H\x1b[2J"
)

type hostSample struct {
	Time string `json:"time"`
	CPU  struct {
		Percent *float64 `json:"percent"`
		Cores   int      `json:"cores"`
	} `json:"cpu"`
	Load   map[string]float64 `json:"load"`
	Memory struct {
		Total   uint64   `json:"total"`
		Used    uint64   `json:"used"`
		Percent *float64 `json:"percent"`
	} `json:"memory"`
	Swap struct {
		Total   uint64   `json:"total"`
		Used    uint64   `json:"used"`
		Percent *float64 `json:"percent"`
	} `json:"swap"`
	Disks []struct {
		Path    string   `json:"path"`
		FSType  string   `json:"fstype"`
		Total   uint64   `json:"total"`
		Used    uint64   `json:"used"`
		Percent *float64 `json:"percent"`
	} `json:"disks"`
	DiskIO struct {
		Read  *float64 `json:"read_bytes_per_s"`
		Write *float64 `json:"write_bytes_per_s"`
	} `json:"disk_io"`
	Network struct {
		Rx *float64 `json:"rx_bytes_per_s"`
		Tx *float64 `json:"tx_bytes_per_s"`
	} `json:"network"`
	UptimeS int64 `json:"uptime_s"`
}

type containerSample struct {
	Time       string   `json:"time"`
	Name       string   `json:"name"`
	Project    string   `json:"project"`
	Service    string   `json:"service"`
	CPUPercent *float64 `json:"cpu_percent"`
	Memory     struct {
		Usage   uint64   `json:"usage"`
		Percent *float64 `json:"percent"`
	} `json:"memory"`
	Network struct {
		Rx *float64 `json:"rx_bytes_per_s"`
		Tx *float64 `json:"tx_bytes_per_s"`
	} `json:"network"`
}

func newStatsCmd(f *flags) *cobra.Command {
	var (
		watch    bool
		interval time.Duration
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show host resource usage and the busiest containers",
		Long: `Show host CPU, load, memory, swap, disks and I/O rates, plus the ten
containers using the most CPU. With --watch the view refreshes live until Ctrl-C.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStats(cmd, f, statsOpts{host: true, watch: watch, interval: interval, asJSON: asJSON})
		},
	}
	addStatsFlags(cmd, &watch, &interval, &asJSON)
	return cmd
}

func newContainersCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "containers",
		Short: "Inspect managed containers",
	}
	var (
		project  string
		watch    bool
		interval time.Duration
		asJSON   bool
	)
	stats := &cobra.Command{
		Use:   "stats",
		Short: "Show per-container resource usage",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStats(cmd, f, statsOpts{project: project, watch: watch, interval: interval, asJSON: asJSON})
		},
	}
	stats.Flags().StringVar(&project, "project", "", "only containers of this project")
	addStatsFlags(stats, &watch, &interval, &asJSON)
	cmd.AddCommand(stats)
	return cmd
}

func addStatsFlags(cmd *cobra.Command, watch *bool, interval *time.Duration, asJSON *bool) {
	cmd.Flags().BoolVar(watch, "watch", false, "refresh live until Ctrl-C")
	cmd.Flags().DurationVar(interval, "interval", 0, "refresh interval with --watch (default: the agent's, 2s host / 5s containers)")
	cmd.Flags().BoolVar(asJSON, "json", false, "print the raw results or updates as JSON lines")
}

type statsOpts struct {
	host     bool // include the host summary and limit containers to the top 10
	project  string
	watch    bool
	interval time.Duration
	asJSON   bool
}

func (o statsOpts) params() map[string]any {
	p := map[string]any{}
	if o.project != "" {
		p["project"] = o.project
	}
	if o.interval > 0 {
		p["interval_ms"] = o.interval.Milliseconds()
	}
	return p
}

// statsView holds the latest data of a (possibly live) stats display.
type statsView struct {
	mu         sync.Mutex
	host       *hostSample
	available  bool
	haveConts  bool
	containers []containerSample
	top        bool
}

func runStats(cmd *cobra.Command, f *flags, o statsOpts) error {
	if o.interval < 0 || (o.interval > 0 && !o.watch) {
		return errors.New("--interval needs a positive duration and --watch")
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c, err := connect(cmd, f)
	if err != nil {
		return err
	}
	defer c.Close()

	out := cmd.OutOrStdout()
	if o.watch {
		return watchStats(ctx, c, out, o)
	}

	v := &statsView{top: o.host}
	if o.host {
		var res json.RawMessage
		if err := c.Call(ctx, methodHostStats, nil, &res); err != nil {
			return explainError(err)
		}
		if o.asJSON {
			if err := jsonLine(out, res); err != nil {
				return err
			}
		} else {
			var r struct {
				Current *hostSample `json:"current"`
			}
			if err := json.Unmarshal(res, &r); err != nil {
				return err
			}
			v.host = r.Current
		}
	}
	var res json.RawMessage
	if err := c.Call(ctx, methodContainerStats, o.params(), &res); err != nil {
		return explainError(err)
	}
	if o.asJSON {
		return jsonLine(out, res)
	}
	var r struct {
		Available  bool `json:"available"`
		Containers []struct {
			Current *containerSample `json:"current"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return err
	}
	v.available, v.haveConts = r.Available, true
	for _, e := range r.Containers {
		if e.Current != nil {
			v.containers = append(v.containers, *e.Current)
		}
	}
	return v.render(out, o.host)
}

func watchStats(ctx context.Context, c *client.Client, out io.Writer, o statsOpts) error {
	v := &statsView{top: o.host}
	tty := isTerminal(out)
	// draw must be called with v.mu held. On a terminal it redraws the whole
	// view; otherwise it prints one line per update (see hostLine, containerLines).
	draw := func(host bool) {
		switch {
		case tty:
			fmt.Fprint(out, clearScreen)
			_ = v.render(out, o.host)
		case host:
			fmt.Fprintln(out, v.hostLine())
		case !o.host:
			for _, l := range v.containerLines() {
				fmt.Fprintln(out, l)
			}
		}
	}

	var subs []*client.Subscription
	closeAll := func() {
		for _, s := range subs {
			uctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = s.Close(uctx)
			cancel()
		}
	}
	if o.host {
		hp := map[string]any{}
		if o.interval > 0 {
			hp["interval_ms"] = o.interval.Milliseconds()
		}
		s, err := c.Subscribe(ctx, methodHostSubscribe, hp, notifyHostUpdate,
			func(_ int64, data json.RawMessage) bool {
				if len(data) == 0 {
					return true
				}
				if o.asJSON {
					_ = jsonLine(out, data)
					return true
				}
				var u struct {
					Sample *hostSample `json:"sample"`
				}
				if json.Unmarshal(data, &u) != nil {
					return true
				}
				v.mu.Lock()
				defer v.mu.Unlock()
				v.host = u.Sample
				draw(true)
				return true
			})
		if err != nil {
			return explainError(err)
		}
		subs = append(subs, s)
	}
	s, err := c.Subscribe(ctx, methodContainerSubscribe, o.params(), notifyContainerUpdate,
		func(_ int64, data json.RawMessage) bool {
			if len(data) == 0 {
				return true
			}
			if o.asJSON {
				_ = jsonLine(out, data)
				return true
			}
			var u struct {
				Available  bool              `json:"available"`
				Containers []containerSample `json:"containers"`
			}
			if json.Unmarshal(data, &u) != nil {
				return true
			}
			v.mu.Lock()
			defer v.mu.Unlock()
			v.available, v.haveConts, v.containers = u.Available, true, u.Containers
			draw(false)
			return true
		})
	if err != nil {
		closeAll()
		return explainError(err)
	}
	subs = append(subs, s)

	select {
	case <-ctx.Done():
		closeAll()
		return nil
	case <-c.Done():
		return errors.New("connection to the agent was lost")
	}
}

func sampleTime(t string) string {
	if t == "" {
		return time.Now().UTC().Format(time.RFC3339)
	}
	return t
}

// hostLine is the one-line form of a live `stats` update.
func (v *statsView) hostLine() string {
	h := v.host
	if h == nil {
		return sampleTime("") + " no host sample"
	}
	line := fmt.Sprintf("%s cpu %s load %.2f mem %s disk-io r %s w %s net ↓%s ↑%s containers %d",
		sampleTime(h.Time), formatPercent(h.CPU.Percent), h.Load["1"], formatPercent(h.Memory.Percent),
		formatRate(h.DiskIO.Read), formatRate(h.DiskIO.Write), formatRate(h.Network.Rx), formatRate(h.Network.Tx),
		len(v.containers))
	var top *containerSample
	for i, c := range v.containers {
		if c.CPUPercent != nil && (top == nil || *c.CPUPercent > *top.CPUPercent) {
			top = &v.containers[i]
		}
	}
	if top != nil {
		line += fmt.Sprintf(" top %s %s", top.Name, formatPercent(top.CPUPercent))
	}
	return line
}

// containerLines are the one-line-per-container form of a live `containers stats` update.
func (v *statsView) containerLines() []string {
	var lines []string
	for _, c := range v.containers {
		lines = append(lines, fmt.Sprintf("%s %s %s/%s cpu %s mem %s (%s) net ↓%s ↑%s",
			sampleTime(c.Time), c.Name, dash(c.Project), dash(c.Service), formatPercent(c.CPUPercent),
			formatBytes(float64(c.Memory.Usage)), formatPercent(c.Memory.Percent),
			formatRate(c.Network.Rx), formatRate(c.Network.Tx)))
	}
	return lines
}

// isTerminal reports whether w is a character device (an interactive terminal).
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func jsonLine(w io.Writer, raw json.RawMessage) error {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return err
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

func (v *statsView) render(w io.Writer, withHost bool) error {
	if withHost {
		renderHost(w, v.host)
		fmt.Fprintln(w)
	}
	if !v.haveConts {
		fmt.Fprintln(w, "Containers: no sample yet")
		return nil
	}
	if !v.available {
		fmt.Fprintln(w, "Containers: the container engine is unavailable")
		return nil
	}
	list := append([]containerSample(nil), v.containers...)
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i].CPUPercent, list[j].CPUPercent
		switch {
		case a != nil && b != nil && *a != *b:
			return *a > *b
		case (a == nil) != (b == nil):
			return a != nil
		}
		return list[i].Name < list[j].Name
	})
	if v.top && len(list) > topContainers {
		list = list[:topContainers]
	}
	if len(list) == 0 {
		fmt.Fprintln(w, "No managed containers.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPROJECT\tSERVICE\tCPU%\tMEM\tMEM%\tNET↓/s\tNET↑/s")
	for _, c := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			c.Name, dash(c.Project), dash(c.Service), formatPercentNum(c.CPUPercent),
			formatBytes(float64(c.Memory.Usage)), formatPercentNum(c.Memory.Percent),
			formatRateNum(c.Network.Rx), formatRateNum(c.Network.Tx))
	}
	return tw.Flush()
}

func renderHost(w io.Writer, h *hostSample) {
	if h == nil {
		fmt.Fprintln(w, "Host: no sample yet")
		return
	}
	fmt.Fprintf(w, "CPU      %s (%d cores)   load %.2f %.2f %.2f   up %s\n",
		formatPercent(h.CPU.Percent), h.CPU.Cores, h.Load["1"], h.Load["5"], h.Load["15"],
		formatUptime(h.UptimeS))
	fmt.Fprintf(w, "Memory   %s / %s (%s)   swap %s / %s (%s)\n",
		formatBytes(float64(h.Memory.Used)), formatBytes(float64(h.Memory.Total)), formatPercent(h.Memory.Percent),
		formatBytes(float64(h.Swap.Used)), formatBytes(float64(h.Swap.Total)), formatPercent(h.Swap.Percent))
	for _, d := range h.Disks {
		fmt.Fprintf(w, "Disk     %s %s / %s (%s) %s\n", d.Path,
			formatBytes(float64(d.Used)), formatBytes(float64(d.Total)), formatPercent(d.Percent), d.FSType)
	}
	fmt.Fprintf(w, "Disk I/O read %s   write %s\n", formatRate(h.DiskIO.Read), formatRate(h.DiskIO.Write))
	fmt.Fprintf(w, "Network  down %s   up %s\n", formatRate(h.Network.Rx), formatRate(h.Network.Tx))
}

// formatBytes renders n with a binary unit and one decimal (plain bytes are
// whole numbers).
func formatBytes(n float64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", int64(n))
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	n /= 1024
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}

// formatRate renders a bytes-per-second rate; nil (unknown) is "-".
func formatRate(r *float64) string {
	if r == nil {
		return "-"
	}
	return formatBytes(*r) + "/s"
}

// formatRateNum is formatRate for table cells (the header already says /s).
func formatRateNum(r *float64) string {
	if r == nil {
		return "-"
	}
	return formatBytes(*r)
}

// formatPercent renders a percentage with one decimal; nil is "-".
func formatPercent(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", *p)
}

// formatPercentNum is formatPercent for table cells (the header has the %).
func formatPercentNum(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *p)
}

func formatUptime(s int64) string {
	d := time.Duration(s) * time.Second
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", d/(24*time.Hour), d%(24*time.Hour)/time.Hour)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", d/time.Hour, d%time.Hour/time.Minute)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%ds", d/time.Minute, d%time.Minute/time.Second)
	}
	return fmt.Sprintf("%ds", s)
}
