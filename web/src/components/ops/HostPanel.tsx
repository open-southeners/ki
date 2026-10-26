import { useQuery } from "@tanstack/react-query";
import * as React from "react";
import { Area, AreaChart, CartesianGrid, XAxis, YAxis } from "recharts";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { chartColors, ChartFrame, ChartTooltip } from "@/components/ui/chart";
import { subscribeHost } from "@/lib/api";
import { formatBytes, formatDuration, formatPercent, formatRate } from "@/lib/format";
import { useApi, useSubscription } from "@/lib/rpc-context";
import type { HostSample } from "@/lib/types";
import { ErrorState, Stat } from "./common";
import { appendHostSample, mergeHostSeries, type HostPoint } from "./logic";

const LIVE_INTERVAL_MS = 2000;

/** 1 h of history (host.stats) plus live samples (host.subscribe) as one series. */
function useHostSeries() {
  const api = useApi();
  const history = useQuery({
    queryKey: ["host", "history"],
    queryFn: () => api.host.stats({ history: true }),
    staleTime: 60_000,
  });
  const [live, setLive] = React.useState<HostSample | null>(null);
  const [extra, setExtra] = React.useState<HostPoint[]>([]);

  useSubscription(
    (rpc) =>
      subscribeHost(
        rpc,
        { interval_ms: LIVE_INTERVAL_MS },
        {
          onData: (s) => {
            setLive(s);
            setExtra((prev) => appendHostSample(prev, s));
          },
        },
      ),
    [],
  );

  const series = React.useMemo(() => {
    const base = mergeHostSeries(history.data?.history ?? [], []);
    if (extra.length === 0) return base;
    const seen = new Set(base.map((p) => p.t));
    const merged = [...base, ...extra.filter((p) => !seen.has(p.t))].sort((a, b) => a.t - b.t);
    const cutoff = (merged[merged.length - 1]?.t ?? 0) - 3600_000;
    return merged.filter((p) => p.t >= cutoff);
  }, [history.data, extra]);

  const current = live ?? history.data?.current ?? null;
  return { series, current, history };
}

/** Compact byte-rate tick: "0", "928K", "3.6M" (unit shown in the chart title). */
export function compactRate(v: number): string {
  if (!Number.isFinite(v) || v <= 0) return "0";
  const units = ["", "K", "M", "G", "T"];
  let n = v;
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n >= 100 || i === 0 ? Math.round(n) : n.toFixed(1).replace(/\.0$/, "")}${units[i]}`;
}

const timeTick = (t: number) => new Date(t).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });

function SeriesChart({
  title,
  data,
  series,
  yFormat,
  tooltipFormat,
  domain,
  label,
}: {
  title: string;
  data: HostPoint[];
  series: Array<{ key: keyof HostPoint; name: string; color: string }>;
  yFormat: (v: number) => string;
  tooltipFormat?: (v: number) => string;
  domain?: [number, number];
  label: string;
}) {
  return (
    <Card className="min-w-0">
      <CardHeader className="flex-row items-center justify-between gap-2 p-4 pb-1">
        <CardTitle className="label-mono whitespace-nowrap text-[11px] font-medium">{title}</CardTitle>
        <div className="flex gap-3 font-mono text-[11px] text-muted-foreground">
          {series.map((s) => (
            <span key={s.key} className="inline-flex items-center gap-1">
              <span className="size-2 rounded-[2px]" style={{ background: s.color }} />
              {s.name}
            </span>
          ))}
        </div>
      </CardHeader>
      <CardContent className="px-2 pb-3">
        <ChartFrame label={label}>
          <AreaChart data={data} margin={{ top: 10, right: 8, bottom: 0, left: 0 }}>
            <CartesianGrid stroke={chartColors.grid} vertical={false} />
            <XAxis
              dataKey="t"
              type="number"
              scale="time"
              domain={["dataMin", "dataMax"]}
              tickFormatter={timeTick}
              tick={{ fill: chartColors.axis, fontSize: 10, fontFamily: "var(--font-mono)" }}
              stroke={chartColors.grid}
              minTickGap={48}
            />
            <YAxis
              width={44}
              interval="preserveStartEnd"
              domain={domain ?? [0, "auto"]}
              tickFormatter={yFormat}
              tick={{ fill: chartColors.axis, fontSize: 10, fontFamily: "var(--font-mono)" }}
              stroke={chartColors.grid}
              allowDecimals={false}
            />
            <ChartTooltip format={(v) => (tooltipFormat ?? yFormat)(v)} labelFormat={(l) => new Date(Number(l)).toLocaleTimeString()} />
            {series.map((s) => (
              <Area
                key={s.key}
                type="monotone"
                dataKey={s.key}
                name={s.name}
                stroke={s.color}
                fill={s.color}
                fillOpacity={0.12}
                strokeWidth={1.5}
                dot={false}
                isAnimationActive={false}
                connectNulls
              />
            ))}
          </AreaChart>
        </ChartFrame>
      </CardContent>
    </Card>
  );
}

/** Host charts (CPU, memory, network, disk IO) and the load/swap/disk/uptime tiles. */
export function HostPanel() {
  const { series, current, history } = useHostSeries();

  if (history.isError && !current) {
    return (
      <Card>
        <ErrorState title="Host stats unavailable" error={history.error} onRetry={() => void history.refetch()} />
      </Card>
    );
  }

  const root = current?.disks.find((d) => d.path === "/") ?? current?.disks[0];

  return (
    <section aria-label="Host" className="grid gap-4">
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        {current ? (
          <>
            <Stat
              label="Load 1 / 5 / 15"
              value={`${current.load["1"].toFixed(2)} ${current.load["5"].toFixed(2)} ${current.load["15"].toFixed(2)}`}
              sub={`${current.cpu.cores} cores`}
              className="col-span-2 md:col-span-1"
            />
            <Stat
              label="Swap"
              value={current.swap.total > 0 ? formatPercent(current.swap.percent) : "none"}
              sub={current.swap.total > 0 ? `${formatBytes(current.swap.used)} / ${formatBytes(current.swap.total)}` : undefined}
            />
            <Stat
              label={root ? `Disk ${root.path}` : "Disk"}
              value={root ? formatPercent(root.percent) : "–"}
              sub={root ? `${formatBytes(root.used)} / ${formatBytes(root.total)}` : undefined}
            />
            <Stat label="Uptime" value={formatDuration(current.uptime_s)} sub={`memory ${formatPercent(current.memory.percent)}`} />
          </>
        ) : (
          Array.from({ length: 4 }, (_, i) => <Skeleton key={i} className="h-[74px]" />)
        )}
      </div>

      {history.isPending && series.length === 0 ? (
        <div className="grid gap-4 md:grid-cols-2" role="status" aria-label="Loading charts">
          {Array.from({ length: 4 }, (_, i) => (
            <Skeleton key={i} className="h-[190px]" />
          ))}
        </div>
      ) : (
        <div className="grid gap-4 md:grid-cols-2">
          <SeriesChart
            title="CPU"
            label="CPU usage over the last hour"
            data={series}
            series={[{ key: "cpu", name: "cpu", color: chartColors.a }]}
            yFormat={(v) => `${Math.round(v)}%`}
            domain={[0, 100]}
          />
          <SeriesChart
            title="Memory"
            label="Memory usage over the last hour"
            data={series}
            series={[{ key: "mem", name: "used", color: chartColors.b }]}
            yFormat={(v) => `${Math.round(v)}%`}
            domain={[0, 100]}
          />
          <SeriesChart
            title="Network · bytes/s"
            label="Network throughput over the last hour"
            data={series}
            series={[
              { key: "rx", name: "rx", color: chartColors.a },
              { key: "tx", name: "tx", color: chartColors.b },
            ]}
            yFormat={compactRate}
            tooltipFormat={formatRate}
          />
          <SeriesChart
            title="Disk IO · bytes/s"
            label="Disk throughput over the last hour"
            data={series}
            series={[
              { key: "read", name: "read", color: chartColors.a },
              { key: "write", name: "write", color: chartColors.b },
            ]}
            yFormat={compactRate}
            tooltipFormat={formatRate}
          />
        </div>
      )}
    </section>
  );
}
