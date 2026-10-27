import { describe, expect, test } from "bun:test";
import { composeInvalid, composeLabel, composeRef, parseComposeRef } from "./compose";
import { RpcError } from "./rpc";

describe("compose refs", () => {
  test("round trip", () => {
    expect(composeRef(4)).toBe("compose@v4");
    expect(parseComposeRef("compose@v4")).toBe(4);
    expect(parseComposeRef("main")).toBeNull();
    expect(parseComposeRef("compose@v")).toBeNull();
  });
  test("label", () => {
    expect(composeLabel({ source: "compose", compose_version: 3 })).toBe("Compose v3");
    expect(composeLabel({ source: "compose" })).toBe("Compose");
    expect(composeLabel({ source: "git" })).toBe("Git");
  });
});

describe("composeInvalid", () => {
  test("reads violations", () => {
    const e = new RpcError(-32602, "invalid", {
      reason: "policy_violation",
      detail: "bad",
      violations: [{ service: "web", rule: "privileged", detail: "no" }],
    });
    expect(composeInvalid(e)).toEqual({
      detail: "bad",
      violations: [{ service: "web", rule: "privileged", detail: "no" }],
    });
  });
  test("ignores other errors", () => {
    expect(composeInvalid(new RpcError(-32602, "missing slug"))).toBeNull();
    expect(composeInvalid(new RpcError(-32006, "x", { reason: "r" }))).toBeNull();
    expect(composeInvalid(new Error("x"))).toBeNull();
  });
});
