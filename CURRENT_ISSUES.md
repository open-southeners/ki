# Current issues

Discovered during orchestrated work; routed here instead of fixed inline.

## No per-peer cap on concurrent request handlers (M1)

- **Where:** `internal/rpc/peer.go` (incoming request dispatch).
- **What:** every incoming request (and every element of a batch) gets its own
  goroutine with no upper bound, so one authenticated peer can flood the agent.
- **Fix:** add a per-peer concurrency limit (option, e.g. `WithMaxInFlight(n)`)
  that answers excess requests with `Overloaded` (−32005).

## WebSocket heartbeat timeout closes without a close code (M1)

- **Where:** `internal/transport/ws/ws.go` (heartbeat loop).
- **What:** a peer that misses its pong is dropped with `CloseNow`, so it never
  receives a 1001/policy close frame. A graceful close would block ~5 s on a
  dead peer, which is why it was skipped.
- **Fix:** acceptable as is; revisit only if clients need to distinguish
  "heartbeat timeout" from a network drop (e.g. send the close frame
  non-blocking, then `CloseNow`).

## WebSocket rejects cross-origin browser connections (M1)

- **Where:** `internal/transport/ws/ws.go` (`websocket.Accept` options).
- **What:** the library's default Origin check rejects browser requests from
  another origin. Fine by design — browsers talk to the control plane, not the
  agent — but a future local web UI would be blocked.
- **Fix:** add a configurable `websocket.origin_patterns` → `AcceptOptions.OriginPatterns`
  if a browser client is ever supported.

## Subscription interval is advisory (M1)

- **Where:** `internal/rpc/subscription.go` (`Subscriptions.Open`).
- **What:** `interval` is stored but not enforced by the rpc package; each
  producer runs its own ticker. A producer that ignores it could exceed the
  client's requested rate.
- **Fix:** acceptable for now; if more producers appear, add a rate limiter in
  `Subscription.Send` for Snapshot kinds.

## WebSocket connection logs lack the remote address (M1)

- **Where:** `internal/transport/ws/ws.go` (`NewHandler` `onConn` callback),
  `internal/server/server.go` (connection logging).
- **What:** `onConn` receives no `*http.Request`/remote address, so
  "connection opened/closed" logs only the transport.
- **Fix:** pass connection metadata (remote addr, user agent) to `onConn`, or
  stash it in the ctx; honour `X-Forwarded-For` only behind a configured proxy.

## Long Unix socket paths fail with an opaque error (M1)

- **Where:** `internal/config/config.go` (`Validate`).
- **What:** `sun_path` is limited to ~108 bytes; a longer `socket.path` fails at
  bind time with `bind: invalid argument`.
- **Fix:** validate the length in `config.Validate` with a clear message.

## `agent.hello` client info and capabilities are discarded (M1)

- **Where:** `internal/agent/agent.go` (`hello` handler).
- **What:** `client` and `capabilities` are accepted and documented but not
  stored on the peer, so nothing can log or branch on them yet.
- **Fix:** store them via `Peer.Set` and include `client.name/version` in the
  connection log once something needs them.
