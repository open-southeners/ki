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

## Subscription interval is advisory (M1)

- **Where:** `internal/rpc/subscription.go` (`Subscriptions.Open`).
- **What:** `interval` is stored but not enforced by the rpc package; each
  producer runs its own ticker. A producer that ignores it could exceed the
  client's requested rate.
- **Fix:** acceptable for now; if more producers appear, add a rate limiter in
  `Subscription.Send` for Snapshot kinds.

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

## Policy rules not yet covered (M2 follow-up)

- **Where:** `internal/policy/check.go`.
- **What:** `group_add`, `pull_policy`, `ulimits`, `develop.watch` and
  `post_start`/`pre_stop` hooks are not inspected. None is a known host
  escape on its own. (`provider` services and non-default `runtime` are now
  rejected: rules `provider` and `runtime`.)
- **Fix:** review each; reject any that turns out to run on the host or to
  widen privileges.

## Go `rpc.Peer` dispatches incoming notifications concurrently (M2)

- **Where:** `internal/rpc/peer.go` (notification dispatch).
- **What:** the wire order of subscription updates is correct (single writer),
  but a Go consumer using `rpc.Peer` as a client gets notification handlers run
  in separate goroutines, so ordered streams (container logs) can be observed
  out of order. Matters once the agent itself consumes ordered streams, or for
  anyone reusing the package as a client library.
- **Fix:** dispatch notifications serially per subscription id (or per peer),
  and state "order by `seq`" in the OpenRPC `SubscriptionUpdate` description.

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

## `ReasonRefNotFound` doc comment mentions branches only (M4)

- **Where:** `internal/git/errors.go` (`ReasonRefNotFound`).
- **What:** the reason now also covers tags, shas and "commit not on ref" (`internal/git/refs.go`),
  but its comment still says "branch".
- **Fix:** reword the comment; no behaviour change.

## `deploys logs` without `--follow` guesses when a running deploy's log ends (M4)

- **Where:** `internal/cli/deploys.go` (`deploys logs`).
- **What:** the API only streams logs, so for a deploy that is still running the CLI stops after
  500 ms without new replayed lines. On a slow link it may stop before the stored log is replayed.
- **Fix:** add a non-streaming `deploys.logs {id, since_seq, limit}` method, or include the stored
  log's last `seq` in `deploys.get` so the client knows when the replay is complete.

## Project delete cancels deploys even when the delete then fails (M4)

- **Where:** `internal/projects/service.go` (`Delete` runs `OnDelete` hooks before the lock).
- **What:** if pruning fails afterwards (engine unreachable, concurrent delete), the project survives
  but its running and queued deploys were already cancelled and recorded as interrupted.
- **Fix:** acceptable; if it matters, check the runtime is reachable before running the hooks.

## Webhook dedupe and the webhooks package's store dependency (M4)

- **Where:** `internal/webhooks` (`dedupe.go`, import of `internal/store` for `ErrNotFound`).
- **What:** delivery-id dedupe is in memory, so a redelivery after an agent restart deploys again
  (harmless: same commit). Importing `internal/store` only for the sentinel pulls the sqlite driver
  into the package's build, making its tests slow to build.
- **Fix:** define `webhooks.ErrNotFound` and translate in the wiring adapter; persist dedupe only if
  duplicate deploys become a problem.

## A deploy waiting for the project lock cannot be cancelled (M4)

- **Where:** `internal/projects/env.go` (`WithProject` takes the lock without a context).
- **What:** a deploy queued behind a long `projects.update`/`verify` (git over a slow remote) keeps
  waiting after shutdown or cancellation until the lock is free. Delete is unaffected (its hook
  runs before the lock).
- **Fix:** make the per-project lock context-aware (channel-based semaphore) and pass ctx.

## Triggering a deploy while its project is being deleted returns an internal error (M4)

- **Where:** `internal/deploys/manager.go` (`Cancel` marks the slug as deleting).
- **What:** `deploys.trigger` during a delete answers −32603 instead of a clear error; if the delete
  then fails, triggers keep failing until the slug's worker exits.
- **Fix:** answer `project_not_ready` (or NotFound) while deleting, and clear the marker when the
  delete fails (e.g. a second hook after `Delete` returns).

## A sha that is not on the trigger branch fails at checkout, not at trigger (M4)

- **Where:** `internal/deploys/refs.go`.
- **What:** `deploys.trigger {ref: <sha>}` is accepted and the deploy ends `failed`/`fetch_failed`
  ("commit not on ref") instead of an immediate `ref_not_allowed`.
- **Fix:** acceptable; checking up front needs a fetch. Revisit if callers rely on the sync error.

## Old Compose plugins inline `env_file` despite `--no-env-resolution` (M4)

- **Where:** `internal/compose/runner.go` (`Config`), `install.sh` (only checks that `docker compose version` works).
- **What:** Compose 2.40.3 (what `docker:28-cli` ships) still reads `env_file` entries during
  `config --no-env-resolution` and inlines them into `environment`, so `env_file: /etc/passwd` is
  neither reported by the `env_file` policy rule nor kept out of the container: a file-read
  primitive for whoever controls the Compose file. v5.1.0 behaves as intended, which is why the image
  pins `docker/compose-bin:v5.1.0`. Hosts using the packaged install run whatever plugin they have.
- **Mitigated:** `compose.CheckIncludes` now rejects `env_file`/`label_file` paths that are not inside
  the checkout (raw YAML, every included/extended file, symlinks resolved) before Compose runs, so
  the file read is blocked on any Compose version. Remaining risk: a future Compose feature that
  reads another file during `config`.
- **Fix:** still worth a start-up `docker compose version` check with a known-good minimum, stated in
  the README and `install.sh`.

## The dind `test` service fails inside a Git worktree (M4)

- **Where:** `docker-compose.yml` (`test` service mounts `.` at `/src`).
- **What:** in a worktree, `/src/.git` is a file pointing at the main repository's absolute path,
  which does not exist in the container, so every Git subprocess started with the working directory
  under `/src` fails with "not a git repository" (about a dozen packages' tests). It passes in a
  normal clone. Worked around locally with an uncommitted override that mounts the main `.git` at
  the same path.
- **Fix:** run the Git fixtures with a neutral working directory (for example `cmd.Dir = t.TempDir()`
  in the test helpers and in `internal/git` when no directory is needed) or `GIT_CEILING_DIRECTORIES`.


## `/hooks` is routed only on server hostnames, not on project domains (M4a)

- **Where:** `internal/edge/gen.go` (`Dynamic`: the `ki-webhooks` router).
- **What:** deliberate narrowing of the original plan. `PathPrefix(/hooks/)` goes to the agent's webhook
  listener only on the server's own hostnames (`edge.domain`, hostnames with an installed certificate),
  on unknown hosts and on the bare IP. Routing it on project domains would shadow an application's own
  `/hooks` paths. Webhook senders therefore need the server hostname or the IP, not an app domain.
- **Fix:** none needed unless a per-project opt-in (for example a `ki.expose.hooks` label) is wanted.

## The edge container keeps `DAC_OVERRIDE` (M4a)

- **Where:** `internal/edge/gen.go` (`Compose`: `cap_add`).
- **What:** the proxy runs as root with `cap_drop: ALL` plus `NET_BIND_SERVICE` and `DAC_OVERRIDE`. The
  extra capability exists because the agent's key files (`selfsigned.key`, `certs/*.key`) are 0600 and
  owned by the agent's user (`ki-agent` under systemd), which root without `DAC_OVERRIDE` cannot read.
  The container has host networking, a read-only root filesystem and read-only config mounts.
- **Fix:** run the container as the agent's uid/gid (needs it passed in, and `NET_BIND_SERVICE` is then
  enough) or hand the key files to a dedicated group.

## Webhook listener and the edge only meet over `127.0.0.1` plain HTTP (M4a)

- **Where:** `internal/cli/run.go` (`effectiveWebhooksListen`), `internal/edge/gen.go`.
- **What:** the edge forwards `/hooks/` to `http://127.0.0.1:<port>`. An explicit `webhooks.listen` that
  is neither loopback nor a wildcard, or one with `webhooks.tls_*`, cannot be reached that way; the
  agent logs a warning and keeps the listener as configured. In the dev stack, an edge running inside
  the `dind` sidecar cannot reach the agent container's loopback either (the smoke test shared the
  network namespace with `network_mode: service:dind`).
- **Fix:** if needed, let the edge target a configured upstream (host, scheme) for webhooks.


## Control-plane `Manager.Apply` races with `Run` starting the connector (M1b)

- **Where:** `internal/controlplane/manager.go` (`start` writes `mc.cancel`/`mc.done` under `m.mu`;
  `Apply` takes `old := m.cur` under `m.mu`, releases it, then `stop(old)` reads them unlocked).
- **What:** if `Apply` (e.g. enrollment finishing) runs while `Run` is starting, `stop` can read
  `cancel == nil` and return without stopping a connector that `Run` starts a moment later, leaving the
  old connection running next to the new one. Caught intermittently by `go test -race`
  (`TestManagerConcurrentApplyAndStatus`, about 1 in 10 runs). Present since M1b; not touched by M4/M4a.
- **Fix:** snapshot `cancel`/`done` under `m.mu` (or mark `old` as replaced so `Run` never starts a
  connector that is no longer `m.cur`), and make `stop` wait on a value read under the lock.

## Setup link picks the first non-loopback IPv4, which may be a bridge address (M4b)

- **Where:** `internal/cli/setupbase.go` (`firstNonLoopbackIPv4`).
- **What:** with no `panel.public_url`/`edge.domain`/installed hostname, the link host is the first
  non-loopback, non-link-local IPv4 address in interface order. On a host that runs Docker this can be
  a bridge such as `docker0` (172.17.0.1) instead of the address the operator reaches the server on.
  Without the edge the link points at `http://<api.listen>` (loopback), not at an IP.
- **Fix:** prefer the source address of the default route (for example by dialing a UDP socket to a
  public address without sending) and skip `docker0`/`br-*`/`veth*` interfaces; `panel.public_url`
  already overrides it.

## `containers` logs test races the subscription close (M2)

- **Where:** `internal/containers/containers_test.go` (`TestLogsStreamToEOF`, last assertion).
- **What:** the test expects `containers.logs.unsubscribe` to answer `-32004` right after the `eof`
  update, but the subscription is removed after that update is sent, so the unsubscribe can win the
  race and succeed (`err = <nil>`). Seen once in the dind run (about 1 in 100+ runs); not touched by
  M4b.
- **Fix:** remove the subscription before sending the final `eof` update, or poll until the
  unsubscribe reports `-32004`.

## Service actions are not serialised against a running deploy (M5b)

- **Where:** `internal/projects/services.go` (`StartService`, `StopService`, `RestartService`).
- **What:** the actions act on the containers that exist at that moment and do not take the
  per-project deploy lock, so a restart or stop during a deploy can race with `compose up` (for
  example a container stopped just before the deploy recreates it, or a restart of a container
  being replaced).
- **Fix:** refuse (or queue) service actions while a deploy of the project is running, via the
  deploys manager's per-project state.

## Web stats view matches container ids by prefix (M5b)

- **Where:** `web/src/components/ops/ServicesTab.tsx` (stats lookup by container id).
- **What:** `containers.stats` samples and `projects.services` containers may carry full or short
  ids, so the services table matches them with `startsWith` in both directions. Two ids sharing a
  prefix could in theory be mixed up.
- **Fix:** have both methods return the same id form (full 64-digit id) and compare for equality.

## Two table component sets in the web app (M5b)

- **Where:** `web/src/components/ui/table.tsx` (operations pages) and
  `web/src/components/admin/table.tsx` (admin pages).
- **What:** the operations and admin pages were written in parallel and each added its own table
  primitives with slightly different styling.
- **Fix:** keep the `ui/` shadcn one and port the admin pages to it.


## install.sh cannot install an unsigned local build when cosign is installed (M6)

- **Where:** `install.sh` (`verify_signature`), `scripts/local-release.sh`.
- **What:** with `cosign` on the machine the installer insists on `checksums.txt.sigstore.json`,
  which a local build does not have, so `KI_INSTALL_BASE_URL=file://...` installs fail there.
- **Fix:** add an explicit opt-out (for example `KI_INSTALL_SKIP_SIGNATURE=1`, only honoured for
  `file://` base URLs) and document it next to the local-release instructions.

## Docker config dir is not covered by the systemd sandbox when moved (M6)

- **Where:** `packaging/systemd/ki-agent.service`, `docker.config_dir`.
- **What:** the default `<data_dir>/docker` is inside `ReadWritePaths`; a custom location needs a
  drop-in. Documented, but the agent only fails at the first `docker login`/deploy.
- **Fix:** optionally check writability of `docker.config_dir` at start-up and log a warning.

## Tunnel readiness is not tested against real Cloudflare (M6b)

- **Where:** `internal/tunnel/manager.go` (`probe`).
- **What:** tests use a fake `/ready` server and the dind smoke only ran with a syntactically valid
  but fake token (the connector reached Cloudflare and got "Failed to get tunnel", so the state
  went `starting` then `disconnected`). The `/ready` JSON fields (`readyConnections`) were taken
  from the cloudflared source (`metrics/readiness.go`); Cloudflare's docs only promise HTTP 200 when
  connected. A real connected tunnel was never exercised.
- **Fix:** try once with a real tunnel before a release; the agent already falls back to
  `connections: 0` when the body has no `readyConnections`.

## The tunnel container runs as the agent's uid (M6b)

- **Where:** `internal/tunnel/gen.go` (`user:`), `internal/cli/run.go`.
- **What:** the connector runs as the agent's uid:gid so it can read the 0600 token without
  `DAC_OVERRIDE`. The smoke ran with the agent as root (dev container); the non-root systemd case
  (`ki-agent` user, distroless image without a passwd entry for that uid) was only reasoned about.
- **Fix:** smoke-test with the packaged unit on a real host; if `cloudflared` needs a `HOME`, add
  `tmpfs` plus `HOME=/tmp` to the generated service.

## Panel has no UI to set or remove the tunnel token (M6b)

- **Where:** `web/src/components/ops/TunnelCard.tsx`.
- **What:** the token is only set through the CLI or the control plane (the panel transport is
  deliberately refused by `tunnel.configure`), so the card is read-only.
- **Fix:** none needed unless the transport policy changes.

## `TestIntervalAdaptation` is timing-sensitive (M5)

- **Where:** `internal/monitor/manager_test.go` (`TestIntervalAdaptation`).
- **What:** failed once in a full `go test ./...` run (no `-race`) and passed on reruns; the test
  depends on real timer scheduling of the adaptive sampling loop.
- **Fix:** drive the schedule with an injected clock/ticker instead of wall-clock waits, or assert the
  computed interval directly rather than observed sample timing.

## Compose validation rejects required-variable interpolation (M6c)

- **Where:** `internal/compose/validate.go` (`Validate`).
- **What:** a compose file is validated with an empty `.env`, so `${VAR:?message}` required-variable
  interpolation fails at `projects.create` / `projects.compose.set` time even when the variable is
  set on the project, and the file cannot be stored.
- **Fix:** validate with the project's env names (values sealed, e.g. placeholders) or skip
  required-variable errors during validation; the deploy itself resolves the real values.

