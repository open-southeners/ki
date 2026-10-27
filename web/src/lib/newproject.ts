// Pure helpers for the New project dialog and the Update image action.

import type { GitProjectCreateParams, ImageSpec } from "./types";

const SLUG_RE = /^[a-z0-9][a-z0-9_-]{0,62}$/;
const ENV_KEY_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;
const TAG_RE = /^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$/;

export function validSlug(s: string): boolean {
  return SLUG_RE.test(s);
}

/**
 * Error text for an image reference, or null when it is acceptable: a tag or
 * digest is required and `latest` is refused.
 */
export function imageRefError(ref: string): string | null {
  const r = ref.trim();
  if (r === "") return "Enter an image reference";
  if (/\s/.test(r)) return "An image reference cannot contain spaces";
  const at = r.indexOf("@");
  if (at >= 0) {
    return at > 0 && /^[a-z0-9]+:[a-fA-F0-9]{32,}$/.test(r.slice(at + 1)) ? null : "Invalid digest (expected name@sha256:…)";
  }
  const slash = r.lastIndexOf("/");
  const colon = r.lastIndexOf(":");
  if (colon <= slash) return "Pin a tag or digest, e.g. ghcr.io/acme/web:1.4.2";
  const tag = r.slice(colon + 1);
  if (colon === 0 || !TAG_RE.test(tag)) return "Invalid tag";
  if (tag === "latest") return "Pin a tag or digest; latest is not allowed";
  return null;
}

/** Error for a bare tag (Update image): not empty, valid, not latest. */
export function tagError(tag: string): string | null {
  const t = tag.trim();
  if (t === "") return "Enter a tag";
  if (t === "latest") return "Pin a tag; latest is not allowed";
  return TAG_RE.test(t) ? null : "Invalid tag";
}

/** A full reference contains `/` or `@`; anything else is a bare tag (matches the CLI). */
export function isFullRef(s: string): boolean {
  return s.includes("/") || s.includes("@");
}

/** Update image input to the right projects.image.set fields, or an error. */
export function imageSetTarget(input: string): { image: string } | { tag: string } | { error: string } {
  const v = input.trim();
  if (isFullRef(v)) {
    const error = imageRefError(v);
    return error ? { error } : { image: v };
  }
  const error = tagError(v);
  return error ? { error } : { tag: v };
}

export function splitList(s: string): string[] {
  return s
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter(Boolean);
}

export interface EnvRow {
  key: string;
  value: string;
}

export interface VolumeRow {
  name: string;
  path: string;
  readOnly: boolean;
}

/** Env rows to a map; blank-key rows are skipped. See envRowsError for validation. */
export function envRowsToMap(rows: EnvRow[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const r of rows) {
    const k = r.key.trim();
    if (k) out[k] = r.value;
  }
  return out;
}

export function envRowsError(rows: EnvRow[]): string | null {
  const seen = new Set<string>();
  for (const r of rows) {
    const k = r.key.trim();
    if (!k) continue;
    if (!ENV_KEY_RE.test(k)) return `Invalid variable name "${k}"`;
    if (seen.has(k)) return `Duplicate variable "${k}"`;
    seen.add(k);
  }
  return null;
}

/** Parses pasted `KEY=value` lines (comments and blanks skipped). */
export function parseEnvText(text: string): EnvRow[] {
  const rows: EnvRow[] = [];
  for (const line of text.split(/\r?\n/)) {
    const l = line.trim();
    if (!l || l.startsWith("#")) continue;
    const eq = l.indexOf("=");
    if (eq < 0) rows.push({ key: l, value: "" });
    else rows.push({ key: l.slice(0, eq).trim(), value: l.slice(eq + 1) });
  }
  return rows;
}

export function volumeRowsToSpecs(rows: VolumeRow[]): string[] {
  return rows
    .filter((r) => r.name.trim() || r.path.trim())
    .map((r) => `${r.name.trim()}:${r.path.trim()}${r.readOnly ? ":ro" : ""}`);
}

export function volumeRowsError(rows: VolumeRow[]): string | null {
  for (const r of rows) {
    if (!r.name.trim() && !r.path.trim()) continue;
    if (!/^[A-Za-z0-9][A-Za-z0-9_.-]*$/.test(r.name.trim())) return `Volume "${r.name}" must be a named volume (not a path)`;
    if (!r.path.trim().startsWith("/")) return `Volume "${r.name}" needs an absolute container path`;
  }
  return null;
}

export interface ImageForm {
  slug: string;
  image: string;
  service: string;
  port: string;
  domains: string;
  path: string;
  volumes: VolumeRow[];
  env: EnvRow[];
}

/** First client-side problem of an image form, or null. Mirrors the server checks. */
export function imageFormError(f: ImageForm): string | null {
  if (!validSlug(f.slug.trim())) return "Slug: lowercase letters, digits, - and _ (start with a letter or digit)";
  const e = imageRefError(f.image);
  if (e) return e;
  if (f.service.trim() && !validSlug(f.service.trim())) return "Invalid service name";
  const port = f.port.trim();
  if (port) {
    const n = Number(port);
    if (!Number.isInteger(n) || n < 1 || n > 65535) return "Port must be between 1 and 65535";
  }
  if (splitList(f.domains).length > 0 && !port) return "Domains need a port";
  if (f.path.trim() && !port) return "A path needs a port";
  return volumeRowsError(f.volumes) ?? envRowsError(f.env);
}

export function imageFormToParams(f: ImageForm): { slug: string; image: ImageSpec } {
  const image: ImageSpec = { image: f.image.trim() };
  if (f.service.trim()) image.service = f.service.trim();
  if (f.port.trim()) image.port = Number(f.port.trim());
  const domains = splitList(f.domains);
  if (domains.length) image.domains = domains;
  if (f.path.trim()) image.path = f.path.trim();
  const volumes = volumeRowsToSpecs(f.volumes);
  if (volumes.length) image.volumes = volumes;
  const env = envRowsToMap(f.env);
  if (Object.keys(env).length) image.env = env;
  return { slug: f.slug.trim(), image };
}

export interface GitForm {
  repoUrl: string;
  slug: string;
  mode: "branch" | "tag_pattern";
  ref: string;
  composeFile: string;
  token: string;
  tokenUser: string;
}

export function isSshUrl(u: string): boolean {
  return /^(ssh:\/\/|[\w.-]+@[\w.-]+:)/.test(u.trim());
}

export function gitFormError(f: GitForm): string | null {
  const u = f.repoUrl.trim();
  if (!u) return "Enter the repository URL";
  if (!/^(https?:\/\/|ssh:\/\/|[\w.-]+@[\w.-]+:)/.test(u)) return "Use an https:// or SSH repository URL";
  if (f.slug.trim() && !validSlug(f.slug.trim())) return "Invalid slug";
  if (f.token && isSshUrl(u)) return "A token only applies to HTTPS URLs";
  return null;
}

export function gitFormToParams(f: GitForm): GitProjectCreateParams {
  const p: GitProjectCreateParams = { repo_url: f.repoUrl.trim() };
  if (f.slug.trim()) p.slug = f.slug.trim();
  if (f.ref.trim()) p[f.mode] = f.ref.trim();
  if (f.composeFile.trim()) p.compose_file = f.composeFile.trim();
  if (f.token && !isSshUrl(p.repo_url)) {
    p.token = f.token;
    if (f.tokenUser.trim()) p.token_user = f.tokenUser.trim();
  }
  return p;
}

/** Services that declare an `image:`, found by lightly scanning compose YAML (no full parse). */
export function composeImageServices(content: string): Array<{ name: string; image: string }> {
  const out: Array<{ name: string; image: string }> = [];
  let inServices = false;
  let svcIndent = -1;
  let name = "";
  for (const line of content.split(/\r?\n/)) {
    if (/^\s*(#.*)?$/.test(line)) continue;
    const indent = line.length - line.trimStart().length;
    if (indent === 0) {
      inServices = /^services:\s*(#.*)?$/.test(line);
      svcIndent = -1;
      name = "";
      continue;
    }
    if (!inServices) continue;
    if (svcIndent < 0) svcIndent = indent;
    if (indent === svcIndent) {
      const m = /^\s+["']?([A-Za-z0-9][\w.-]*)["']?:\s*(#.*)?$/.exec(line);
      name = m ? m[1]! : "";
      continue;
    }
    const img = /^\s+image:\s*["']?([^\s"'#]+)["']?/.exec(line);
    if (img && name && !out.some((s) => s.name === name)) out.push({ name, image: img[1]! });
  }
  return out;
}
