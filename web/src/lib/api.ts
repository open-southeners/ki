// Typed helpers over RpcClient: one function per request method of
// api/openrpc.json, plus subscription wrappers with the right kind/resume rules.

import type { CallOptions, RpcClient, Subscription, SubscriptionState, RpcError } from "./rpc";
import type {
  ContainerStatsUpdate,
  DeployUpdate,
  Gap,
  HostSample,
  LogLine,
  LogUpdate,
  MethodMap,
  MethodName,
} from "./types";

type Args<M extends MethodName> = undefined extends MethodMap[M]["params"]
  ? [params?: MethodMap[M]["params"], opts?: CallOptions]
  : [params: MethodMap[M]["params"], opts?: CallOptions];

export type Api = ReturnType<typeof createApi>;

export function createApi(rpc: RpcClient) {
  const m =
    <M extends MethodName>(name: M) =>
    (...args: Args<M>): Promise<MethodMap[M]["result"]> =>
      rpc.call<MethodMap[M]["result"]>(name, args[0], args[1]);

  return {
    /** Escape hatch for any method by name. */
    call: <M extends MethodName>(name: M, ...args: Args<M>): Promise<MethodMap[M]["result"]> =>
      rpc.call<MethodMap[M]["result"]>(name, args[0], args[1]),

    agent: { info: m("agent.info"), ping: m("agent.ping") },
    runtime: { info: m("runtime.info") },
    auth: { whoami: m("auth.whoami") },
    containers: {
      list: m("containers.list"),
      inspect: m("containers.inspect"),
      start: m("containers.start"),
      stop: m("containers.stop"),
      restart: m("containers.restart"),
      remove: m("containers.remove"),
      stats: m("containers.stats"),
    },
    projects: {
      list: m("projects.list"),
      get: m("projects.get"),
      create: m("projects.create"),
      update: m("projects.update"),
      verify: m("projects.verify"),
      delete: m("projects.delete"),
      envList: m("projects.env.list"),
      envSet: m("projects.env.set"),
      envUnset: m("projects.env.unset"),
      expose: m("projects.expose"),
      unexpose: m("projects.unexpose"),
      services: m("projects.services"),
      serviceStart: m("projects.services.start"),
      serviceStop: m("projects.services.stop"),
      composeGet: m("projects.compose.get"),
      composeSet: m("projects.compose.set"),
      composeHistory: m("projects.compose.history"),
      serviceRestart: m("projects.services.restart"),
    },
    deploys: {
      trigger: m("deploys.trigger"),
      rollback: m("deploys.rollback"),
      get: m("deploys.get"),
      list: m("deploys.list"),
    },
    host: { stats: m("host.stats") },
    aws: { status: m("aws.status") },
    tunnel: { status: m("tunnel.status") },
    edge: {
      status: m("edge.status"),
      csr: m("edge.csr"),
      installCertificate: m("edge.certificate.install"),
      removeCertificate: m("edge.certificate.remove"),
    },
    enrollment: { start: m("enrollment.start"), wait: m("enrollment.wait"), remove: m("enrollment.remove") },
    users: {
      list: m("users.list"),
      create: m("users.create"),
      update: m("users.update"),
      reset: m("users.reset"),
      delete: m("users.delete"),
    },
    panel: { setup: m("panel.setup") },
    tokens: { list: m("tokens.list"), create: m("tokens.create"), revoke: m("tokens.revoke") },
    audit: { list: m("audit.list") },
  };
}

// ------------------------------------------------------------ subscriptions

export interface LiveHandlers<T> {
  onData: (data: T) => void;
  onState?: (state: SubscriptionState, err?: RpcError) => void;
}

/** host.subscribe: latest sample wins. */
export function subscribeHost(
  rpc: RpcClient,
  params: { interval_ms?: number; per_core?: boolean },
  h: LiveHandlers<HostSample>,
): Subscription {
  return rpc.subscribe<{ sample: HostSample }>({
    method: "host.subscribe",
    params,
    kind: "snapshot",
    onData: (d) => h.onData(d.sample),
    onState: h.onState,
  });
}

/** containers.stats.subscribe: latest snapshot wins. */
export function subscribeContainerStats(
  rpc: RpcClient,
  params: { project?: string; id?: string; interval_ms?: number },
  h: LiveHandlers<ContainerStatsUpdate>,
): Subscription {
  return rpc.subscribe<ContainerStatsUpdate>({
    method: "containers.stats.subscribe",
    params,
    kind: "snapshot",
    onData: h.onData,
    onState: h.onState,
  });
}

/**
 * containers.logs.subscribe: ordered. The wrapper asks for timestamps so that a
 * re-subscribe after a reconnect continues after the last line (`since`)
 * instead of replaying the tail.
 */
export function subscribeContainerLogs(
  rpc: RpcClient,
  params: { id: string; tail?: number },
  h: LiveHandlers<LogUpdate> & { onGap?: (gap: Gap) => void },
): Subscription {
  return rpc.subscribe<LogUpdate>({
    method: "containers.logs.subscribe",
    params: { ...params, timestamps: true },
    kind: "ordered",
    onData: h.onData,
    onGap: h.onGap ? (g) => h.onGap?.(g) : undefined,
    onState: h.onState,
    isFinal: (d) => "eof" in d && d.eof === true,
    resume: ({ data }) => {
      const t = data && "time" in data ? (data as LogLine).time : undefined;
      return t ? { since: t, tail: 0 } : { tail: 0 };
    },
  });
}

/**
 * deploys.subscribe: ordered replay of the stored log, live lines, then a final
 * status update. After a reconnect it resumes with since_seq = the last stored
 * line number received.
 */
export function subscribeDeploy(
  rpc: RpcClient,
  deployID: number,
  h: LiveHandlers<DeployUpdate> & { onGap?: (gap: Gap) => void },
): Subscription {
  let lastLine = 0;
  return rpc.subscribe<DeployUpdate>({
    method: "deploys.subscribe",
    params: { id: deployID },
    kind: "ordered",
    onData: (d) => {
      if (d.type === "log" && d.seq !== undefined && d.seq > lastLine) lastLine = d.seq;
      h.onData(d);
    },
    onGap: h.onGap ? (g) => h.onGap?.(g) : undefined,
    onState: h.onState,
    isFinal: (d) => d.type === "status" && isTerminal(d.deploy.status),
    resume: () => ({ since_seq: lastLine }),
  });
}

export function isTerminal(status: string): boolean {
  return status === "succeeded" || status === "failed" || status === "superseded" || status === "interrupted";
}

