import { describe, expect, test } from "bun:test";
import { RpcError } from "@/lib/rpc";
import type { Deploy, HostSample, ProjectService } from "@/lib/types";
import {
  appendHostSample,
  awsAgentBadge,
  failureReasonText,
  isWildcardName,
  containersRunning,
  deployErrorText,
  deployStatusBadge,
  emptyDeployLog,
  exposureUrls,
  markDeployLogGap,
  mergeHostSeries,
  previousGood,
  reduceDeployLog,
  rollbackTargets,
  serviceCounts,
  serviceStateBadge,
} from "./logic";

function sample(t: number, cpu: number | null = 10): HostSample {
  return {
    time: new Date(t).toISOString(),
    cpu: { percent: cpu, cores: 4 },
    load: { "1": 0, "5": 0, "15": 0 },
    memory: { total: 100, used: 50, available: 50, percent: 50 },
    swap: { total: 0, used: 0, percent: 0 },
    disks: [],
    disk_io: { read_bytes_per_s: 1, write_bytes_per_s: 2, read_ops_per_s: 0, write_ops_per_s: 0 },
    network: { rx_bytes_per_s: 3, tx_bytes_per_s: 4, interfaces: [] },
    uptime_s: 1,
  };
}

function svc(name: string, state: ProjectService["state"], running = 1, total = 1): ProjectService {
  return {
    name,
    state,
    containers: Array.from({ length: total }, (_, i) => ({
      id: `${name}${i}`,
      name: `${name}-${i}`,
      state: i < running ? "running" : "exited",
      status: "",
      health: "",
      restart_count: 0,
    })),
  };
}

function deploy(id: number, status: Deploy["status"], commit = "abc"): Deploy {
  return {
    id,
    project: "p",
    ref: "main",
    commit,
    source: "api",
    status,
    stage: "up",
    created_at: "2026-01-01T00:00:00Z",
    started_at: null,
    finished_at: null,
  };
}

describe("badges", () => {
  test("service states", () => {
    expect(serviceStateBadge("running").tone).toBe("success");
    expect(serviceStateBadge("partial").tone).toBe("warning");
    expect(serviceStateBadge("unhealthy").tone).toBe("destructive");
    expect(serviceStateBadge("stopped").tone).toBe("secondary");
    expect(serviceStateBadge("missing").tone).toBe("outline");
    expect(serviceStateBadge("weird").label).toBe("weird");
  });
  test("deploy statuses", () => {
    expect(deployStatusBadge("succeeded").tone).toBe("success");
    expect(deployStatusBadge("failed").tone).toBe("destructive");
    expect(deployStatusBadge("running").tone).toBe("default");
  });
});

describe("services", () => {
  test("counts and containers", () => {
    const list = [svc("a", "running"), svc("b", "partial", 1, 2), svc("c", "stopped", 0), svc("d", "unhealthy")];
    expect(serviceCounts(list)).toEqual({ total: 4, running: 1, problem: 2 });
    expect(containersRunning(list[1]!)).toEqual({ running: 1, total: 2 });
  });
  test("exposure urls", () => {
    const s = svc("web", "running");
    expect(exposureUrls(s)).toEqual([]);
    s.exposure = { service: "web", port: 80, domains: ["a.test", "b.test"], path: "/app", source: "labels" };
    expect(exposureUrls(s)).toEqual(["https://a.test/app", "https://b.test/app"]);
    s.exposure.path = "/";
    expect(exposureUrls(s)[0]).toBe("https://a.test");
  });
});

describe("host series", () => {
  test("merges, dedupes and windows", () => {
    const hist = [sample(1000, 1), sample(2000, 2), sample(3000, 3)];
    const live = [sample(3000, 33), sample(4000, 4)];
    const s = mergeHostSeries(hist, live, 2500);
    expect(s.map((p) => p.t)).toEqual([2000, 3000, 4000].filter((t) => t >= 1500));
    expect(s.find((p) => p.t === 3000)?.cpu).toBe(33);
  });
  test("append keeps order and trims", () => {
    let s = mergeHostSeries([sample(1000), sample(2000)], []);
    s = appendHostSample(s, sample(3000), 1500);
    expect(s.map((p) => p.t)).toEqual([2000, 3000]);
    s = appendHostSample(s, sample(2500, 7), 1500);
    expect(s.map((p) => p.t)).toEqual([2000, 2500, 3000]);
  });
  test("empty", () => {
    expect(mergeHostSeries([], [])).toEqual([]);
  });
});

describe("deploy log", () => {
  test("ignores replayed lines after a resume", () => {
    let st = emptyDeployLog;
    st = reduceDeployLog(st, { type: "log", seq: 1, stream: "stdout", text: "a" });
    st = reduceDeployLog(st, { type: "log", seq: 2, stream: "stdout", text: "b" });
    st = reduceDeployLog(st, { type: "log", seq: 2, stream: "stdout", text: "b" });
    st = reduceDeployLog(st, { type: "log", seq: 3, stream: "agent", text: "c" });
    expect(st.lines.map((l) => l.text)).toEqual(["a", "b", "c"]);
    expect(st.lastSeq).toBe(3);
  });
  test("lines without seq always append", () => {
    let st = reduceDeployLog(emptyDeployLog, { type: "log", stream: "stdout", text: "x" });
    st = reduceDeployLog(st, { type: "log", stream: "stdout", text: "y" });
    expect(st.lines).toHaveLength(2);
    expect(st.lastSeq).toBe(0);
  });
  test("caps the buffer and counts dropped lines", () => {
    let st = emptyDeployLog;
    for (let i = 1; i <= 5; i++) st = reduceDeployLog(st, { type: "log", seq: i, stream: "stdout", text: String(i) }, 3);
    expect(st.lines.map((l) => l.text)).toEqual(["3", "4", "5"]);
    expect(st.dropped).toBe(2);
  });
  test("status updates and gaps", () => {
    let st = reduceDeployLog(emptyDeployLog, { type: "status", deploy: deploy(1, "running") });
    expect(st.deploy?.status).toBe("running");
    st = markDeployLogGap(st);
    expect(st.lines[0]?.gap).toBe(true);
  });
});

describe("errors", () => {
  test("friendly deploy errors", () => {
    const e = new RpcError(-32012, "deploy error", { reason: "no_rollback_target", detail: "x" });
    expect(deployErrorText(e)).toContain("roll back");
    expect(deployErrorText(new RpcError(-32012, "msg", { reason: "other", detail: "boom" }))).toBe("boom");
    expect(deployErrorText(new RpcError(-32006, "nf"))).toBeNull();
    expect(deployErrorText(new Error("x"))).toBeNull();
  });
});

describe("rollback targets", () => {
  test("filters and picks previous good", () => {
    const list = [deploy(5, "failed"), deploy(4, "succeeded"), deploy(3, "superseded"), deploy(2, "succeeded"), deploy(1, "succeeded", "")];
    expect(rollbackTargets(list).map((d) => d.id)).toEqual([4, 2]);
    expect(previousGood(list)?.id).toBe(2);
    expect(previousGood([deploy(1, "succeeded")])).toBeUndefined();
  });
});

describe("aws helpers", () => {
  test("awsAgentBadge", () => {
    expect(awsAgentBadge({ installed: false, active: false, enabled: false }).label).toBe("not installed");
    expect(awsAgentBadge({ installed: true, active: true, enabled: true }).tone).toBe("success");
    expect(awsAgentBadge({ installed: true, active: false, enabled: true }).tone).toBe("warning");
    expect(awsAgentBadge({ installed: true, active: false, enabled: false }).label).toBe("inactive");
  });
  test("isWildcardName", () => {
    expect(isWildcardName("*.example.com")).toBe(true);
    expect(isWildcardName("app.example.com")).toBe(false);
  });
  test("pull_failed has friendly text", () => {
    expect(failureReasonText("pull_failed")).toContain("pulled");
  });
});
