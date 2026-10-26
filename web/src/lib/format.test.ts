import { describe, expect, test } from "bun:test";
import { formatBytes, formatDuration, formatPercent, formatRate, shortSha } from "./format";

describe("format", () => {
  test("bytes", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(1536)).toBe("1.5 KiB");
    expect(formatBytes(5 * 1024 ** 3)).toBe("5.0 GiB");
    expect(formatBytes(null)).toBe("–");
    expect(formatRate(2048)).toBe("2.0 KiB/s");
    expect(formatRate(null)).toBe("–");
  });
  test("percent and duration", () => {
    expect(formatPercent(12.345)).toBe("12.3%");
    expect(formatPercent(null)).toBe("–");
    expect(formatDuration(42)).toBe("42s");
    expect(formatDuration(3 * 3600 + 5 * 60)).toBe("3h 5m");
    expect(formatDuration(90000)).toBe("1d 1h");
  });
  test("sha", () => {
    expect(shortSha("0123456789abcdef")).toBe("01234567");
    expect(shortSha("")).toBe("–");
  });
});
