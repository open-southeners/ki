package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/ki/internal/rpc"
)

const hostSampleJSON = `{"time":"2026-01-02T03:04:05Z","cpu":{"percent":12.5,"cores":4},
"load":{"1":0.2,"5":0.3,"15":0.25},
"memory":{"total":4294967296,"used":1073741824,"available":3221225472,"percent":25},
"swap":{"total":0,"used":0,"percent":0},
"disks":[{"path":"/","device":"/dev/sda1","fstype":"ext4","total":21474836480,"used":10737418240,"free":10737418240,"percent":50,"inodes_percent":3.2}],
"disk_io":{"read_bytes_per_s":2048,"write_bytes_per_s":null,"read_ops_per_s":1,"write_ops_per_s":null},
"network":{"rx_bytes_per_s":1536,"tx_bytes_per_s":512,"interfaces":[]},"uptime_s":90061}`

func containerJSON(name string, cpu string) string {
	return fmt.Sprintf(`{"time":"2026-01-02T03:04:05Z","id":"abc","name":%q,"project":"app","service":"web","component":"",
"cpu_percent":%s,"memory":{"usage":52428800,"limit":104857600,"percent":50},
"network":{"rx_bytes_per_s":1024,"tx_bytes_per_s":null},"block_io":{"read_bytes_per_s":null,"write_bytes_per_s":null},"pids":3}`, name, cpu)
}

func statsStubs(rec *calls, conts string) func(*rpc.Router) {
	return func(r *rpc.Router) {
		r.Handle(stub(rec, "host.stats", `{"current":`+hostSampleJSON+`,"history":[]}`))
		r.Handle(stub(rec, "containers.stats", `{"available":true,"containers":[`+conts+`]}`))
		sub := func(method, notify, id, payload string) {
			r.Handle(method+".subscribe", func(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
				rec.add(method+".subscribe", params)
				_ = p.Notify(ctx, notify, rpc.Update{Subscription: id, Seq: 1, Data: json.RawMessage(payload)})
				return map[string]string{"subscription": id}, nil
			})
			r.Handle(method+".unsubscribe", func(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
				rec.add(method+".unsubscribe", params)
				return struct{}{}, nil
			})
		}
		sub("host", "host.update", "H1", `{"sample":`+hostSampleJSON+`}`)
		sub("containers.stats", "containers.stats.update", "C1",
			`{"available":true,"containers":[`+containerJSON("idle", "0.5")+`,`+containerJSON("busy", "250")+`]}`)
	}
}

func TestStatsOneShot(t *testing.T) {
	rec := &calls{}
	var conts []string
	for i := 0; i < 12; i++ {
		conts = append(conts, `{"current":`+containerJSON(fmt.Sprintf("c%02d", i), fmt.Sprint(i))+`}`)
	}
	conts = append(conts, `{"current":null}`)
	sock := startAgent(t, statsStubs(rec, strings.Join(conts, ",")))

	out, _, err := runCLI(t, sock, "", "stats")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"12.5% (4 cores)", "load 0.20 0.30 0.25", "up 1d1h",
		"1.0 GiB / 4.0 GiB (25.0%)", "10.0 GiB / 20.0 GiB (50.0%) ext4",
		"read 2.0 KiB/s   write -", "down 1.5 KiB/s   up 512 B/s",
		"NAME", "NET↓/s", "c11", "50.0 MiB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "c01 ") || strings.Contains(out, "c00") {
		t.Errorf("only the top 10 by CPU expected:\n%s", out)
	}
	if strings.Index(out, "c11") > strings.Index(out, "c10") {
		t.Errorf("rows should be sorted by CPU desc:\n%s", out)
	}
	if !strings.Contains(out, "-") {
		t.Errorf("null rates should print -:\n%s", out)
	}

	out, _, err = runCLI(t, sock, "", "stats", "--json")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], `{"current":`) || !strings.HasPrefix(lines[1], `{"available":true`) {
		t.Errorf("json lines = %q", out)
	}
}

func TestContainersStats(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, statsStubs(rec, `{"current":`+containerJSON("web-1", "null")+`}`))

	out, _, err := runCLI(t, sock, "", "containers", "stats", "--project", "app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "web-1") || strings.Contains(out, "load") {
		t.Errorf("output = %q", out)
	}
	if p := rec.last(t, "containers.stats"); p["project"] != "app" {
		t.Errorf("params = %v", p)
	}
	if rec.count("host.stats") != 0 {
		t.Error("containers stats must not query the host")
	}
}

func TestStatsUnavailable(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, func(r *rpc.Router) {
		r.Handle(stub(rec, "containers.stats", `{"available":false,"containers":[]}`))
	})
	out, _, err := runCLI(t, sock, "", "containers", "stats")
	if err != nil || !strings.Contains(out, "unavailable") {
		t.Errorf("out = %q err = %v", out, err)
	}
}

func TestStatsIntervalNeedsWatch(t *testing.T) {
	if _, _, err := runCLI(t, "/nonexistent", "", "stats", "--interval", "2s"); err == nil || !strings.Contains(err.Error(), "--watch") {
		t.Errorf("err = %v", err)
	}
}

func TestStatsWatch(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, statsStubs(rec, ""))
	cmd := newRootCmd()
	var out syncBuffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"agent", "--socket", sock, "stats", "--watch", "--interval", "1500ms"})
	ctx, cancel := context.WithCancel(context.Background())
	cmd.SetContext(ctx)
	errc := make(chan error, 1)
	go func() { errc <- cmd.Execute() }()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "cpu 12.5%") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stats --watch did not stop")
	}
	s := out.String()
	want := "2026-01-02T03:04:05Z cpu 12.5% load 0.20 mem 25.0% disk-io r 2.0 KiB/s w - net ↓1.5 KiB/s ↑512 B/s containers "
	if !strings.HasPrefix(s, want) || strings.Contains(s, "NAME") {
		t.Fatalf("output = %q", s)
	}
	if strings.Contains(s, "\x1b[") {
		t.Error("no escape codes expected when stdout is not a terminal")
	}
	if p := rec.last(t, "host.subscribe"); p["interval_ms"] != float64(1500) {
		t.Errorf("host params = %v", p)
	}
	if p := rec.last(t, "containers.stats.subscribe"); p["interval_ms"] != float64(1500) {
		t.Errorf("containers params = %v", p)
	}
}

func TestStatsWatchUnsubscribes(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, statsStubs(rec, ""))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := startCLI(ctx, sock, "stats", "--watch", "--json")
	// Both updates delivered means both subscriptions are registered client-side.
	waitFor(t, "both updates", func() bool {
		o := r.out.String()
		return strings.Contains(o, `{"sample":`) && strings.Contains(o, `{"available":true`)
	})
	cancel()
	if err := <-r.done; err != nil {
		t.Fatal(err)
	}
	if rec.count("host.unsubscribe") != 1 || rec.count("containers.stats.unsubscribe") != 1 {
		t.Error("expected both unsubscribe calls")
	}
}

func TestContainersStatsWatchLines(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, statsStubs(rec, ""))
	r := startCLI(context.Background(), sock, "containers", "stats", "--watch")
	waitFor(t, "container lines", func() bool { return strings.Count(r.out.String(), "\n") >= 2 })
	want := "2026-01-02T03:04:05Z idle app/web cpu 0.5% mem 50.0 MiB (50.0%) net ↓1.0 KiB/s ↑-\n"
	if !strings.Contains(r.out.String(), want) {
		t.Errorf("output = %q", r.out.String())
	}
}

func TestStatsWatchJSON(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, statsStubs(rec, ""))
	cmd := newRootCmd()
	var out syncBuffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"agent", "--socket", sock, "containers", "stats", "--watch", "--json"})
	ctx, cancel := context.WithCancel(context.Background())
	cmd.SetContext(ctx)
	errc := make(chan error, 1)
	go func() { errc <- cmd.Execute() }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "\n") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-errc
	if !strings.HasPrefix(out.String(), `{"available":true,"containers":[`) {
		t.Errorf("output = %q", out.String())
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0 B"}, {1023, "1023 B"}, {1024, "1.0 KiB"}, {1536, "1.5 KiB"},
		{1 << 20, "1.0 MiB"}, {52428800, "50.0 MiB"}, {1 << 30, "1.0 GiB"},
		{5.5 * (1 << 40), "5.5 TiB"}, {2048 * (1 << 40), "2048.0 TiB"},
	}
	for _, tt := range tests {
		if got := formatBytes(tt.in); got != tt.want {
			t.Errorf("formatBytes(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatRateAndPercent(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	tests := []struct{ got, want string }{
		{formatRate(nil), "-"},
		{formatRate(f(0)), "0 B/s"},
		{formatRate(f(1536)), "1.5 KiB/s"},
		{formatPercent(nil), "-"},
		{formatPercent(f(12.345)), "12.3%"},
		{formatPercent(f(100)), "100.0%"},
		{formatPercentNum(f(0.04)), "0.0"},
		{formatRateNum(nil), "-"},
		{formatRateNum(f(2048)), "2.0 KiB"},
		{formatUptime(45), "45s"},
		{formatUptime(125), "2m5s"},
		{formatUptime(3700), "1h1m"},
		{formatUptime(90061), "1d1h"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
}
