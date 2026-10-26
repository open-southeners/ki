import * as React from "react";
import { ResponsiveContainer, Tooltip } from "recharts";
import { cn } from "@/lib/utils";

/** Chart palette: CSS variables so both themes work. */
export const chartColors = {
  a: "var(--primary)",
  b: "var(--warning)",
  c: "var(--success)",
  grid: "var(--border)",
  axis: "var(--muted-foreground)",
} as const;

/** Fixed-height responsive frame for a Recharts chart. */
export function ChartFrame({
  className,
  height = 140,
  children,
  label,
}: {
  className?: string;
  height?: number;
  children: React.ReactElement;
  /** Accessible description of the chart. */
  label?: string;
}) {
  return (
    <div role="img" aria-label={label} className={cn("w-full text-xs", className)} style={{ height }}>
      <ResponsiveContainer width="100%" height="100%">
        {children}
      </ResponsiveContainer>
    </div>
  );
}

interface TooltipItem {
  dataKey?: unknown;
  color?: string;
  name?: unknown;
  value?: unknown;
}

/** Tooltip matching the console style; `format` renders each series value. */
export function ChartTooltip({
  format,
  labelFormat,
}: {
  format?: (v: number, key: string) => string;
  labelFormat?: (l: unknown) => string;
}) {
  return (
    <Tooltip
      cursor={{ stroke: "var(--border)" }}
      content={(p) => {
        const items = (p.payload ?? []) as unknown as TooltipItem[];
        if (!p.active || items.length === 0) return null;
        return (
          <div className="rounded-md border bg-popover px-2.5 py-1.5 font-mono text-[11px] text-popover-foreground">
            <div className="mb-1 text-muted-foreground">{labelFormat ? labelFormat(p.label) : String(p.label ?? "")}</div>
            {items.map((it) => (
              <div key={String(it.dataKey)} className="flex items-center gap-2">
                <span className="size-2 rounded-[2px]" style={{ background: it.color }} />
                <span>{String(it.name ?? "")}</span>
                <span className="ml-auto pl-3">
                  {typeof it.value === "number" ? (format ? format(it.value, String(it.dataKey)) : it.value) : "–"}
                </span>
              </div>
            ))}
          </div>
        );
      }}
    />
  );
}
