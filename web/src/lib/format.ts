// Display helpers shared by the pages.

const UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

/** 1536 -> "1.5 KiB". null (an unavailable rate) renders as an en dash. */
export function formatBytes(n: number | null | undefined, digits = 1): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return "–";
  let v = Math.abs(n);
  let i = 0;
  while (v >= 1024 && i < UNITS.length - 1) {
    v /= 1024;
    i++;
  }
  const s = i === 0 ? String(Math.round(v)) : v.toFixed(digits);
  return `${n < 0 ? "-" : ""}${s} ${UNITS[i]}`;
}

export function formatRate(n: number | null | undefined): string {
  return n === null || n === undefined ? "–" : `${formatBytes(n)}/s`;
}

export function formatPercent(n: number | null | undefined, digits = 1): string {
  return n === null || n === undefined || !Number.isFinite(n) ? "–" : `${n.toFixed(digits)}%`;
}

/** Seconds -> "3d 4h", "5h 12m", "42s". */
export function formatDuration(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s % 60}s`;
  return `${s}s`;
}

/** "5 minutes ago" / "in 3 hours" using the browser's locale. */
export function formatRelative(iso: string | null | undefined, now: number = Date.now()): string {
  if (!iso) return "–";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "–";
  const diff = (t - now) / 1000;
  const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
  const abs = Math.abs(diff);
  if (abs < 45) return rtf.format(Math.round(diff), "second");
  if (abs < 45 * 60) return rtf.format(Math.round(diff / 60), "minute");
  if (abs < 22 * 3600) return rtf.format(Math.round(diff / 3600), "hour");
  if (abs < 26 * 86400) return rtf.format(Math.round(diff / 86400), "day");
  return formatDateTime(iso);
}

export function formatDateTime(iso: string | null | undefined): string {
  if (!iso) return "–";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "–";
  return d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "medium" });
}

export function shortSha(sha: string | undefined): string {
  return sha ? sha.slice(0, 8) : "–";
}
