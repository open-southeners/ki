import { describe, expect, test } from "bun:test";
import { RpcClient, RpcError, type SocketLike } from "./rpc";
import { ErrorCode } from "./types";

type Msg = { id?: number; method: string; params?: any };

class FakeSocket implements SocketLike {
  onopen: SocketLike["onopen"] = null;
  onmessage: SocketLike["onmessage"] = null;
  onclose: SocketLike["onclose"] = null;
  onerror: SocketLike["onerror"] = null;
  sent: Msg[] = [];
  closed = false;
  constructor(readonly server: FakeServer) {}
  send(data: string): void {
    const m = JSON.parse(data) as Msg;
    this.sent.push(m);
    this.server.onRequest(this, m);
  }
  close(): void {
    if (this.closed) return;
    this.closed = true;
    queueMicrotask(() => this.onclose?.({ code: 1000 }));
  }
  // server side helpers
  push(obj: unknown): void {
    this.onmessage?.({ data: JSON.stringify(obj) });
  }
  reply(id: number, result: unknown): void {
    this.push({ jsonrpc: "2.0", id, result });
  }
  fail(id: number, code: number, message: string, data?: unknown): void {
    this.push({ jsonrpc: "2.0", id, error: { code, message, data } });
  }
  update(method: string, subscription: string, seq: number, rest: { data?: unknown; gap?: unknown }): void {
    this.push({ jsonrpc: "2.0", method, params: { subscription, seq, ...rest } });
  }
  drop(): void {
    this.closed = true;
    this.onclose?.({ code: 1006 });
  }
}

class FakeServer {
  sockets: FakeSocket[] = [];
  autoHello = true;
  helloError: { code: number; message: string; data?: unknown } | null = null;
  handler: (s: FakeSocket, m: Msg) => boolean = () => false;
  create = (_url: string): SocketLike => {
    const s = new FakeSocket(this);
    this.sockets.push(s);
    queueMicrotask(() => s.onopen?.());
    return s;
  };
  get last(): FakeSocket {
    return this.sockets[this.sockets.length - 1]!;
  }
  onRequest(s: FakeSocket, m: Msg): void {
    if (m.method === "agent.hello" && this.autoHello) {
      if (this.helloError) s.fail(m.id!, this.helloError.code, this.helloError.message, this.helloError.data);
      else
        s.reply(m.id!, {
          protocol: "1.0.0",
          agent: { name: "ki-agent", version: "t", commit: "c" },
          capabilities: ["stats"],
        });
      return;
    }
    this.handler(s, m);
  }
}

async function until(cond: () => boolean, ms = 1000): Promise<void> {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error("timeout waiting for condition");
    await new Promise((r) => setTimeout(r, 1));
  }
}

function make(srv: FakeServer, extra: Partial<ConstructorParameters<typeof RpcClient>[0]> = {}) {
  return new RpcClient({
    url: "ws://test/rpc",
    createSocket: srv.create,
    timeoutMs: 200,
    backoff: { baseMs: 2, maxMs: 8 },
    random: () => 1,
    ...extra,
  });
}

describe("handshake and calls", () => {
  test("sends agent.hello first and reaches open", async () => {
    const srv = new FakeServer();
    const c = make(srv);
    const states: string[] = [];
    c.onStateChange(() => states.push(c.state));
    c.connect();
    await until(() => c.state === "open");
    expect(srv.sockets[0]!.sent[0]).toMatchObject({
      jsonrpc: "2.0",
      id: 1,
      method: "agent.hello",
      params: { protocol: "1.0.0", client: { name: "ki-panel" } },
    });
    expect(c.hello?.capabilities).toEqual(["stats"]);
    expect(states).toEqual(["connecting", "open"]);
    c.close();
  });

  test("matches responses by id even when they arrive out of order", async () => {
    const srv = new FakeServer();
    const held: Array<{ s: FakeSocket; m: Msg }> = [];
    srv.handler = (s, m) => (held.push({ s, m }), true);
    const c = make(srv);
    c.connect();
    const a = c.call<string>("slow");
    const b = c.call<string>("fast", { x: 1 });
    await until(() => held.length === 2);
    held[1]!.s.reply(held[1]!.m.id!, "B");
    held[0]!.s.reply(held[0]!.m.id!, "A");
    expect(await a).toBe("A");
    expect(await b).toBe("B");
    expect(held[1]!.m.params).toEqual({ x: 1 });
    c.close();
  });

  test("maps JSON-RPC errors to RpcError with code and data", async () => {
    const srv = new FakeServer();
    srv.handler = (s, m) => (s.fail(m.id!, -32012, "deploy rejected", { reason: "ref_not_found", detail: "x" }), true);
    const c = make(srv);
    c.connect();
    const err = await failure(c.call("deploys.trigger", { project: "p" }));
    expect(err).toBeInstanceOf(RpcError);
    expect(err.code).toBe(ErrorCode.Deploy);
    expect(err.message).toBe("deploy rejected");
    expect(err.data).toEqual({ reason: "ref_not_found", detail: "x" });
    c.close();
  });

  test("calls time out", async () => {
    const srv = new FakeServer();
    srv.handler = () => true;
    const c = make(srv);
    c.connect();
    const err = await failure(c.call("never", undefined, { timeoutMs: 20 }));
    expect(err).toBeInstanceOf(RpcError);
    expect(err.code).toBe(ErrorCode.Timeout);
    c.close();
  });

  test("dispatches plain notifications", async () => {
    const srv = new FakeServer();
    const c = make(srv);
    c.connect();
    await until(() => c.state === "open");
    const got: unknown[] = [];
    const off = c.on("thing.happened", (p) => got.push(p));
    srv.last.push({ jsonrpc: "2.0", method: "thing.happened", params: { a: 1 } });
    off();
    srv.last.push({ jsonrpc: "2.0", method: "thing.happened", params: { a: 2 } });
    expect(got).toEqual([{ a: 1 }]);
    c.close();
  });

  test("incompatible protocol version is fatal", async () => {
    const srv = new FakeServer();
    srv.helloError = { code: -32003, message: "Incompatible protocol version", data: { supported: "2.0.0" } };
    const c = make(srv);
    c.connect();
    await until(() => c.state === "closed");
    expect(c.lastError).toContain("Incompatible");
    expect(srv.sockets.length).toBe(1);
  });

  test("a call made while connecting waits for the handshake", async () => {
    const srv = new FakeServer();
    srv.handler = (s, m) => (s.reply(m.id!, "pong"), true);
    const c = make(srv);
    const p = c.call("agent.ping"); // before connect() resolves anything
    c.connect();
    expect(await p).toBe("pong");
    c.close();
  });
});

function subServer() {
  const srv = new FakeServer();
  let n = 0;
  srv.handler = (s, m) => {
    if (m.method.endsWith(".subscribe")) {
      s.reply(m.id!, { subscription: `sub${++n}`.padEnd(32, "0") });
      return true;
    }
    if (m.method.endsWith(".unsubscribe")) {
      s.reply(m.id!, {});
      return true;
    }
    return false;
  };
  return srv;
}
const id = (n: number) => `sub${n}`.padEnd(32, "0");

describe("subscriptions", () => {
  test("buffers updates that arrive before the subscribe response and orders them by seq", async () => {
    const srv = new FakeServer();
    const held: Msg[] = [];
    srv.handler = (_s, m) => (held.push(m), true);
    const c = make(srv);
    c.connect();
    const seen: Array<[unknown, number]> = [];
    c.subscribe<string>({
      method: "containers.logs.subscribe",
      params: { id: "abc" },
      kind: "ordered",
      onData: (d, meta) => seen.push([d, meta.seq]),
    });
    await until(() => held.length === 1);
    const s = srv.last;
    s.update("containers.logs.update", id(1), 2, { data: "two" });
    s.update("containers.logs.update", id(1), 1, { data: "one" });
    expect(seen).toEqual([]);
    s.reply(held[0]!.id!, { subscription: id(1) });
    await until(() => seen.length === 2);
    expect(seen).toEqual([
      ["one", 1],
      ["two", 2],
    ]);
    c.close();
  });

  test("drops stale and duplicate seq", async () => {
    const srv = subServer();
    const c = make(srv);
    c.connect();
    const seen: number[] = [];
    c.subscribe<number>({ method: "host.subscribe", kind: "snapshot", onData: (d) => seen.push(d) });
    await until(() => srv.sockets[0]?.sent.some((m) => m.method === "host.subscribe") ?? false);
    await until(() => srv.last.sent.length >= 2);
    const s = srv.last;
    await new Promise((r) => setTimeout(r, 5));
    s.update("host.update", id(1), 5, { data: 5 });
    s.update("host.update", id(1), 5, { data: 55 });
    s.update("host.update", id(1), 3, { data: 3 });
    s.update("host.update", id(1), 9, { data: 9 });
    expect(seen).toEqual([5, 9]);
    c.close();
  });

  test("ordered streams report gap markers and keep seq contiguous", async () => {
    const srv = subServer();
    const c = make(srv);
    c.connect();
    const events: string[] = [];
    c.subscribe<string>({
      method: "containers.logs.subscribe",
      kind: "ordered",
      onData: (d, m) => events.push(`${m.seq}:${d}`),
      onGap: (g, synthetic) => events.push(`gap ${g.from}-${g.to}${synthetic ? " synthetic" : ""}`),
    });
    await until(() => c.state === "open");
    await new Promise((r) => setTimeout(r, 5));
    const s = srv.last;
    s.update("containers.logs.update", id(1), 1, { data: "a" });
    s.update("containers.logs.update", id(1), 4, { gap: { from: 2, to: 4 } });
    s.update("containers.logs.update", id(1), 5, { data: "b" }); // contiguous after the marker
    s.update("containers.logs.update", id(1), 8, { data: "c" }); // hole without a marker
    expect(events).toEqual(["1:a", "gap 2-4", "5:b", "gap 6-7 synthetic", "8:c"]);
    c.close();
  });

  test("snapshot streams skip seq silently and ignore gap markers", async () => {
    const srv = subServer();
    const c = make(srv);
    c.connect();
    const events: string[] = [];
    c.subscribe<string>({
      method: "host.subscribe",
      kind: "snapshot",
      onData: (d, m) => events.push(`${m.seq}:${d}`),
      onGap: () => events.push("gap"),
    });
    await until(() => c.state === "open");
    await new Promise((r) => setTimeout(r, 5));
    srv.last.update("host.update", id(1), 1, { data: "a" });
    srv.last.update("host.update", id(1), 7, { data: "b" });
    expect(events).toEqual(["1:a", "7:b"]);
    c.close();
  });

  test("re-subscribes after a reconnect with resume params and a fresh seq", async () => {
    const srv = subServer();
    const c = make(srv);
    c.connect();
    const seen: string[] = [];
    const states: string[] = [];
    c.subscribe<{ seq?: number; t: string }>({
      method: "deploys.subscribe",
      params: { id: 7 },
      kind: "ordered",
      onData: (d, m) => seen.push(`${m.seq}:${d.t}`),
      onState: (s) => states.push(s),
      resume: (last) => ({ since_seq: last.data?.seq ?? 0 }),
    });
    await until(() => srv.sockets[0]?.sent.some((m) => m.method === "deploys.subscribe") ?? false);
    await new Promise((r) => setTimeout(r, 5));
    srv.sockets[0]!.update("deploys.update", id(1), 1, { data: { seq: 10, t: "x" } });
    srv.sockets[0]!.update("deploys.update", id(1), 2, { data: { seq: 11, t: "y" } });
    srv.sockets[0]!.drop();
    await until(() => srv.sockets.length === 2);
    await until(() => srv.sockets[1]!.sent.some((m) => m.method === "deploys.subscribe"));
    const resub = srv.sockets[1]!.sent.find((m) => m.method === "deploys.subscribe")!;
    expect(resub.params).toEqual({ id: 7, since_seq: 11 });
    await new Promise((r) => setTimeout(r, 5));
    // new subscription: seq restarts at 1 and must be accepted
    srv.sockets[1]!.update("deploys.update", id(2), 1, { data: { seq: 12, t: "z" } });
    expect(seen).toEqual(["1:x", "2:y", "1:z"]);
    expect(states).toEqual(["active", "suspended", "active"]);
    c.close();
  });

  test("a final update ends the subscription: no unsubscribe, no re-subscribe", async () => {
    const srv = subServer();
    const c = make(srv);
    c.connect();
    const states: string[] = [];
    c.subscribe<{ eof?: boolean }>({
      method: "containers.logs.subscribe",
      kind: "ordered",
      onData: () => {},
      isFinal: (d) => d.eof === true,
      onState: (s) => states.push(s),
    });
    await until(() => c.state === "open");
    await new Promise((r) => setTimeout(r, 5));
    srv.last.update("containers.logs.update", id(1), 1, { data: { eof: true } });
    expect(states).toEqual(["active", "ended"]);
    srv.sockets[0]!.drop();
    await until(() => srv.sockets.length === 2);
    await until(() => c.state === "open");
    await new Promise((r) => setTimeout(r, 5));
    expect(srv.sockets[1]!.sent.map((m) => m.method)).toEqual(["agent.hello"]);
    expect(srv.sockets[0]!.sent.some((m) => m.method.endsWith(".unsubscribe"))).toBe(false);
    c.close();
  });

  test("unsubscribe sends <ns>.unsubscribe and stops delivery", async () => {
    const srv = subServer();
    const c = make(srv);
    c.connect();
    const seen: number[] = [];
    const sub = c.subscribe<number>({
      method: "host.subscribe",
      kind: "snapshot",
      onData: (d) => seen.push(d),
    });
    await until(() => c.state === "open");
    await new Promise((r) => setTimeout(r, 5));
    srv.last.update("host.update", id(1), 1, { data: 1 });
    sub.unsubscribe();
    srv.last.update("host.update", id(1), 2, { data: 2 });
    await until(() => srv.last.sent.some((m) => m.method === "host.unsubscribe"));
    expect(srv.last.sent.find((m) => m.method === "host.unsubscribe")!.params).toEqual({ subscription: id(1) });
    expect(seen).toEqual([1]);
    c.close();
  });

  test("reports a failed subscribe as an error state", async () => {
    const srv = new FakeServer();
    srv.handler = (s, m) => (s.fail(m.id!, -32006, "not found"), true);
    const c = make(srv);
    c.connect();
    let err: RpcError | undefined;
    c.subscribe({
      method: "containers.logs.subscribe",
      kind: "ordered",
      onData: () => {},
      onState: (st, e) => st === "error" && (err = e),
    });
    await until(() => err !== undefined);
    expect(err!.code).toBe(ErrorCode.NotFound);
    c.close();
  });
});

describe("reconnect", () => {
  test("pending calls reject when the link drops, and the client reconnects with backoff", async () => {
    const srv = new FakeServer();
    srv.handler = () => true; // never answer
    const c = make(srv);
    c.connect();
    await until(() => c.state === "open");
    const p = failure(c.call("hang"));
    await until(() => srv.last.sent.length === 2);
    srv.last.drop();
    const err = await p;
    expect(err.code).toBe(ErrorCode.ConnectionLost);
    expect(c.state).toBe("reconnecting");
    await until(() => c.state === "open");
    expect(srv.sockets.length).toBe(2);
    c.close();
  });

  test("a failed refused upgrade followed by a 401 probe stops retrying", async () => {
    const srv = new FakeServer();
    srv.autoHello = false;
    const c = make(srv, { probe: async () => "unauthenticated" });
    c.connect();
    await until(() => srv.sockets.length === 1);
    await new Promise((r) => setTimeout(r, 5));
    srv.sockets[0]!.drop(); // closed before the handshake
    await until(() => c.state === "unauthorized");
    await new Promise((r) => setTimeout(r, 30));
    expect(srv.sockets.length).toBe(1);
    const err = await failure(c.call("x"));
    expect(err.code).toBe(ErrorCode.Unauthorized);
    // signing in again lets the app reconnect
    srv.autoHello = true;
    c.reconnect();
    await until(() => c.state === "open");
    c.close();
  });

  test("backoff grows and is capped", async () => {
    const srv = new FakeServer();
    const delays: number[] = [];
    let last = Date.now();
    srv.create = (url: string) => {
      const now = Date.now();
      delays.push(now - last);
      last = now;
      const s = new FakeSocket(srv);
      srv.sockets.push(s);
      void url;
      queueMicrotask(() => s.drop()); // never opens
      return s;
    };
    const c = new RpcClient({
      createSocket: srv.create,
      url: "x",
      backoff: { baseMs: 10, maxMs: 40 },
      random: () => 1,
    });
    c.connect();
    await until(() => srv.sockets.length >= 5);
    c.close();
    // delays: ~0, 10, 20, 40, 40 (timers are lower-bounded, allow slack)
    expect(delays[2]!).toBeGreaterThanOrEqual(delays[1]! - 2);
    expect(Math.max(...delays)).toBeLessThan(120);
  });
});

/** Awaits a promise that must reject and returns the error. */
async function failure(p: Promise<unknown>): Promise<RpcError> {
  try {
    await p;
  } catch (e) {
    return e as RpcError;
  }
  throw new Error("expected a rejection");
}
