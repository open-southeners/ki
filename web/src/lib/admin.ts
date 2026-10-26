// Pure helpers of the admin pages (no React), kept here so they are unit-tested.

import type { AuditEntry } from "./types";

/** Token expiry choices. `duration` is a Go duration string: Go has no "d" unit. */
export const EXPIRY_PRESETS = [
  { id: "30", label: "30 days", duration: "720h" },
  { id: "90", label: "90 days", duration: "2160h" },
  { id: "365", label: "365 days", duration: "8760h" },
  { id: "none", label: "No expiry", duration: "" },
] as const;

export type ExpiryPresetId = (typeof EXPIRY_PRESETS)[number]["id"];

/** The `expires_in` param for a preset, or undefined for "no expiry". */
export function expiryDuration(id: string): string | undefined {
  const p = EXPIRY_PRESETS.find((e) => e.id === id);
  return p && p.duration ? p.duration : undefined;
}

export interface DotenvResult {
  vars: Record<string, string>;
  /** First parse problem, with its 1-based line number; vars is empty then. */
  error?: string;
}

/**
 * Parses a dotenv text with the same rules as `ki-agent projects env import`:
 * NAME=VALUE with an optional "export " prefix, blank and # lines skipped,
 * single quotes literal, double quotes with \n \r \t \" \\ escapes, unquoted
 * values end at an unescaped " #".
 */
export function parseDotenv(text: string): DotenvResult {
  const vars: Record<string, string> = {};
  const lines = text.split(/\r?\n/);
  for (let i = 0; i < lines.length; i++) {
    const n = i + 1;
    let line = (lines[i] ?? "").trim();
    if (n === 1) line = line.replace(/^﻿/, "");
    if (line === "" || line.startsWith("#")) continue;
    if (line.startsWith("export ")) line = line.slice(7).trim();
    const eq = line.indexOf("=");
    const name = eq < 0 ? "" : line.slice(0, eq).trim();
    if (eq < 0 || name === "") return { vars: {}, error: `Line ${n}: expected NAME=VALUE` };
    const v = dotenvValue(line.slice(eq + 1).trim());
    if (typeof v !== "string") return { vars: {}, error: `Line ${n}: ${v.error}` };
    vars[name] = v;
  }
  return { vars };
}

function dotenvValue(v: string): string | { error: string } {
  if (v === "") return "";
  if (v[0] === "'") {
    const end = v.indexOf("'", 1);
    return end < 0 ? { error: "unterminated single quote" } : v.slice(1, end);
  }
  if (v[0] === '"') {
    let out = "";
    for (let i = 1; i < v.length; i++) {
      const c = v[i];
      if (c === '"') return out;
      if (c !== "\\") {
        out += c;
        continue;
      }
      i++;
      if (i >= v.length) return { error: "unterminated double quote" };
      const e = v[i];
      if (e === "n") out += "\n";
      else if (e === "r") out += "\r";
      else if (e === "t") out += "\t";
      else if (e === '"' || e === "\\" || e === "$" || e === "'") out += e;
      else out += "\\" + e;
    }
    return { error: "unterminated double quote" };
  }
  const hash = v.indexOf(" #");
  return (hash >= 0 ? v.slice(0, hash) : v).trim();
}

/** Client-side audit filter: every whitespace-separated term must appear in some field (case-insensitive). */
export function filterAudit(entries: AuditEntry[], query: string): AuditEntry[] {
  const terms = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (terms.length === 0) return entries;
  return entries.filter((e) => {
    const hay = [e.principal_kind, e.principal_name, e.ip, e.action, e.target, e.result, e.time]
      .join("\n")
      .toLowerCase();
    return terms.every((t) => hay.includes(t));
  });
}

/** True when changing `from` to `to` takes admin rights away from the person changed. */
export function losesAdmin(from: string, to: string): boolean {
  return from === "admin" && to !== "admin";
}

/** Audit results: "ok" on success, otherwise an error code or message. */
export function auditFailed(result: string): boolean {
  return result !== "" && result !== "ok" && result !== "success";
}
