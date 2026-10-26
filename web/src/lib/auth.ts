// Client for the panel's /auth JSON endpoints (docs/protocol.md, "Sign-in, setup
// links and sessions"). Same-origin: the browser adds the Origin header and the
// HttpOnly session cookie, so nothing secret is handled here.

import type { Role } from "./types";

export interface SessionUser {
  username: string;
  role: Role;
}

export interface SetupInfo {
  username: string;
  role: Role;
  totp_secret: string;
  otpauth_uri: string;
}

/** An /auth call that did not succeed. `message` is the server's error text. */
export class AuthError extends Error {
  readonly status: number;
  /** Seconds, from Retry-After (429 and 503). */
  readonly retryAfter?: number;
  constructor(status: number, message: string, retryAfter?: number) {
    super(message);
    this.name = "AuthError";
    this.status = status;
    this.retryAfter = retryAfter;
  }
}

const ROLE_ORDER: Record<Role, number> = { viewer: 0, operator: 1, admin: 2 };

/** True when `have` includes the privileges of `need`. */
export function roleAtLeast(have: Role | undefined, need: Role): boolean {
  return have !== undefined && ROLE_ORDER[have] >= ROLE_ORDER[need];
}

/** The text to show for a failed /auth call: the API's message, plus the wait for 429. */
export function describeAuthError(e: unknown): string {
  if (e instanceof AuthError) {
    if (e.status === 429 && e.retryAfter) return `${e.message} (retry in ${formatWait(e.retryAfter)}).`;
    if (e.status === 503 && e.retryAfter) return `${e.message} (retry in ${formatWait(e.retryAfter)}).`;
    return e.message;
  }
  return "Could not reach the agent. Check your connection and try again.";
}

export function formatWait(seconds: number): string {
  if (seconds < 90) return `${seconds} second${seconds === 1 ? "" : "s"}`;
  const m = Math.ceil(seconds / 60);
  return `${m} minutes`;
}

async function request<T>(path: string, init: RequestInit): Promise<T | null> {
  let res: Response;
  try {
    res = await fetch(path, { credentials: "same-origin", ...init });
  } catch {
    throw new AuthError(0, "Could not reach the agent. Check your connection and try again.");
  }
  if (res.ok) {
    if (res.status === 204) return null;
    return (await res.json()) as T;
  }
  let message = res.statusText || `request failed (${res.status})`;
  try {
    const body = (await res.json()) as { error?: unknown };
    if (typeof body.error === "string" && body.error) message = body.error;
  } catch {
    /* non-JSON error body */
  }
  const ra = Number.parseInt(res.headers.get("Retry-After") ?? "", 10);
  throw new AuthError(res.status, message, Number.isFinite(ra) && ra > 0 ? ra : undefined);
}

function post<T>(path: string, body: unknown): Promise<T | null> {
  return request<T>(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

/** The signed-in user, or null when there is no valid session. */
export async function getSession(signal?: AbortSignal): Promise<SessionUser | null> {
  try {
    return await request<SessionUser>("/auth/session", { method: "GET", signal });
  } catch (e) {
    if (e instanceof AuthError && e.status === 401) return null;
    throw e;
  }
}

/** Used by the RPC client to tell a refused upgrade from a network failure. */
export async function probeSession(): Promise<"ok" | "unauthenticated"> {
  const s = await getSession();
  return s ? "ok" : "unauthenticated";
}

export async function login(username: string, password: string, code: string): Promise<SessionUser> {
  return (await post<SessionUser>("/auth/login", { username, password, code }))!;
}

export async function logout(): Promise<void> {
  await post("/auth/logout", {});
}

export async function setupBegin(token: string): Promise<SetupInfo> {
  return (await post<SetupInfo>("/auth/setup/begin", { token }))!;
}

export async function setupFinish(token: string, password: string, code: string): Promise<SessionUser> {
  return (await post<SessionUser>("/auth/setup/finish", { token, password, code }))!;
}

/** Minimum length the server enforces (docs: 12 to 1024 characters). */
export const PASSWORD_MIN = 12;
export const PASSWORD_MAX = 1024;
