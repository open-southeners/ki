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

## Compose must be normalized without env resolution (M4)

- **Where:** M4 deploy pipeline (`docker compose config` call before `internal/policy.Check`).
- **What:** by default `docker compose config` reads `env_file` contents itself —
  even from outside the project root — and drops the key, so the `env_file`
  policy rule never sees it. `include`/`extends` also make compose read files
  outside the root while normalizing untrusted YAML.
- **Fix:** run `docker compose config --format json --no-env-resolution`, with
  a minimal environment and the project dir as cwd; consider rejecting
  `include`/`extends` sources outside the root before invoking compose.

## Policy rules not yet covered (M2 follow-up)

- **Where:** `internal/policy/check.go`.
- **What:** `group_add`, `runtime`, `pull_policy`, `ulimits`, `develop.watch`,
  `post_start`/`pre_stop` hooks and `provider:` services are not inspected.
  None is a known host escape on its own, but `runtime` (custom OCI runtimes)
  and `provider` (runs host-side plugins) deserve a look.
- **Fix:** review each; reject `provider` services and non-default `runtime`
  unless allow-listed in config.

## Go `rpc.Peer` dispatches incoming notifications concurrently (M2)

- **Where:** `internal/rpc/peer.go` (notification dispatch).
- **What:** the wire order of subscription updates is correct (single writer),
  but a Go consumer using `rpc.Peer` as a client gets notification handlers run
  in separate goroutines, so ordered streams (container logs) can be observed
  out of order. Matters once the agent itself consumes ordered streams, or for
  anyone reusing the package as a client library.
- **Fix:** dispatch notifications serially per subscription id (or per peer),
  and state "order by `seq`" in the OpenRPC `SubscriptionUpdate` description.

## Dev compose `ki-agent` service has no Docker engine (M2)

- **Where:** `docker-compose.yml` (`ki-agent` service).
- **What:** no `KI_DOCKER_HOST` and not attached to `testnet`, so the dev agent
  logs "runtime unreachable" and every `containers.*` call returns −32008.
- **Fix:** set `KI_DOCKER_HOST=tcp://dind:2375`, add `testnet` to its networks
  and `depends_on: dind (healthy)`.

## Runtime-unavailable cause is not logged (M2)

- **Where:** `internal/containers/containers.go` (error mapping to −32008).
- **What:** callers get the fixed message "container runtime unavailable"; the
  underlying engine error is discarded, which hampers diagnosing socket
  permission problems (e.g. missing `docker` group).
- **Fix:** log the wrapped error at warn level (rate-limited) when mapping
  `runtime.ErrUnavailable`.

## Control-plane `last_error` carries verbose library wording (M1b)

- **Where:** `internal/controlplane/controlplane.go` (dial error → `Status.LastError`).
- **What:** dial failures surface the `coder/websocket` chain verbatim, e.g.
  `dial: failed to WebSocket dial: failed to send handshake request: Get "ws://…"`.
  Credentials are redacted, but the text is long and library-specific.
- **Fix:** map common failures to short causes (`connection refused`,
  `HTTP 401`, `TLS: unknown authority`, `timeout`) and keep the full chain in logs only.

## Control-plane rejection status is only in free text (M1b)

- **Where:** `controlplane.Status` / `agent.info.control_plane`.
- **What:** a 401/403 rejection is visible only inside `last_error` (`HTTP 401`);
  a UI cannot tell "bad token" from "network down" without parsing text.
- **Fix:** add an optional `last_http_status` field (additive, minor protocol bump).

## Control-plane connection can be served before `Server.Serve` starts (M1b)

- **Where:** `internal/cli/run.go` (connector goroutine started before `srv.Serve`).
- **What:** harmless today (tracking is mutex-guarded, `closed` only set on
  shutdown), but the ordering is implicit.
- **Fix:** start the connector after the listeners are up, or document that
  `ServeConn` is valid before `Serve`.

## Subscription updates can precede the subscribe response (M1b)

- **Where:** `internal/rpc/subscription.go` (`Subscriptions.Open` starts the pump
  before the handler's response is queued).
- **What:** the first `*.update` for a new subscription id can reach the client
  before the `{subscription}` result — most likely for container logs with `tail`.
  `docs/protocol.md` tells clients to buffer updates for unknown ids.
- **Fix:** hold the pump until the response is written (e.g. a start gate released
  after the response is enqueued), then relax the docs.

## Reserved error code −32005 is never returned (M1b)

- **Where:** `internal/rpc/errors.go`, `api/openrpc.json` (−32005 Overloaded).
- **What:** still defined and documented as reserved; no overload path exists yet. (−32001
  Unauthorized is now used for local-only methods since M3b.)
- **Fix:** use −32005 when the per-peer concurrency cap lands, or drop it before 1.0.

## Outbound contract leaves some behaviour unspecified (M1b)

- **Where:** `docs/protocol.md` ("Outbound connection").
- **What:** not stated: whether the agent sends a close frame when the control plane
  closes first; whether the control plane should reject unknown `Ki-Protocol`
  majors before upgrading; which of 401/403 to use.
- **Fix:** decide and document (suggest: control plane may reject an incompatible
  `Ki-Protocol` major with HTTP 426; agent treats it like 401/403).

## Example config is not exercised by tests (M1b)

- **Where:** `packaging/config.example.yaml`, `internal/config`.
- **What:** no test loads the shipped example, so it can drift from the config
  struct (it was only verified manually during the M1b smoke test).
- **Fix:** add a config test that loads the example (with paths overridden) and
  asserts it validates.

## Git URLs: bracketed IPv6 scp-style form unsupported (M3)

- **Where:** `internal/git/url.go` (`ParseURL`).
- **What:** `user@[::1]:repo.git` is rejected; scp-style URLs also require a `user@`
  (deliberately, to avoid `ext:`/local-path ambiguity). `ssh://user@[::1]/repo.git` works.
- **Fix:** acceptable; document "use `ssh://` for IPv6 hosts" if anyone hits it.

## `--socket` flag shows up on `ki-agent run` (M3)

- **Where:** `internal/cli/root.go` (persistent `--socket` flag).
- **What:** the flag is persistent on the root command, so `ki-agent run --help` lists it although
  the daemon takes its socket path from config only.
- **Fix:** move it to a persistent flag on a client-commands group, or make `run` honour it as an
  override of `socket.path`.

## CLI client loses gap ranges in subscriptions (M3)

- **Where:** `internal/client/client.go` (`Subscribe` callback `fn(seq, data)`).
- **What:** a `gap` marker reaches the callback as an update with empty `data`; consumers infer
  the dropped count from the previous `seq`. A seq that never arrives is skipped after 500 ms,
  which assumes streams start at seq 1.
- **Fix:** pass a typed update (`Update{Seq, Data, Gap *struct{From, To int64}}`) to the
  callback; start reordering from the first seq received instead of assuming 1.

## `projects.update` cannot clear optional fields (M3)

- **Where:** `internal/projects/service.go` (`UpdateParams`: empty string = unchanged).
- **What:** once `compose_file` or `token_user` is set explicitly there is no way back to
  auto-detection / the default token user short of deleting the project.
- **Fix:** accept JSON `null` vs absent (pointer fields) or an explicit `reset: ["compose_file"]`.

## HTTPS-token create path has no success-path test (M3)

- **Where:** `internal/projects/service_test.go`, `internal/git`.
- **What:** tests have no HTTPS git server, so `https_token` create is only exercised against an
  unreachable host; the header construction is unit-tested separately.
- **Fix:** add a test with `httptest.NewTLSServer` wrapping `git http-backend` (CGI) that checks
  the `Authorization` header, with the test CA trusted via `GIT_SSL_CAINFO`.

## Server test `stubRuntime` panics on unimplemented methods (M3)

- **Where:** `internal/server/containers_test.go` (`stubRuntime` embeds `runtime.Runtime`).
- **What:** calling `PruneProject` (or any method not overridden) on it hits a nil interface and
  panics instead of failing the test clearly.
- **Fix:** implement the missing methods returning an error, or drop the embedding.

## `projects update --branch ""` is a silent no-op (M3)

- **Where:** `internal/cli/projects.go` / `internal/projects` (empty string = unchanged).
- **What:** the CLI sends an empty branch, the server ignores it, and the command reports success.
- **Fix:** reject empty `--branch`/`--tags`/`--compose` values in the CLI with a clear message.

## Example control plane advertises http:// discovery URLs behind a TLS proxy (M3b)

- **Where:** `examples/control-plane/enroll.go` (discovery document built from `Host` + `r.TLS`).
- **What:** behind a TLS-terminating proxy the discovery document lists `http://` endpoints, which
  the agent rejects for non-loopback hosts, so enrollment fails with `invalid_response`.
- **Fix:** derive endpoint URLs from a `-public-enroll-url` flag (or honour `X-Forwarded-Proto`
  behind a trusted proxy); the example is for local development, so low priority.

## Config with `control_plane.url` but no `token_file` fails to load even when enrolled (M3b)

- **Where:** `internal/config/config.go` (`ControlPlane.validate` requires the token file).
- **What:** an enrollment credential file replaces `url`/`token_file` at runtime, but a config that
  sets `url` without `token_file` is still rejected at load, before the credential file is read.
- **Fix:** require the token file only when no credential file exists (check in `run.go` after
  loading credentials), or defer the token check to the control-plane manager.

## Enrollment `expires_at` varies between runs of the example (M3b)

- **Where:** `examples/control-plane/enroll.go` (`expires_in`) and `internal/enroll` (`ExpiresAt`).
- **What:** in the M3b smoke test the reported expiry was ~20 min in one run and ~10 min in
  another, although the example issues 10-minute codes. Cosmetic, cause not investigated.
- **Fix:** check how `expires_in` is computed on both sides (and the 30-minute cap) and add a test
  asserting `ExpiresAt ≈ now + expires_in`.
