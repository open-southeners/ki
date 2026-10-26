import { describe, expect, test } from "bun:test";
import { auditFailed, expiryDuration, filterAudit, losesAdmin, parseDotenv } from "./admin";
import type { AuditEntry } from "./types";

describe("losesAdmin", () => {
  test("only admin to something lower", () => {
    expect(losesAdmin("admin", "operator")).toBe(true);
    expect(losesAdmin("admin", "admin")).toBe(false);
    expect(losesAdmin("viewer", "operator")).toBe(false);
  });
});

describe("expiryDuration", () => {
  test("presets map to Go durations (no d unit)", () => {
    expect(expiryDuration("30")).toBe("720h");
    expect(expiryDuration("90")).toBe("2160h");
    expect(expiryDuration("365")).toBe("8760h");
  });
  test("none and unknown mean no expiry", () => {
    expect(expiryDuration("none")).toBeUndefined();
    expect(expiryDuration("bogus")).toBeUndefined();
  });
});

describe("parseDotenv", () => {
  test("plain, export, comments and blanks", () => {
    const r = parseDotenv("# c\n\nA=1\nexport B = two \r\nC=\n");
    expect(r.error).toBeUndefined();
    expect(r.vars).toEqual({ A: "1", B: "two", C: "" });
  });
  test("quotes", () => {
    const r = parseDotenv(`A='lit\\n #x'\nB="a\\nb \\"q\\" \\\\ \\$ #kept"\nC=plain # tail\nD=a#b`);
    expect(r.vars).toEqual({ A: "lit\\n #x", B: 'a\nb "q" \\ $ #kept', C: "plain", D: "a#b" });
  });
  test("value may contain =", () => {
    expect(parseDotenv("URL=postgres://u:p@h/db?x=1").vars.URL).toBe("postgres://u:p@h/db?x=1");
  });
  test("BOM on the first line", () => {
    expect(parseDotenv("﻿A=1").vars).toEqual({ A: "1" });
  });
  test("errors carry the line number", () => {
    expect(parseDotenv("A=1\nnoequals").error).toBe("Line 2: expected NAME=VALUE");
    expect(parseDotenv("=x").error).toContain("Line 1");
    expect(parseDotenv("A='open").error).toBe("Line 1: unterminated single quote");
    expect(parseDotenv('A="open').error).toBe("Line 1: unterminated double quote");
    expect(parseDotenv("A=1\nB='x").vars).toEqual({});
  });
});

const e = (id: number, o: Partial<AuditEntry>): AuditEntry => ({
  id,
  time: "2026-01-01T00:00:00Z",
  principal_kind: "user",
  principal_name: "alice",
  ip: "10.0.0.1",
  action: "projects.create",
  target: "web",
  result: "ok",
  ...o,
});

describe("filterAudit", () => {
  const rows = [
    e(1, {}),
    e(2, { principal_name: "bob", action: "users.delete", target: "carol", result: "-32001" }),
    e(3, { ip: "192.168.1.5", action: "auth.login" }),
  ];
  test("empty query keeps all", () => {
    expect(filterAudit(rows, "  ")).toHaveLength(3);
  });
  test("case-insensitive and AND across terms", () => {
    expect(filterAudit(rows, "BOB").map((r) => r.id)).toEqual([2]);
    expect(filterAudit(rows, "alice login").map((r) => r.id)).toEqual([3]);
    expect(filterAudit(rows, "alice carol")).toEqual([]);
  });
  test("matches ip and result", () => {
    expect(filterAudit(rows, "192.168").map((r) => r.id)).toEqual([3]);
    expect(filterAudit(rows, "-32001").map((r) => r.id)).toEqual([2]);
  });
  test("auditFailed", () => {
    expect(auditFailed("ok")).toBe(false);
    expect(auditFailed("-32001")).toBe(true);
  });
});
