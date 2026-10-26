import { afterEach, describe, expect, test } from "bun:test";
import { AuthError, describeAuthError, getSession, login, roleAtLeast, setupBegin } from "./auth";

const realFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = realFetch;
});

function stub(res: Response, seen?: (url: string, init: RequestInit) => void): void {
  globalThis.fetch = (async (url: string, init: RequestInit) => {
    seen?.(url, init);
    return res;
  }) as unknown as typeof fetch;
}

describe("auth client", () => {
  test("login posts JSON and returns the session user", async () => {
    let got: { url: string; init: RequestInit } | undefined;
    stub(Response.json({ username: "ann", role: "admin" }), (url, init) => (got = { url, init }));
    expect(await login("ann", "pw", "123456")).toEqual({ username: "ann", role: "admin" });
    expect(got?.url).toBe("/auth/login");
    expect(got?.init.method).toBe("POST");
    expect((got?.init.headers as Record<string, string>)["Content-Type"]).toBe("application/json");
    expect(JSON.parse(got?.init.body as string)).toEqual({ username: "ann", password: "pw", code: "123456" });
  });

  test("API error text is shown verbatim and 429 carries Retry-After", async () => {
    stub(Response.json({ error: "too many attempts, try again later" }, { status: 429, headers: { "Retry-After": "91" } }));
    const err = (await login("a", "b", "c").catch((e: unknown) => e)) as AuthError;
    expect(err).toBeInstanceOf(AuthError);
    expect(err.status).toBe(429);
    expect(err.retryAfter).toBe(91);
    expect(describeAuthError(err)).toBe("too many attempts, try again later (retry in 2 minutes).");
  });

  test("401 on login keeps the generic message", async () => {
    stub(Response.json({ error: "invalid credentials" }, { status: 401 }));
    const err = (await login("a", "b", "c").catch((e: unknown) => e)) as AuthError;
    expect(describeAuthError(err)).toBe("invalid credentials");
  });

  test("session: 401 means signed out, other failures throw", async () => {
    stub(Response.json({ error: "unauthenticated" }, { status: 401 }));
    expect(await getSession()).toBeNull();
    stub(Response.json({ error: "internal error" }, { status: 500 }));
    await expect(getSession()).rejects.toBeInstanceOf(AuthError);
  });

  test("setup begin returns the enrolment data", async () => {
    stub(Response.json({ username: "ann", role: "viewer", totp_secret: "SEC", otpauth_uri: "otpauth://totp/x" }));
    expect((await setupBegin("tok")).totp_secret).toBe("SEC");
  });

  test("network failure becomes a friendly AuthError", async () => {
    globalThis.fetch = (async () => {
      throw new TypeError("fetch failed");
    }) as unknown as typeof fetch;
    const err = (await login("a", "b", "c").catch((e: unknown) => e)) as AuthError;
    expect(err.status).toBe(0);
  });

  test("role ordering", () => {
    expect(roleAtLeast("admin", "operator")).toBe(true);
    expect(roleAtLeast("viewer", "operator")).toBe(false);
    expect(roleAtLeast(undefined, "viewer")).toBe(false);
  });
});
