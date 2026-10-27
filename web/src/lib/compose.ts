// Pure helpers for compose-sourced projects.

import { RpcError } from "./rpc";
import { ErrorCode, type ComposeInvalidData, type PolicyViolation, type Project } from "./types";

/** The deploy ref that deploys a stored compose version. */
export function composeRef(version: number): string {
  return `compose@v${version}`;
}

/** The version in a `compose@vN` ref, or null when `ref` is not one. */
export function parseComposeRef(ref: string): number | null {
  const m = /^compose@v(\d+)$/.exec(ref);
  return m ? Number(m[1]) : null;
}

/** "Compose v3" for a compose project, "Compose" before its first version, "Git" otherwise. */
export function composeLabel(p: Pick<Project, "source" | "compose_version">): string {
  if (p.source !== "compose") return "Git";
  return p.compose_version ? `Compose v${p.compose_version}` : "Compose";
}

/** Detail and violations of a rejected projects.compose.set; null for any other error. */
export function composeInvalid(e: unknown): { detail: string; violations: PolicyViolation[] } | null {
  if (!(e instanceof RpcError) || e.code !== ErrorCode.InvalidParams) return null;
  const d = e.data as Partial<ComposeInvalidData> | undefined;
  if (!d || typeof d !== "object" || (!d.reason && !d.violations)) return null;
  return { detail: d.detail || e.message, violations: Array.isArray(d.violations) ? d.violations : [] };
}
