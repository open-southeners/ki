// JSON-RPC 2.0 client for the agent's WebSocket endpoint (/rpc, cookie auth).
//
// - agent.hello handshake on every (re)connection;
// - request/response with per-call timeouts and a typed RpcError;
// - plain notifications;
// - subscriptions: ordered by seq, gap markers for ordered streams, latest-wins
//   snapshot streams, updates that arrive before the subscribe response;
// - reconnect with exponential backoff and automatic re-subscribe.
//
// See docs/protocol.md, "Subscriptions".

import { ErrorCode, type HelloResult, type Gap } from "./types";

export const PROTOCOL_VERSION = "1.0.0";

export class RpcError extends Error {
  readonly code: number;
  readonly data?: unknown;
  constructor(code: number, message: string, data?: unknown) {
    super(message);
    this.name = "RpcError";
    this.code = code;
    this.data = data;
  }
}

/**
 * - connecting: first attempt in progress
 * - open: handshake done, calls flow
 * - reconnecting: the link dropped; backing off or dialling again
 * - unauthorized: the session is gone (probe said so); not retrying
 * - closed: closed by the caller or by a fatal handshake error
 */
export type ConnState = "idle" | "connecting" | "open" | "reconnecting" | "unauthorized" | "closed";

/** The subset of WebSocket the client uses; tests supply a fake. */
export interface SocketLike {
  send(data: string): void;
  close(code?: number, reason?: string): void;
  onopen: ((ev?: unknown) => void) | null;
  onmessage: ((ev: { data: unknown }) => void) | null;
  onclose: ((ev: { code?: number; reason?: string }) => void) | null;
  onerror: ((ev?: unknown) => void) | null;
}

export interface RpcOptions {
  /** Defaults to ws(s)://<location.host>/rpc. */
  url?: string;
  createSocket?: (url: string) => SocketLike;
  client?: { name: string; version: string };
  /** Per-call timeout, ms. Default 30 s. */
  timeoutMs?: number;
  /** Reconnect delay: min(maxMs, baseMs * 2^attempt), jittered. */
  backoff?: { baseMs: number; maxMs: number };
  random?: () => number;
  /**
   * Called when a connection ended before the handshake finished (a refused
   * upgrade looks like a plain close in browsers). Return "unauthenticated" to
   * stop reconnecting.
   */
  probe?: () => Promise<"ok" | "unauthenticated">;
}

export interface CallOptions {
  timeoutMs?: number;
  signal?: AbortSignal;
}

export type SubscriptionState = "pending" | "active" | "suspended" | "ended" | "error";

export interface SubscribeMeta {
  seq: number;
}

export interface SubscribeOptions<TData> {
  /** The subscribe method, e.g. "host.subscribe"; "<ns>.update" and "<ns>.unsubscribe" derive from it. */
  method: string;
  params?: Record<string, unknown>;
  /**
   * ordered: every update matters, gaps are reported (logs, deploys).
   * snapshot: latest wins, seq may skip silently (host, stats, heartbeat).
   */
  kind: "ordered" | "snapshot";
  onData: (data: TData, meta: SubscribeMeta) => void;
  /** An ordered stream lost updates [from, to]. `synthetic` is true when the client noticed a hole itself. */
  onGap?: (gap: Gap, synthetic: boolean) => void;
  onState?: (state: SubscriptionState, err?: RpcError) => void;
  /** Params to use when re-subscribing after a reconnect (merged over `params`). */
  resume?: (last: { seq: number; data: TData | undefined }) => Record<string, unknown> | undefined;
  /** True for the last update of a stream; the subscription ends without re-subscribing. */
  isFinal?: (data: TData) => boolean;
  /** Called with the subscribe result after (re)subscribing, e.g. for interval_ms. */
  onSubscribed?: (result: unknown) => void;
}

export interface Subscription {
  unsubscribe(): void;
}

type Pending = {
  resolve: (v: unknown) => void;
  reject: (e: unknown) => void;
  timer: ReturnType<typeof setTimeout>;
  cleanup?: () => void;
};

type RawUpdate = { subscription: string; seq: number; data?: unknown; gap?: Gap };

interface Sub {
  opts: SubscribeOptions<unknown>;
  id: string | null;
  lastSeq: number;
  lastData: unknown;
  cancelled: boolean;
  state: SubscriptionState;
  /** Generation guards against stale subscribe responses after a reconnect. */
  gen: number;
}

const MAX_EARLY_BUFFER = 256;

export class RpcClient {
  private opts: Required<Pick<RpcOptions, "timeoutMs" | "backoff" | "random" | "client">> & RpcOptions;
  private sock: SocketLike | null = null;
  private _state: ConnState = "idle";
  private stateListeners = new Set<() => void>();
  private nextID = 1;
  private pending = new Map<number, Pending>();
  private subs = new Set<Sub>();
  private byID = new Map<string, Sub>();
  private early = new Map<string, RawUpdate[]>();
  private notifHandlers = new Map<string, Set<(params: unknown) => void>>();
  private attempt = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private wantOpen = false;
  private generation = 0;
  private readyWaiters: Array<{ resolve: () => void; reject: (e: unknown) => void }> = [];
  private _hello: HelloResult | null = null;
  private _lastError: string | null = null;

  constructor(opts: RpcOptions = {}) {
    this.opts = {
      timeoutMs: 30_000,
      backoff: { baseMs: 500, maxMs: 15_000 },
      random: Math.random,
      client: { name: "ki-panel", version: "0.0.0" },
      ...opts,
    };
  }

  // ---------------------------------------------------------- observability

  get state(): ConnState {
    return this._state;
  }
  /** Agent info from the last successful handshake. */
  get hello(): HelloResult | null {
    return this._hello;
  }
  get lastError(): string | null {
    return this._lastError;
  }
  /** useSyncExternalStore-compatible. */
  onStateChange = (fn: () => void): (() => void) => {
    this.stateListeners.add(fn);
    return () => this.stateListeners.delete(fn);
  };
  getState = (): ConnState => this._state;

  private setState(s: ConnState): void {
    if (this._state === s) return;
    this._state = s;
    for (const fn of [...this.stateListeners]) fn();
  }

  // -------------------------------------------------------------- lifecycle

  connect(): void {
    if (this.wantOpen) return;
    this.wantOpen = true;
    this.attempt = 0;
    this.setState("connecting");
    this.dial();
  }

  /** Closes for good; pending calls reject, subscriptions end. */
  close(): void {
    this.wantOpen = false;
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
    this.reconnectTimer = null;
    const s = this.sock;
    this.sock = null;
    this.failPending(new RpcError(ErrorCode.ConnectionLost, "connection closed"));
    this.rejectWaiters(new RpcError(ErrorCode.ConnectionLost, "connection closed"));
    for (const sub of [...this.subs]) this.endSub(sub, "ended");
    this.setState("closed");
    if (s) {
      detach(s);
      s.close(1000);
    }
  }

  /** Forces a new connection attempt now (e.g. after signing in again). */
  reconnect(): void {
    if (!this.wantOpen) {
      this.connect();
      return;
    }
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
    this.reconnectTimer = null;
    if (this._state === "unauthorized") this.setState("reconnecting");
    if (this.sock) return;
    this.attempt = 0;
    this.dial();
  }

  private url(): string {
    if (this.opts.url) return this.opts.url;
    const l = globalThis.location;
    return `${l.protocol === "https:" ? "wss" : "ws"}://${l.host}/rpc`;
  }

  private dial(): void {
    const gen = ++this.generation;
    let sock: SocketLike;
    try {
      sock = this.opts.createSocket
        ? this.opts.createSocket(this.url())
        : (new WebSocket(this.url()) as unknown as SocketLike);
    } catch (e) {
      this._lastError = String(e);
      this.scheduleReconnect();
      return;
    }
    this.sock = sock;
    let helloDone = false;

    sock.onopen = () => {
      if (gen !== this.generation) return;
      // Pipelined: requests may follow agent.hello without waiting.
      this.rawCall<HelloResult>(
        "agent.hello",
        { protocol: PROTOCOL_VERSION, client: this.opts.client },
        this.opts.timeoutMs,
      ).then(
        (res) => {
          if (gen !== this.generation) return;
          helloDone = true;
          this._hello = res;
          this._lastError = null;
          this.attempt = 0;
          this.setState("open");
          this.resolveWaiters();
          for (const sub of this.subs) void this.startSub(sub);
        },
        (err: unknown) => {
          if (gen !== this.generation) return;
          if (err instanceof RpcError && err.code === ErrorCode.IncompatibleVersion) {
            this._lastError = err.message;
            this.fatal(err);
            return;
          }
          // Anything else: drop the link and let the normal path retry.
          this._lastError = err instanceof Error ? err.message : String(err);
          sock.close();
        },
      );
    };
    sock.onmessage = (ev) => {
      if (gen !== this.generation) return;
      this.handleFrame(ev.data);
    };
    sock.onerror = () => {
      /* followed by onclose */
    };
    sock.onclose = () => {
      if (gen !== this.generation) return;
      this.sock = null;
      this.failPending(new RpcError(ErrorCode.ConnectionLost, "connection lost"));
      for (const sub of this.subs) this.suspendSub(sub);
      this.early.clear();
      if (!this.wantOpen) return;
      this.setState("reconnecting");
      if (!helloDone && this.opts.probe) {
        this.opts.probe().then(
          (r) => {
            if (!this.wantOpen || gen !== this.generation) return;
            if (r === "unauthenticated") this.unauthorized();
            else this.scheduleReconnect();
          },
          () => {
            if (this.wantOpen && gen === this.generation) this.scheduleReconnect();
          },
        );
        return;
      }
      this.scheduleReconnect();
    };
  }

  private fatal(err: RpcError): void {
    this.wantOpen = false;
    const s = this.sock;
    this.sock = null;
    this.failPending(err);
    this.rejectWaiters(err);
    for (const sub of [...this.subs]) this.endSub(sub, "error", err);
    this.setState("closed");
    if (s) {
      detach(s);
      s.close(1000);
    }
  }

  private unauthorized(): void {
    const err = new RpcError(ErrorCode.Unauthorized, "not signed in");
    this.rejectWaiters(err);
    for (const sub of this.subs) this.notifySub(sub, "suspended");
    this.setState("unauthorized");
  }

  private scheduleReconnect(): void {
    if (!this.wantOpen) return;
    const { baseMs, maxMs } = this.opts.backoff;
    const ceil = Math.min(maxMs, baseMs * 2 ** Math.min(this.attempt, 20));
    const delay = Math.round(ceil * (0.5 + this.opts.random() * 0.5));
    this.attempt++;
    this.setState("reconnecting");
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      if (this.wantOpen) this.dial();
    }, delay);
  }

  private waitReady(timeoutMs: number, signal?: AbortSignal): Promise<void> {
    if (this._state === "open") return Promise.resolve();
    if (this._state === "closed" || this._state === "unauthorized") {
      return Promise.reject(
        new RpcError(
          this._state === "unauthorized" ? ErrorCode.Unauthorized : ErrorCode.ConnectionLost,
          this._state === "unauthorized" ? "not signed in" : "connection closed",
        ),
      );
    }
    return new Promise((resolve, reject) => {
      const w = {
        resolve: () => {
          clearTimeout(t);
          resolve();
        },
        reject: (e: unknown) => {
          clearTimeout(t);
          reject(e);
        },
      };
      const t = setTimeout(() => {
        this.readyWaiters = this.readyWaiters.filter((x) => x !== w);
        reject(new RpcError(ErrorCode.Timeout, "not connected"));
      }, timeoutMs);
      signal?.addEventListener("abort", () => {
        this.readyWaiters = this.readyWaiters.filter((x) => x !== w);
        w.reject(new RpcError(ErrorCode.ConnectionLost, "aborted"));
      });
      this.readyWaiters.push(w);
    });
  }

  private resolveWaiters(): void {
    const ws = this.readyWaiters;
    this.readyWaiters = [];
    for (const w of ws) w.resolve();
  }

  private rejectWaiters(e: unknown): void {
    const ws = this.readyWaiters;
    this.readyWaiters = [];
    for (const w of ws) w.reject(e);
  }

  // ------------------------------------------------------------------ calls

  /** Calls a method. Waits for the connection to be open (within the timeout). */
  async call<T = unknown>(method: string, params?: unknown, o: CallOptions = {}): Promise<T> {
    const timeout = o.timeoutMs ?? this.opts.timeoutMs;
    await this.waitReady(timeout, o.signal);
    return this.rawCall<T>(method, params, timeout, o.signal);
  }

  private rawCall<T>(method: string, params: unknown, timeoutMs: number, signal?: AbortSignal): Promise<T> {
    return new Promise<T>((resolve, reject) => {
      const sock = this.sock;
      if (!sock) {
        reject(new RpcError(ErrorCode.ConnectionLost, "not connected"));
        return;
      }
      if (signal?.aborted) {
        reject(new RpcError(ErrorCode.ConnectionLost, "aborted"));
        return;
      }
      const id = this.nextID++;
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new RpcError(ErrorCode.Timeout, `${method} timed out`));
      }, timeoutMs);
      const onAbort = () => {
        clearTimeout(timer);
        this.pending.delete(id);
        reject(new RpcError(ErrorCode.ConnectionLost, "aborted"));
      };
      signal?.addEventListener("abort", onAbort, { once: true });
      this.pending.set(id, {
        resolve: resolve as (v: unknown) => void,
        reject,
        timer,
        cleanup: () => signal?.removeEventListener("abort", onAbort),
      });
      const msg: Record<string, unknown> = { jsonrpc: "2.0", id, method };
      if (params !== undefined) msg.params = params;
      try {
        sock.send(JSON.stringify(msg));
      } catch (e) {
        clearTimeout(timer);
        this.pending.delete(id);
        reject(new RpcError(ErrorCode.ConnectionLost, String(e)));
      }
    });
  }

  private failPending(err: RpcError): void {
    const all = [...this.pending.values()];
    this.pending.clear();
    for (const p of all) {
      clearTimeout(p.timer);
      p.cleanup?.();
      p.reject(err);
    }
  }

  /** Listens for a notification that is not a subscription update. Returns an unsubscriber. */
  on(method: string, fn: (params: unknown) => void): () => void {
    let set = this.notifHandlers.get(method);
    if (!set) this.notifHandlers.set(method, (set = new Set()));
    set.add(fn);
    return () => set.delete(fn);
  }

  // --------------------------------------------------------------- framing

  private handleFrame(raw: unknown): void {
    if (typeof raw !== "string") return;
    let msg: unknown;
    try {
      msg = JSON.parse(raw);
    } catch {
      return;
    }
    if (Array.isArray(msg)) {
      for (const m of msg) this.handleMessage(m);
    } else {
      this.handleMessage(msg);
    }
  }

  private handleMessage(m: unknown): void {
    if (typeof m !== "object" || m === null) return;
    const o = m as Record<string, unknown>;
    if (typeof o.method === "string" && o.id === undefined) {
      this.handleNotification(o.method, o.params);
      return;
    }
    if (typeof o.id === "number") {
      const p = this.pending.get(o.id);
      if (!p) return;
      this.pending.delete(o.id);
      clearTimeout(p.timer);
      p.cleanup?.();
      if (o.error && typeof o.error === "object") {
        const e = o.error as { code?: number; message?: string; data?: unknown };
        p.reject(new RpcError(e.code ?? ErrorCode.Internal, e.message ?? "error", e.data));
      } else {
        p.resolve(o.result);
      }
    }
  }

  private handleNotification(method: string, params: unknown): void {
    if (method.endsWith(".update") && isUpdate(params)) {
      const sub = this.byID.get(params.subscription);
      if (sub) {
        this.deliver(sub, params);
      } else {
        // The first update may precede the subscribe response.
        let q = this.early.get(params.subscription);
        if (!q) {
          if (this.early.size >= MAX_EARLY_BUFFER) return;
          this.early.set(params.subscription, (q = []));
        }
        if (q.length < 1024) q.push(params);
      }
      return;
    }
    const set = this.notifHandlers.get(method);
    if (set) for (const fn of [...set]) fn(params);
  }

  // ---------------------------------------------------------- subscriptions

  subscribe<TData>(opts: SubscribeOptions<TData>): Subscription {
    const sub: Sub = {
      opts: opts as SubscribeOptions<unknown>,
      id: null,
      lastSeq: 0,
      lastData: undefined,
      cancelled: false,
      state: "pending",
      gen: 0,
    };
    this.subs.add(sub);
    if (this._state === "open") void this.startSub(sub);
    return {
      unsubscribe: () => {
        if (sub.cancelled) return;
        sub.cancelled = true;
        this.endSub(sub, "ended", undefined, true);
      },
    };
  }

  private async startSub(sub: Sub): Promise<void> {
    if (sub.cancelled) return;
    const gen = ++sub.gen;
    const resumeParams =
      sub.lastSeq > 0 && sub.opts.resume
        ? sub.opts.resume({ seq: sub.lastSeq, data: sub.lastData })
        : undefined;
    const params = { ...(sub.opts.params ?? {}), ...(resumeParams ?? {}) };
    sub.lastSeq = 0;
    try {
      const res = await this.rawCall<{ subscription: string }>(
        sub.opts.method,
        Object.keys(params).length ? params : undefined,
        this.opts.timeoutMs,
      );
      if (sub.cancelled) {
        void this.rawCall(unsubMethod(sub.opts.method), { subscription: res.subscription }, 5000).catch(() => {});
        return;
      }
      if (gen !== sub.gen) return; // superseded by a reconnect
      sub.id = res.subscription;
      this.byID.set(sub.id, sub);
      sub.opts.onSubscribed?.(res);
      this.notifySub(sub, "active");
      const buffered = this.early.get(sub.id);
      this.early.delete(sub.id);
      if (buffered) {
        buffered.sort((a, b) => a.seq - b.seq);
        for (const u of buffered) {
          if (sub.id !== res.subscription) break;
          this.deliver(sub, u);
        }
      }
    } catch (e) {
      if (sub.cancelled || gen !== sub.gen) return;
      if (e instanceof RpcError && e.code === ErrorCode.ConnectionLost) return; // retried on reconnect
      this.endSub(sub, "error", e instanceof RpcError ? e : new RpcError(ErrorCode.Internal, String(e)));
    }
  }

  private deliver(sub: Sub, u: RawUpdate): void {
    if (sub.cancelled || u.seq <= sub.lastSeq) return; // stale or duplicate
    const kind = sub.opts.kind;
    if (u.gap) {
      sub.lastSeq = u.seq;
      if (kind === "ordered") sub.opts.onGap?.(u.gap, false);
      return;
    }
    if (u.data === undefined) return;
    if (kind === "ordered" && u.seq > sub.lastSeq + 1) {
      sub.opts.onGap?.({ from: sub.lastSeq + 1, to: u.seq - 1 }, true);
    }
    sub.lastSeq = u.seq;
    sub.lastData = u.data;
    sub.opts.onData(u.data, { seq: u.seq });
    if (sub.opts.isFinal?.(u.data)) {
      // The server closes the subscription itself after the final update.
      this.endSub(sub, "ended", undefined, false);
    }
  }

  private suspendSub(sub: Sub): void {
    if (sub.id) this.byID.delete(sub.id);
    sub.id = null;
    sub.gen++;
    if (sub.state !== "ended" && sub.state !== "error") this.notifySub(sub, "suspended");
  }

  private endSub(sub: Sub, state: "ended" | "error", err?: RpcError, sendUnsub = false): void {
    const id = sub.id;
    if (id) this.byID.delete(id);
    sub.id = null;
    sub.gen++;
    this.subs.delete(sub);
    if (sendUnsub && id && this.sock && this._state === "open") {
      void this.rawCall(unsubMethod(sub.opts.method), { subscription: id }, 5000).catch(() => {});
    }
    this.notifySub(sub, state, err);
  }

  private notifySub(sub: Sub, state: SubscriptionState, err?: RpcError): void {
    if (sub.state === state && !err) return;
    sub.state = state;
    sub.opts.onState?.(state, err);
  }
}

function detach(s: SocketLike): void {
  s.onopen = s.onmessage = s.onclose = s.onerror = null;
}

function nsOf(method: string): string {
  return method.replace(/\.subscribe$/, "");
}
function unsubMethod(method: string): string {
  return nsOf(method) + ".unsubscribe";
}

function isUpdate(p: unknown): p is RawUpdate {
  if (typeof p !== "object" || p === null) return false;
  const o = p as Record<string, unknown>;
  return typeof o.subscription === "string" && typeof o.seq === "number";
}
