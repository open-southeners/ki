// Pure helpers of the operations pages (no React): badge mappings, chart data
// merging and the deploy log buffer. Kept separate so they are unit tested.

import { RpcError } from "@/lib/rpc";
import {
  ErrorCode,
  type Deploy,
  type DeployFailureReason,
  type DeployStatus,
  type DeployUpdate,
  type DeployErrorData,
  type HostSample,
  type Project,
  type ProjectService,
  type ServiceState,
} from "@/lib/types";

export type BadgeTone = "default" | "secondary" | "outline" | "success" | "warning" | "destructive";

export interface BadgeSpec {
  tone: BadgeTone;
  label: string;
}

// ------------------------------------------------------------------ badges

export function serviceStateBadge(state: ServiceState | string): BadgeSpec {
  switch (state) {
    case "running":
      return { tone: "success", label: "running" };
    case "partial":
      return { tone: "warning", label: "partial" };
    case "unhealthy":
      return { tone: "destructive", label: "unhealthy" };
    case "stopped":
      return { tone: "secondary", label: "stopped" };
    case "missing":
      return { tone: "outline", label: "missing" };
    default:
      return { tone: "outline", label: state };
  }
}

export function deployStatusBadge(status: DeployStatus | string): BadgeSpec {
  switch (status) {
    case "succeeded":
      return { tone: "success", label: "succeeded" };
    case "failed":
      return { tone: "destructive", label: "failed" };
    case "running":
      return { tone: "default", label: "running" };
    case "queued":
      return { tone: "warning", label: "queued" };
    case "superseded":
      return { tone: "secondary", label: "superseded" };
    case "interrupted":
      return { tone: "warning", label: "interrupted" };
    default:
      return { tone: "outline", label: status };
  }
}

export function projectStatusBadge(status: Project["status"] | string): BadgeSpec {
  if (status === "ready") return { tone: "success", label: "ready" };
  if (status === "pending_access") return { tone: "warning", label: "pending access" };
  return { tone: "outline", label: status };
}

export function containerHealthBadge(health: string, state: string): BadgeSpec {
  if (health === "healthy") return { tone: "success", label: "healthy" };
  if (health === "unhealthy") return { tone: "destructive", label: "unhealthy" };
  if (health === "starting") return { tone: "warning", label: "starting" };
  if (state === "restarting") return { tone: "destructive", label: "restarting" };
  return { tone: "outline", label: "no check" };
}

// ---------------------------------------------------------------- services

export interface ServiceCounts {
  total: number;
  running: number;
  problem: number;
}

/** Service health counts of a project: running, and unhealthy/partial/missing as "problem". */
export function serviceCounts(services: ProjectService[]): ServiceCounts {
  let running = 0;
  let problem = 0;
  for (const s of services) {
    if (s.state === "running") running++;
    else if (s.state === "unhealthy" || s.state === "partial" || s.state === "missing") problem++;
  }
  return { total: services.length, running, problem };
}

export function containersRunning(s: ProjectService): { running: number; total: number } {
  return { running: s.containers.filter((c) => c.state === "running").length, total: s.containers.length };
}

/** Public URLs of a service's exposure (https, with the path when set). */
export function exposureUrls(s: ProjectService): string[] {
  const e = s.exposure;
  if (!e) return [];
  const path = e.path && e.path !== "/" ? (e.path.startsWith("/") ? e.path : `/${e.path}`) : "";
  return e.domains.map((d) => `https://${d}${path}`);
}

// ------------------------------------------------------------ host charts

export interface HostPoint {
  /** Epoch ms. */
  t: number;
  cpu: number | null;
  mem: number;
  rx: number | null;
  tx: number | null;
  read: number | null;
  write: number | null;
}

export const HOST_WINDOW_MS = 60 * 60 * 1000;

export function hostPoint(s: HostSample): HostPoint {
  return {
    t: Date.parse(s.time),
    cpu: s.cpu.percent,
    mem: s.memory.percent,
    rx: s.network.rx_bytes_per_s,
    tx: s.network.tx_bytes_per_s,
    read: s.disk_io.read_bytes_per_s,
    write: s.disk_io.write_bytes_per_s,
  };
}

/**
 * Merges a history (oldest first) with live samples into one ascending series:
 * duplicates by timestamp collapse (the later one wins) and points older than
 * the window, measured from the newest point, are dropped.
 */
export function mergeHostSeries(
  history: HostSample[],
  live: HostSample[],
  windowMs: number = HOST_WINDOW_MS,
): HostPoint[] {
  const byTime = new Map<number, HostPoint>();
  for (const s of [...history, ...live]) {
    const p = hostPoint(s);
    if (Number.isNaN(p.t)) continue;
    byTime.set(p.t, p);
  }
  const all = [...byTime.values()].sort((a, b) => a.t - b.t);
  const newest = all[all.length - 1];
  if (!newest) return all;
  const cutoff = newest.t - windowMs;
  return all.filter((p) => p.t >= cutoff);
}

/** Appends one live sample to an existing series (same rules as mergeHostSeries). */
export function appendHostSample(series: HostPoint[], s: HostSample, windowMs: number = HOST_WINDOW_MS): HostPoint[] {
  const p = hostPoint(s);
  if (Number.isNaN(p.t)) return series;
  const last = series[series.length - 1];
  let next: HostPoint[];
  if (!last || p.t > last.t) next = [...series, p];
  else next = [...series.filter((x) => x.t !== p.t), p].sort((a, b) => a.t - b.t);
  const cutoff = (next[next.length - 1]?.t ?? p.t) - windowMs;
  return next[0] && next[0].t < cutoff ? next.filter((x) => x.t >= cutoff) : next;
}

// ------------------------------------------------------------- deploy log

export interface LogEntry {
  /** Stored line number; absent for lines streamed before they are stored. */
  seq?: number;
  stream: "stdout" | "stderr" | "agent";
  text: string;
  /** A marker inserted where the stream lost lines. */
  gap?: boolean;
}

export interface DeployLogState {
  lines: LogEntry[];
  /** Highest stored line number seen. */
  lastSeq: number;
  /** Lines dropped from the front because of the cap. */
  dropped: number;
  deploy: Deploy | null;
}

export const MAX_LOG_LINES = 5000;

export const emptyDeployLog: DeployLogState = { lines: [], lastSeq: 0, dropped: 0, deploy: null };

/**
 * Folds one deploys.update payload into the log. Lines with a stored number at
 * or below the highest seen are replays after a re-subscribe and are ignored.
 * The buffer keeps the newest MAX_LOG_LINES lines.
 */
export function reduceDeployLog(state: DeployLogState, u: DeployUpdate, max: number = MAX_LOG_LINES): DeployLogState {
  if (u.type === "status") return { ...state, deploy: u.deploy };
  if (u.seq !== undefined && u.seq <= state.lastSeq) return state;
  const lines = [...state.lines, { seq: u.seq, stream: u.stream, text: u.text }];
  return capLog({ ...state, lines, lastSeq: u.seq ?? state.lastSeq }, max);
}

/** Records a hole in the stream (lines lost to a slow connection). */
export function markDeployLogGap(state: DeployLogState, max: number = MAX_LOG_LINES): DeployLogState {
  return capLog(
    { ...state, lines: [...state.lines, { stream: "agent", text: "-- some log lines were skipped --", gap: true }] },
    max,
  );
}

function capLog(state: DeployLogState, max: number): DeployLogState {
  const over = state.lines.length - max;
  if (over <= 0) return state;
  return { ...state, lines: state.lines.slice(over), dropped: state.dropped + over };
}

// ------------------------------------------------------------------ errors

const FAILURE_TEXT: Record<DeployFailureReason, string> = {
  fetch_failed: "The repository could not be fetched.",
  compose_not_found: "No compose file was found in the repository.",
  compose_invalid: "The compose file is invalid.",
  policy_violation: "The compose file violates the agent's policy.",
  build_failed: "An image failed to build.",
  up_failed: "Containers could not be started.",
  health_failed: "A service did not become healthy in time.",
  runtime_unavailable: "The container runtime is unavailable.",
  domain_conflict: "A domain is already used by another project.",
  internal: "Internal error.",
};

export function failureReasonText(reason: string): string {
  return FAILURE_TEXT[reason as DeployFailureReason] ?? reason.replace(/_/g, " ");
}

const TRIGGER_TEXT: Record<string, string> = {
  project_not_ready: "The project is not ready: the agent cannot read the repository yet.",
  ref_not_allowed: "That ref is not allowed by the project's trigger settings.",
  ref_not_found: "That ref does not exist in the repository.",
  no_rollback_target: "There is no earlier successful deploy to roll back to.",
};

/** Friendly text for a -32012 deploy error; null when `e` is not one. */
export function deployErrorText(e: unknown): string | null {
  if (!(e instanceof RpcError) || e.code !== ErrorCode.Deploy) return null;
  const d = e.data as Partial<DeployErrorData> | undefined;
  const base = d?.reason ? TRIGGER_TEXT[d.reason] : undefined;
  if (!base) return d?.detail || e.message;
  return d?.detail && !base.includes(d.detail) ? `${base} (${d.detail})` : base;
}

// --------------------------------------------------------------- rollback

/** Deploys that can be a rollback target: succeeded, with a commit. Newest first. */
export function rollbackTargets(deploys: Deploy[]): Deploy[] {
  return deploys.filter((d) => d.status === "succeeded" && d.commit).sort((a, b) => b.id - a.id);
}

/** The deploy "previous good" resolves to: the second-newest succeeded one. */
export function previousGood(deploys: Deploy[]): Deploy | undefined {
  return rollbackTargets(deploys)[1];
}
