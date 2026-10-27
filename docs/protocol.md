# ki-agent protocol

This document is for people building a control plane (or any other client) that
talks to ki-agent. The agent does not care what is on the other end, as long as
it speaks this protocol.

The source of truth for every method, parameter and result schema is the
OpenRPC document [`api/openrpc.json`](../api/openrpc.json) (also returned by
`rpc.discover`). This document covers what it cannot: transports, connection
rules, subscription semantics, errors and versioning. A runnable client lives
in [`examples/control-plane`](../examples/control-plane).

## Message format

[JSON-RPC 2.0](https://www.jsonrpc.org/specification), UTF-8 JSON, params by
name (an object). Batches are accepted. Notifications from the agent have no
`id`. Request ids may be strings, numbers or null; the agent echoes them
verbatim.

## Transports

All transports carry the same protocol. Both ends are symmetric JSON-RPC
peers, but the agent API only ever flows client to agent.

| Transport | Who connects | Framing | Auth |
|---|---|---|---|
| Unix socket (`socket.path`, default `/run/ki-agent/ki.sock`) | local client to agent | one JSON message per line (`\n`), max 4 MiB per line | file mode |
| HTTP listeners (`api.listen`, `api.edge_listen`), WebSocket at `/rpc` | local scripts, the web panel, and clients through the edge | one JSON message per WebSocket text message, max 4 MiB | API token (`Authorization: Bearer ki_...`) or panel session cookie (401 otherwise); see [Authentication and roles](#authentication-and-roles) |
| Outbound connection (`control_plane.url`) | **agent to control plane** | as the WebSocket at `/rpc` | bearer token sent by the agent |

The outbound connection exists for agents that cannot be reached from outside
(NAT, firewalls): the agent dials the control plane, and once the WebSocket is
up, **the control plane is the JSON-RPC client**. The rest of this document
applies equally to all three transports unless it says otherwise.

## Authentication and roles

Capability `auth`. Every principal has a role, and every method has the
minimum role it needs (`x-role` in the OpenRPC document). Roles are ordered
`viewer < operator < admin`; a higher role includes the lower ones.

| Role | Methods |
|---|---|
| `viewer` | `rpc.discover`, `agent.*`, `runtime.info`, `containers.list`, `containers.inspect`, `containers.logs.subscribe`/`unsubscribe`, `containers.stats`, `containers.stats.subscribe`/`unsubscribe`, `host.stats`, `host.subscribe`/`unsubscribe`, `projects.get`, `projects.list`, `projects.services`, `projects.env.list` (names only), `deploys.get`, `deploys.list`, `deploys.subscribe`/`unsubscribe`, `edge.status`, `aws.status`, `tunnel.status`, `updates.status`, `auth.whoami` |
| `operator` | viewer, plus `containers.start`, `containers.stop`, `containers.restart`, `deploys.trigger`, `deploys.rollback`, `projects.services.start`/`stop`/`restart` |
| `admin` | operator, plus `containers.remove`, `projects.create`, `projects.update`, `projects.verify`, `projects.delete`, `projects.env.set`/`unset`, `projects.expose`/`unexpose`, `edge.csr`, `edge.certificate.install`/`import`/`remove`, `tunnel.configure`/`remove`, `updates.check`/`apply`, `enrollment.*`, `users.*`, `panel.setup`, `tokens.*`, `audit.list` |

A call below the required role fails with `-32001` and the message
`forbidden: requires <role>`. A method that is not in the role table is denied
for everyone, so new methods are never open by accident. `auth.whoami` returns
`{kind, name, role}` of the connection's principal.

### Principals

| Kind | Who | Role |
|---|---|---|
| `local` | A client on the Unix socket (file mode `0660`, group `ki-agent`) | `admin` |
| `control-plane` | The control plane over the outbound connection or enrollment | `admin` |
| `user` | A panel user signed in with a session cookie | the user's role |
| `token` | A client presenting an API token | the token's role |

### Listener model

The agent never trusts forwarded headers on a port it does not own. It has two
loopback-only HTTP listeners (validation rejects anything else) with the same
handlers:

- **Direct** (`api.listen`, default `127.0.0.1:8091`): for local scripts and
  development. `/rpc` accepts API tokens and, with the panel enabled, session
  cookies. The client address is the TCP peer; `X-Forwarded-For` is ignored.
- **Edge** (`api.edge_listen`, default `127.0.0.1:8092`, only when the edge is
  enabled): the only upstream the edge routes to. Session cookies work (panel
  enabled); API tokens are accepted on `/rpc` **only when
  `api.public_tokens: true`** (default false). The client address for rate
  limiting and the audit log is the last `X-Forwarded-For` hop, added by the
  edge.

A bearer token presented where tokens are not accepted is refused with 401
rather than falling back to a cookie. With `panel.enabled: false` (or `ki agent
run --no-panel`) `/auth/*` and `/` answer 404, the edge routes neither, and
`/rpc` keeps serving tokens on the direct listener (and through the edge when
`api.public_tokens` is set).

The edge routes `/rpc`, `/auth/` and `/` on the server's own hostnames and on
IP-literal hosts only, never as a catch-all, so path-limited project domains
never fall through to the panel. `/hooks/` keeps going to the webhook
listener.

### Sign-in, setup links and sessions

Panel users have a username, a role, a password (argon2id) and a TOTP
authenticator (RFC 6238: SHA-1, 6 digits, 30 s, one step of drift; a code is
accepted once). The HTTP endpoints below are JSON.

- **Setup link.** `users.create`, `users.reset` and `panel.setup` issue a link
  `https://<host>/setup#<token>`. The token is in the URL fragment (it never
  reaches server logs), single use and valid for 30 minutes; issuing a new
  link invalidates older ones. The host is `panel.public_url`, else
  `edge.domain`, else the first installed control-plane hostname, else the
  host's first non-loopback IPv4 address. In the IP case the result also
  carries `fingerprint_sha256` of the edge's self-signed certificate, to
  compare in the browser warning. With the edge disabled the link points at
  the direct listener on localhost.
- `POST /auth/setup/begin {token}` returns `{username, role, totp_secret,
  otpauth_uri}`; the secret is generated on the server and replaced on every
  call. `POST /auth/setup/finish {token, password, code}` checks the password
  policy (12 to 1024 characters, not equal to the username) and the TOTP code,
  activates the user, consumes the token and signs the user in. Any token
  problem is one generic 400.
- `POST /auth/login {username, password, code}` answers a generic 401
  `invalid credentials` for every failure (unknown user, wrong password,
  wrong or replayed code, disabled or pending user). Failures are limited per
  client address (10 per 15 minutes, then 429 with `Retry-After`) and per
  account (5 consecutive failures lock the account for 15 minutes, doubling up
  to 24 hours; a successful login resets it). `POST /auth/logout` ends the
  session; `GET /auth/session` returns `{username, role}` or 401.
- **CSRF.** Every `/auth/*` POST requires `Content-Type: application/json` and
  an `Origin` header whose host equals the request `Host`. The `/rpc` upgrade
  keeps the same-origin check of the WebSocket library when a cookie is used,
  so a page on another origin cannot open an authenticated socket.
- **Session cookie** `__Host-ki_session`: `Secure`, `HttpOnly`,
  `SameSite=Strict`, `Path=/`. Because it is `Secure`, the panel works over
  HTTPS (through the edge) or on `localhost`; plain HTTP on another host will
  not keep the cookie. A session ends after `panel.session_idle` (12 hours)
  without use or `panel.session_max` (7 days) after sign-in. Only a hash of the
  session id is stored.
- Responses carry `Referrer-Policy: no-referrer` and
  `X-Content-Type-Options: nosniff`; `/auth/*` responses are `no-store`. The
  built-in sign-in page (served while the web app is not built in) carries
  `Content-Security-Policy: default-src 'self'` and `frame-ancestors 'none'`.
  The web app carries
  `default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'`.
  Scripts are never inline and never from another origin. `style-src` allows
  `'unsafe-inline'` only because Radix/shadcn components position popovers,
  menus and tooltips with `style` attributes, which a nonce cannot cover;
  `data:` images exist only for the TOTP QR code shown at setup. The app is
  served for every path except `/auth/*`, `/rpc` and `/hooks/*` (client-side
  routing); a missing file under `/assets/` is a 404.

### API tokens

`tokens.create {name, role, expires_in?}` returns a token `ki_...` once; only
its hash is stored. `expires_in` is a Go duration of at most one year.
`tokens.list` never shows secrets (it shows name, role, creator, expiry and
last use); `tokens.revoke {name}` stops the token at once. Use a token with
`Authorization: Bearer ki_...` on the WebSocket handshake of the direct
listener (or the edge with `api.public_tokens: true`).

### Users and revocation

`users.list`, `users.create {username, role}`, `users.update {username, role?,
disabled?}`, `users.reset {username}` (clears password and authenticator,
issues a new link) and `users.delete {username}`. Usernames match
`^[a-z][a-z0-9_.-]{1,31}$`. The last active admin cannot be deleted, disabled
or demoted (`-32602`). `panel.setup {username?}` creates the first admin and
fails with `-32602` once an active admin exists; use `users.reset` then.

Disabling, deleting, resetting or changing the role of a user ends its
sessions **and closes its open connections**; revoking a token closes the
connections that used it. Clients reconnect and are refused.

### Audit log

`audit.list {limit?, before_id?}` returns entries newest first (at most 500
per call; page with `before_id`). The log records every call to an `operator`
or `admin` method, successful or failed, and the authentication events
(login, failed login, setup, logout, lockout) with time, principal, client
address, action, target (a project slug, name or id, never secrets or values)
and result (`ok` or the error code). Calls denied for lack of role are not
recorded. The newest 10 000 entries are kept.

## Outbound connection contract

1. The agent opens **one** WebSocket to `control_plane.url` with the headers

   ```
   Authorization: Bearer <token>
   User-Agent: ki-agent/<version>
   Ki-Protocol: <protocol version, e.g. 1.0.0>
   ```

   The control plane must reject bad credentials with HTTP 401 or 403 *before*
   upgrading the connection.
2. After the upgrade the control plane calls `agent.hello` first (see
   [Handshake](#handshake)), then any method. The agent sends **no requests of
   its own** on this connection, only responses and subscription
   notifications.
3. Liveness: the agent sends a WebSocket ping every `heartbeat_interval` and
   drops the connection if no pong arrives within `heartbeat_timeout`. Standard
   WebSocket libraries answer pings automatically, provided the application
   keeps reading from the connection.
4. Reconnect: after a drop the agent retries with exponential backoff from
   `reconnect_min` to `reconnect_max` with full jitter. The backoff resets once
   `agent.hello` has succeeded on a connection. After a 401/403 the agent logs
   an error and retries at `reconnect_max`. **Subscriptions do not survive a
   reconnect**: the control plane must call `agent.hello` and subscribe again
   (use `since` for container logs).
5. Transport security: `wss://` is required. `ws://` is accepted only when the
   host is a loopback address or `localhost` (development). The server
   certificate is verified against the system roots plus the optional
   `control_plane.ca_file` (PEM).

If the control plane wants an agent to go away, it closes the WebSocket; the
agent reconnects. To stop it for good, change the agent's configuration.

### Configuration

Keys under `control_plane` in the agent's config file; each can be overridden
by the environment variable `KI_CONTROL_PLANE_<KEY>`.

| Key | Default | Meaning |
|---|---|---|
| `url` | empty (disabled) | `wss://host/path` (or `ws://127.0.0.1:port/path` for development) |
| `token_file` | none | File holding the bearer token the agent presents; required when `url` is set. Must be a readable, non-empty file |
| `ca_file` | none | Extra PEM CA bundle trusted in addition to system roots |
| `heartbeat_interval` | `15s` | Ping period |
| `heartbeat_timeout` | `45s` | Drop the connection if no pong in this time; must exceed `heartbeat_interval` |
| `reconnect_min` | `1s` | First backoff delay |
| `reconnect_max` | `60s` | Maximum backoff delay; must be at least `reconnect_min` |

The agent refuses to start on a bad scheme, `ws://` to a non-loopback host,
`reconnect_min` greater than `reconnect_max`, `heartbeat_timeout` not greater
than `heartbeat_interval`, or an unreadable `ca_file`. The connection state
(`disabled`, `connecting`, `connected`, `backoff`, last error, reconnect count)
is reported in `agent.info` under `control_plane`, with `source` saying whether
the settings came from the config or from [enrollment](#enrollment).

## Handshake

Every connection starts with `agent.hello`:

```json
{"jsonrpc":"2.0","id":1,"method":"agent.hello","params":{"protocol":"1.0.0","client":{"name":"my-cp","version":"0.1.0"}}}
```

- `protocol` is the client's protocol version (`MAJOR.MINOR.PATCH`). Only the
  **major** version must match the agent's; otherwise the agent answers
  `-32003` with `data.supported` set to its own version and **closes the
  connection** after the response.
- The result carries the agent's `protocol`, `agent` (`name`, `version`,
  `commit`) and `capabilities` (for example `heartbeat`, `containers`, `projects`, `deploys`, `edge`, `enrollment`, `auth`, `stats`, `aws`, `tunnel`, `updates`).
- Until `agent.hello` succeeds, every method except `agent.hello` and
  `rpc.discover` fails with `-32002`.

### Pipelining

You do not have to wait for the `agent.hello` response. The agent handles
requests in arrival order until the handshake has completed, so requests sent
right behind `agent.hello` see its effect. After the handshake requests are
handled concurrently, so **responses may arrive in a different order than the
requests**: match them by `id`. In the transcript below, `containers.list`
(id 5) is answered before the slower `agent.heartbeat.subscribe` (id 4).

## Subscriptions

A subscription method (`<ns>.subscribe`) returns `{"subscription": "<id>"}`
(the heartbeat also returns the applied `interval_ms`). From then on the agent
sends `<ns>.update` notifications until `<ns>.unsubscribe`, the end of the
connection, or, for logs, the end of the stream. Unsubscribing an unknown id
fails with `-32004`.

Every notification has the same params envelope:

```json
{"subscription":"83a9c71c…","seq":2,"data":{…}}
{"subscription":"83a9c71c…","seq":7,"gap":{"from":4,"to":7}}
```

- `seq` starts at 1 and increases for every update of a subscription. **Order
  updates by `seq`.** Updates of one subscription are written in order over a
  connection, but `seq` is the authority.
- Exactly one of `data` and `gap` is present.
- A `gap` says that updates `from` to `to` (inclusive) were dropped before
  delivery. For a gap marker, `seq == gap.to`, so the last seq you have seen
  stays contiguous with the next update.
- The first update may arrive **before** the response to the subscribe call.
  Buffer updates for subscription ids you do not know yet.
- Subscriptions are per connection. They are gone when the connection ends and
  are never resumed by `seq`.

### Slow consumers

The agent never blocks on a slow connection and keeps memory bounded:

| Kind | Used by | When the consumer cannot keep up |
|---|---|---|
| Snapshot (latest wins) | `agent.heartbeat` | Undelivered values are replaced by the newest. `seq` skips numbers; no `gap` marker, no stale data |
| Ordered (event stream) | `containers.logs` | Updates wait in a per-subscription buffer (1024 entries); if it overflows the oldest are dropped and a `gap` marker covers them |

There is no flow-control request: a consumer that must not lose logs should
read promptly and, after a gap, re-subscribe with `since` if it needs the
missing lines.

### Container logs

`containers.logs.subscribe {id, tail?, since?, timestamps?}`:

- `tail` is the number of most recent lines sent first (default 100, values
  above 10000 are clamped to 10000, negative is `-32602`).
- `since` is an RFC 3339 timestamp; only newer lines are sent. A malformed
  value is `-32602`. To resume after a reconnect, enable `timestamps` and pass
  the time of the last line you received.
- Updates are `data: {"stream": "stdout"|"stderr", "time"?: …, "text": …}`;
  `time` is present only with `timestamps: true`.
- When the container stops (the stream ends), a final update `data: {"eof": true}`
  arrives and the subscription closes by itself. If the stream failed, the `eof`
  update also carries an `error` string.
- Only containers labelled `ki.managed=true` exist as far as the agent is
  concerned; unmanaged ones answer `-32006`, same as missing ones.

### Projects

The `projects` capability covers `projects.create`, `projects.get`,
`projects.list`, `projects.update`, `projects.verify`, `projects.delete`, the
`projects.compose.*` methods (see [Compose projects](#compose-projects)) and the
service methods (see [Services](#services)).
Schemas are in the OpenRPC document; this section holds what a schema cannot
say.

- **Trigger.** `branch` and `tag_pattern` are mutually exclusive
  (`-32602` if both are given). `Project.trigger` has exactly one key once the
  trigger is known. A project created over SSH without either parameter has
  `trigger: {}` until `projects.verify` resolves the repository's default
  branch; an HTTPS project resolves it during create.
- **Access states.** `status` is `ready` or `pending_access`. Creating an
  HTTPS project (with or without `token`) probes and clones the repository
  before anything is stored, so a failure leaves nothing behind. Creating an
  SSH project cannot clone: the agent generates an Ed25519 deploy key, stores
  the project as `pending_access` and returns the key in `auth.public_key`.
  Once the key has been added to the repository as a read-only deploy key, call
  `projects.verify`: it probes, resolves the default branch, clones (or fetches)
  and detects the Compose file, then sets `status` to `ready`. If it fails, the
  error is returned, `status_detail` records why and the status is unchanged.
  `rotate_deploy_key` in `projects.update` issues a new key and returns the
  project to `pending_access`.
- **Secrets are write-only.** Private keys and access tokens are never
  returned, in any method or error. The only secret a response carries is
  `webhook_secret`, in the result of `projects.create` and of
  `projects.update` with `rotate_webhook_secret`; it cannot be read again.
  `auth.public_key` and `auth.token_user` are not secrets.
- **Identity.** `slug` (`^[a-z0-9][a-z0-9-]{0,62}$`) and `repo_url` are
  immutable. Without `slug`, it is derived from the repository name.
- **Concurrency.** One operation at a time per project; concurrent calls for
  the same slug queue.
- **Delete.** `projects.delete` first removes the project's managed
  containers and networks (and volumes only with `volumes: true`), then the
  checkout, then the record. If the container engine is unreachable it fails
  with `-32008` and nothing is removed, so it can be retried. The result counts
  what was removed. Without `volumes: true`, the anonymous volumes of the
  removed containers are kept and listed in `kept_volumes` (always an array),
  because they carry no project label and cannot be found afterwards; with
  `volumes: true` they are removed and counted in `volumes`.

#### Compose projects

`projects.create` with `compose` (instead of `repo_url`; `slug` is required)
registers a **compose project**: a project without a repository whose Compose
file the agent stores as numbered versions and runs from prebuilt images only.
`Project.source` is `git` or `compose`; for a compose project `repo_url`,
`trigger`, `default_branch`, `compose_file`, `auth`, `webhook` and `commit` are
empty and `compose_version` is the latest version. The project is `ready` at
once, no `webhook_secret` is returned and its webhook id is random and never
published (a request to a guessed path gets 404).

| Method | Params | Result |
|---|---|---|
| `projects.compose.get` | `slug`, `version?` (0 or absent: latest) | `{version, content, created_at, created_by, note?}`; `NotFound` for an unknown version |
| `projects.compose.set` | `slug`, `compose`, `note?`, `deploy?` | `{version, deploy?}`; `deploy` is the queued `Deploy` when `deploy: true` |
| `projects.image.set` | `slug`, `image?` or `tag?` (exactly one), `service?`, `note?`, `deploy?` | `{version, image, deploy?}`; admin |
| `projects.compose.history` | `slug`, `limit?` | `[{version, created_at, created_by, note?, deployed}]`, newest first |

`get` and `set` are admin methods and `history` (which returns no file content) is a viewer method; all three fail with `-32602` ("not a compose project")
for a git project; `projects.verify` and `projects.update` fail the same way
(`not a git project`) for a compose project.

- **Image projects.** `projects.create` with `image` (instead of `compose`;
  `slug` is required) takes `{image, service?, port?, domains?, path?,
  volumes?, env?}` and generates the Compose file: one service (`app` by
  default) with `restart: unless-stopped`, the project's `.env` (optional),
  `environment` from `env` (a `{KEY: value}` object of plain values),
  `expose` and `ki.expose.*` labels from `port`, `domains` and `path`, and
  named `volumes`. `image` needs a tag or digest (`latest` is refused),
  `domains` need `port`, and bind mounts are refused, all as `-32602`. It is
  stored as version 1 with the note `generated from image`. `projects.image.set`
  changes the image of one service of any compose project: `image` is a full
  reference, `tag` replaces the tag or digest of the current one; `service`
  defaults to the only service with an image (several: `-32602` listing them).
  The new file is validated and stored as the next version (default note
  `image <ref>`), and `deploy: true` queues a deploy as for `projects.compose.set`.
- **Validation.** `compose` (at most 256 KiB) is normalized with
  `docker compose config` in an empty scratch directory, so `include`,
  `extends`, `build` and local `env_file` paths cannot resolve, and then
  checked with the safety policy with `build` forbidden. A rejection is
  `-32602` with `data: {reason, detail, violations?}`, where `reason` is
  `compose_invalid` or `policy_violation`; `violations` is
  `[{service, rule, detail}]` and a `build` section is rule `build_not_allowed`.
  Nothing is stored on rejection.
- **Versions.** Each accepted `projects.compose.set` appends the next version
  (`created_by` is the caller, `note` is free text). With `deploy: true` the
  agent then triggers a deploy of `compose@v<N>` exactly like `deploys.trigger`
  (source `cli` on the Unix socket, otherwise `api`); if that fails the version
  stays stored and the deploy error is returned.
  The newest 50 versions are kept, plus the one of the latest succeeded deploy.
- **Deploys.** Their `ref` is `compose@v<N>` (`deploys.trigger` also accepts
  `v<N>` and `<N>`; absent means the latest) and `commit` is always empty. The
  pipeline replaces `fetch` with a `prepare` stage that writes the stored
  version and the project's variables into the project directory, never
  builds, and enforces `build_not_allowed` again at the `policy` stage.
  `deploys.rollback` deploys the version of an earlier succeeded deploy.
  `deploys.trigger` with a branch or sha ref fails `ref_not_allowed`.

`RepositoryError` (`-32010`) carries `data: {reason, detail}`:

| `reason` | Meaning |
|---|---|
| `invalid_url` | The URL is not an accepted `https://`, `ssh://` or `user@host:path` form, or embeds credentials (use `token`) |
| `unreachable` | The Git server could not be contacted (DNS, network, timeout) |
| `auth_failed` | Credentials were rejected or missing; for SSH keys, the deploy key has not been added yet |
| `not_found` | The repository does not exist or is invisible to these credentials |
| `ref_not_found` | The requested branch (or the remote's default branch) does not exist |
| `compose_not_found` | No Compose file was found, or the explicit `compose_file` is missing or escapes the checkout |
| `failed` | Any other Git failure; `detail` has the last line of Git's output |

`detail` is human readable and has token text redacted. `AlreadyExists`
(`-32009`) is returned by `projects.create` when the slug is taken.

### Services

`projects.services {slug}` (viewer) returns `{services: [ProjectService]}`. A
`ProjectService` has `name`, `image?`, `exposure?` (the effective entry of that
service, see [Exposing a service](#exposing-a-service)), `containers`
(`id`, `name`, `state`, `status`, `health`, `restart_count`, `started_at?`) and
`state`. It is built from the last deployed model plus any managed container
labelled with a service that is not in that model. `state` is `running` (all
containers running), `partial` (some running), `stopped` (none running),
`unhealthy` (a container is unhealthy or restarting) or `missing` (in the model
but with no container).

`projects.services.start`, `.stop` and `.restart` (operator) take `{slug,
service}`, act on every container of the service (stop and restart wait up to
10 seconds) and return the updated `ProjectService`. A service that is neither
in the model nor backed by a container is `-32006`; an unreachable engine is
`-32008`. The actions are not serialised against a running deploy of the same
project.

### Environment variables

`projects.env.set` (`slug`, `vars`: `{NAME: value}`, merged into what is
stored), `projects.env.unset` (`slug`, `names`) and `projects.env.list`
(`slug`) manage a project's variables. `set` and `unset` return
`{names: [...]}` with all names after the change; `list` returns
`{vars: [{name, updated_at}]}` sorted by name. **Values are write-only**: they
are encrypted at rest and never returned by any method.

- Names match `^[A-Za-z_][A-Za-z0-9_]*$`; at most 256 variables per project;
  a value is at most 64 KiB and must not contain NUL. Violations are `-32602`.
- At deploy time the variables are written to `.env` (mode 0600) in the
  checkout, which Compose reads, **and** they are used for `${VAR}`
  interpolation of the Compose file itself. The interpolated model is what is
  policy-checked and what runs, so a variable cannot smuggle in something the
  policy would reject.
- Changes apply to the next deploy; running containers are not touched.

### Deploys

The `deploys` capability covers `deploys.trigger`, `deploys.rollback`, `deploys.get`,
`deploys.list`, `deploys.subscribe`, `deploys.unsubscribe` and the
`deploys.update` notification. Schemas are in the OpenRPC document; this
section holds what a schema cannot say.

| Method | Params | Result |
|---|---|---|
| `deploys.trigger` | `project`, `ref?` | `Deploy`: `queued`, or `running` if it started at once |
| `deploys.rollback` | `project`, `deploy?` | `Deploy`: a new deploy of an earlier one (see below) |
| `deploys.get` | `id` | `Deploy` |
| `deploys.list` | `project`, `limit?` (default 20, max 100), `before_id?` | `[Deploy]`, newest first |
| `deploys.subscribe` | `id`, `since_seq?` | `{subscription}`; ordered `deploys.update` stream |
| `deploys.unsubscribe` | `subscription` | `{}` |

A `Deploy` has `id`, `project`, `ref` (normalized), `commit` (resolved sha,
empty until checked out), `source`, `status`, `stage`, `error` (only when
`failed`), `rollback_of` (only on rollbacks), `created_at`, `started_at` and `finished_at` (`null` until set).

- **Source** is set by the agent: `webhook` for webhook deliveries; `api` for
  `deploys.trigger`, or `cli` when the caller is on the Unix socket.
- **Statuses.** `queued`, `running`, `succeeded`, `failed`, `superseded`
  (replaced in the queue by a newer trigger) and `interrupted` (cancelled by
  agent shutdown or `projects.delete`, or left unfinished by a previous agent
  process; it is marked at start-up).
- **Queue.** One deploy runs per project at a time. At most one waits behind
  it; a newer trigger marks the waiting one `superseded` and takes its place.
  A running deploy is never cancelled by a trigger. The running deploy holds
  the project lock, so `projects.update`/`verify` wait for it, and
  `projects.delete` cancels the project's deploys and waits for them first.
- **Refs.** `ref` absent: the head of the project's trigger branch, or for a
  tag trigger the newest remote tag matching `tag_pattern` (version sort).
  Accepted forms are `refs/heads/<branch>`, `refs/tags/<tag>`, a bare branch or
  tag name (resolved against the remote, branch first) and a full 40 or 64 hex
  digit commit sha. Only refs that match the trigger are allowed: a branch
  trigger accepts its branch (or a sha reachable from that branch's head); a
  tag trigger accepts tags matching the pattern (or a sha such a tag points
  at). Anything else is `ref_not_allowed`, which stops a webhook or API caller
  from deploying arbitrary branches.
- **Pipeline.** Each stage writes an `agent` log line and updates `stage`:

  | `stage` | What happens |
  |---|---|
  | `fetch` | Under the project lock: fetch the ref, detached checkout, record `commit`, write `.env` from the project's variables |
  | `prepare` | Compose projects only, instead of `fetch`: write the stored version as `compose.yaml` and the project's variables as `.env` |
  | `config` | `docker compose config` normalizes the Compose file (with the variables above interpolated); `include` and `extends.file` paths that are absolute or leave the checkout are rejected |
  | `policy` | The normalized model is checked against the safety policy; every violation fails the deploy |
  | `pull` | `docker compose pull --ignore-buildable` refreshes image-only services (skipped when `deploys.pull` is `missing`); registry credentials come from the agent's Docker config (`docker.config_dir`) |
  | `build` | `docker compose build` of the labelled model, output streamed |
  | `up` | `docker compose up -d --remove-orphans` |
  | `health` | Wait until every container of the deployed services is `running` and, where a healthcheck exists, `healthy`; fails on `unhealthy`, an exited container or `deploys.health_timeout` (default 5 minutes) |

  Services, networks and volumes are labelled `ki.managed=true`,
  `ki.project=<slug>` (and `ki.service=<name>` for services). What runs is
  exactly the labelled model that passed the policy check. On success the
  project's `commit` is updated. A failure leaves running containers as they
  are (there is no rollback) and the project's `commit` unchanged.
- **Failures** are recorded on the deploy (`status: failed`, `error:
  {reason, detail, violations?}`), not returned as RPC errors:

  | `error.reason` | Meaning |
  |---|---|
  | `fetch_failed` | The ref could not be fetched or checked out |
  | `compose_not_found` | The project's Compose file is missing |
  | `compose_invalid` | Compose could not normalize the file, or it includes files outside the checkout |
  | `policy_violation` | The policy rejected the model; `error.violations` lists `{service, rule, detail}` |
  | `pull_failed` | `docker compose pull` failed, typically missing registry credentials or an unreachable registry |
  | `build_failed` | `docker compose build` failed |
  | `up_failed` | `docker compose up` failed |
  | `health_failed` | Containers did not become healthy in time, or became unhealthy or exited |
  | `runtime_unavailable` | The container engine cannot be reached |
  | `domain_conflict` | A `ki.expose.*` domain is already exposed by another project (or by another service of this project through `projects.expose`), or two services of the model claim it; nothing was started |
  | `internal` | Any other agent-side failure |

- **Logs.** `deploys.subscribe` is an ordered stream: stored log lines after
  `since_seq` are replayed, live lines follow, and the stream ends with a
  `status` update after which the agent closes the subscription. Subscribing
  to a finished deploy replays the stored log and the final status. The
  `deploys.update` payload is one of
  `{"type":"log","seq":7,"stream":"stdout|stderr|agent","text":"..."}` and
  `{"type":"status","deploy":{...}}`. `seq` numbers the stored lines (use the
  last one received as `since_seq` after a reconnect); it is absent on lines
  past the per-deploy cap `deploys.log_max_bytes` (default 1 MiB), which are
  streamed live but not stored, after one `agent` line saying so. The newest
  `deploys.keep` (default 50) deploys per project are kept.

- **Rollback.** `deploys.rollback` queues a normal deploy (fetch, policy,
  build, up, health: nothing is skipped) of the target's `ref` pinned to the
  target's `commit`, with `rollback_of` set to the target's id. The target is
  `deploy` (it must belong to the project, have `succeeded` and have a commit)
  or, when absent, the newest `succeeded` deploy older than the newest
  `succeeded` one, that is the previous good deploy. Queueing, `source` and
  errors are those of `deploys.trigger`. A commit that is no longer reachable
  from its ref (a force-pushed branch) fails at the `fetch` stage.

`DeployError` (`-32012`) is returned by `deploys.trigger` and `deploys.rollback` for failures that
need no pipeline run, with `data: {reason, detail}`:

| `reason` | Meaning |
|---|---|
| `project_not_ready` | The project is `pending_access` |
| `ref_not_allowed` | The ref does not match the project's trigger |
| `ref_not_found` | The ref does not exist on the remote, or no tag matches the pattern |
| `no_rollback_target` | `deploys.rollback` found no deploy to return to: none succeeded before the newest successful one, or `deploy` is not a successful deploy of the project with a commit |

An unknown project or deploy is `-32006`; repository failures while resolving
the ref are `-32010`, an unreachable engine `-32008`.

### Webhooks

With `webhooks.listen` set (off by default), the agent runs a separate HTTP
listener that serves only `POST /hooks/<webhook_id>`; everything else is 404.
A non-loopback address requires `webhooks.tls_cert_file` and
`webhooks.tls_key_file`. Bodies over `webhooks.max_body_bytes` (default 5 MiB)
get 413. The id and secret are those of `projects.create` (the path is in
`Project.webhook.path`).

| Provider | Detected by | Authentication |
|---|---|---|
| GitHub | `X-GitHub-Event` | `X-Hub-Signature-256: sha256=<hex HMAC-SHA256 of the raw body>` with the secret |
| GitLab | `X-Gitlab-Event` | `X-Gitlab-Token` equal to the secret |

Responses are small JSON bodies and never echo secrets:

| Status | Body | When |
|---|---|---|
| 202 | `{"deploy": <id>}` | A deploy was queued |
| 202 | `{"ignored": true}` | Unsupported event, deleted ref, or a ref that does not match the project's trigger |
| 200 | `{"ok": true}` | GitHub `ping` |
| 200 | `{"duplicate": true}` | The delivery id was already handled |
| 400 | `{"error": ...}` | Neither provider header, or an invalid payload |
| 401 | `{"error": ...}` | Missing or wrong signature or token |
| 404 | `{"error": ...}` | Unknown webhook id, or any other path or method |
| 409 | `{"error": ...}` | The project is `pending_access` |
| 413 | `{"error": ...}` | Body too large |
| 500 | `{"error": ...}` | The agent failed to queue the deploy (no detail is given) |

- GitHub: `push` is handled, other events (except `ping`) are ignored.
  GitLab: `Push Hook` and `Tag Push Hook` are handled, others ignored.
- **Dedupe.** Deliveries are remembered by `X-GitHub-Delivery` /
  `X-Gitlab-Event-UUID` (per project, in memory, up to 1024 entries for 24
  hours) and repeated ones answer `duplicate`. A delivery that failed with 5xx
  can be retried.
- **Push handling.** The payload's `ref` must be `refs/heads/<branch>` for a
  branch trigger, or `refs/tags/<tag>` matching `tag_pattern` for a tag
  trigger; otherwise it is ignored. The deploy is pinned to the pushed commit
  (`after`), not to whatever the branch head is by the time it runs. For tag
  pushes the tagged commit is used (GitHub `head_commit.id`, GitLab
  `checkout_sha`; when absent the agent resolves the tag itself).
- Deploys created this way have `source: webhook`.
- **Through the edge.** With the [edge](#exposure-and-the-edge) enabled and
  `webhooks.listen` empty, the agent listens on `127.0.0.1:<edge.webhooks_port>`
  (default 8090) and the edge forwards `/hooks/` to it, so deliveries use the
  server's public address: `https://<server>/hooks/<id>`. Webhooks must use
  **https**: plain `http://` requests to the edge are redirected to https, and
  most providers do not follow a redirect on a POST. For the bare IP the edge
  serves its [self-signed certificate](#default-certificate), so the sender
  must be told to skip verification (or use a server domain). `/hooks/` is
  routed only on the server's own hostnames and for unknown hosts and bare IPs,
  never on project domains, so an application's own `/hooks` paths keep
  working.

## Exposure and the edge

Capability `edge`. The agent can run a reverse proxy (the **edge**, a managed
container started with `docker compose`) that publishes project services on
domains with HTTPS. It is enabled by default (`edge.enabled`), binds ports
80 and 443 (`edge.http_port`, `edge.https_port`) and is configured entirely by
the agent; nothing in a project can reach its configuration. The agent keeps
running when the edge cannot start (port in use, image pull failure, engine
down): `edge.status` reports `state: error` with `last_error`, and the agent
retries with a growing delay.

### Exposing a service

A service is exposed with `{service, port, domains, path?}`, from either:

- **Labels** in the project's Compose file: `ki.expose.port: "8080"`,
  `ki.expose.domains: "app.example.com,www.example.com"` and optionally
  `ki.expose.path: "/api"`. They are read from the normalized model at deploy
  time and stored after a **successful** deploy, replacing the previous label
  set. Other `ki.*` labels and every `traefik.*` label on a service are
  rejected by the [policy](#deploys) (rule `reserved_label`).
- **The project side**: `projects.expose` (`slug`, `service`, `port`, `domains`,
  `path?`) and `projects.unexpose` (`slug`, `service`), both returning the
  `Project`. A project-side entry **overrides** the labels of that service
  entirely; `unexpose` makes the labels apply again.

`Project.exposure` lists the effective entries with their `source` (`labels` or
`project`); `Project.exposure_warnings` carries advisory notes, such as a
service that does not exist in the last deployed model or a label domain that
another project owns (and so is not routed).

- **Validation** (`-32602`): port 1 to 65535; at most 20 domains per service;
  domains are lowercase DNS names in ASCII/punycode with at least one dot, a
  non-numeric last label, at most 253 characters and 63 per label, no wildcards
  and no IP addresses; a path starts with `/`, has no spaces or `..` and is at
  most 256 characters. Domains are trimmed, lowercased and de-duplicated.
- **Uniqueness.** A domain routes to at most one (project, service) on the
  server. `projects.expose` fails with `-32009` and `data: {domain, project}`
  when another project has it; a deploy whose labels collide fails with the
  reason `domain_conflict` before anything is started.
- **Reserved hostnames.** The server's own hostnames (`edge.domain` and the
  hostnames with an [installed certificate](#control-plane-certificates)) cannot
  be exposed by projects (`-32602`).
- **Routing.** One route per exposed service: `Host(...)` (and the path prefix)
  on HTTPS, load-balanced over the running containers of the service on
  `http://<container ip>:<port>`; containers without an address are left out and
  a service with none has no route. Certificates for project domains are
  obtained by ACME (HTTP-01), so the domain must resolve to the server and port
  80 must be reachable. The routes follow deploys, container restarts and
  exposure changes within seconds.

### Default certificate

Requests for the bare IP or an unknown host get a persistent **self-signed**
ECDSA P-256 certificate (CN `ki-agent`, valid 10 years, SANs: the hostname and
the non-loopback interface addresses at creation) generated once into
`<data_dir>/edge/` and never regenerated while its files exist. Its SHA-256
fingerprint and expiry are in `edge.status` (`self_signed`) and
`ki agent edge status`, so an operator can pin or check it.

### Edge methods

| Method | Params | Result | Transports |
|---|---|---|---|
| `edge.status` | none | `EdgeStatus` | all |
| `edge.csr` | `hostname` | `{hostname, csr}` (PEM) | Unix socket, control plane |
| `edge.certificate.install` | `hostname`, `certificate` (PEM chain) | `EdgeStatus` | Unix socket, control plane |
| `edge.certificate.import` | `certificate` (PEM chain), `key` (PEM), `server?` | `EdgeStatus` | Unix socket only |
| `edge.certificate.remove` | `id` or `hostname` | `EdgeStatus` | Unix socket, control plane |

`edge.status` always answers; with the edge disabled it reports `enabled:
false` and `state: disabled`. The restricted methods answer `-32001` on the
agent's HTTP listeners, and a hostname that a project exposes is refused
with `-32009`.

### Control-plane certificates

A control plane that fronts the agent's own hostname (for example with an
origin certificate trusted only by its CDN) provisions the certificate over
RPC; the private key never leaves the server:

1. `edge.csr {hostname}`: the agent creates (or reuses the pending) ECDSA P-256
   key for the hostname, keeps it in `<data_dir>/edge/certs/` (mode 0600), and
   returns a PEM CSR with that single DNS name.
2. The control plane has the CSR signed by its CA.
3. `edge.certificate.install {hostname, certificate}` with the PEM chain, leaf
   first. The leaf must match the stored key, cover the hostname, be valid now
   and not expire within **24 hours**; otherwise `-32602` explains why. The
   chain is **not** verified against system roots, so a private or origin CA is
   fine. On success the hostname becomes a server hostname served with this
   certificate and is reserved against project exposure.
4. Renew by repeating the three steps; `edge.certificate.remove {hostname}`
   deletes the certificate and key.

`edge.status.certificates` lists installed certificates (with `not_after`) and
the ACME ones the edge will obtain (`edge.domain` and project domains).

`EdgeStatus` also reports `mode` (`front` or `behind`) and, in behind mode,
`listen`. Each `certificates[]` entry is `{id, hostname, names, source, server,
not_after?}` with `source` one of `acme`, `installed` or `imported`.

### Bring-your-own certificates

`edge.certificate.import {certificate, key, server?}` is accepted on the Unix
socket only, because it carries a private key; other transports get `-32001`.
The key must match the leaf, and the leaf must be valid now and not expire
within 24 hours (`-32602` otherwise). The names are the leaf's DNS subject
alternative names; wildcards such as `*.example.com` are accepted here (one
label deep) and never conflict with project domains. The files are stored under
`<data_dir>/edge/certs/` (mode 0600) under a stable id derived from the names
(`i-<hex>`); importing the same names again replaces them.

A project router whose every domain is covered by an installed or imported
certificate (exact or wildcard) is served with that certificate instead of an
ACME one. With `server: true` the non-wildcard names also become server
hostnames (reserved against project exposure, panel and hooks routes).
`edge.certificate.remove {id}` removes an imported certificate; `{hostname}`
still removes an installed server-hostname certificate.

### Behind mode

With `edge.mode: behind` the edge does not own ports 80/443: Traefik binds
only `edge.behind_listen` (loopback, default `127.0.0.1:8480`) over plain HTTP,
requests no ACME certificates and does no HTTP-to-HTTPS redirect. It trusts
`X-Forwarded-*` from loopback only. Server routes (panel, hooks, RPC) are served
only for the server hostnames; the bare-IP routes are not created, since the
front web server decides what reaches the agent. `edge.status` reports
`mode: "behind"` and `listen`. `ki agent edge proxy-config` prints the front
server's configuration for the domains to forward.

### Server hostnames

On the server's own hostnames, on unknown hosts and on the bare IP, `/hooks/`
is forwarded to the webhook listener (see [Webhooks](#webhooks)) and anything
else is a plain 404 until a management UI exists. HTTP is redirected to HTTPS.

## Cloudflare Tunnel

Capability `tunnel`. The agent can run a managed `cloudflared` connector (the
`ki-tunnel` Compose project) that links the server to a remotely managed
Cloudflare Tunnel; Cloudflare forwards the public hostnames to the edge in
behind mode. The connector is enabled with `tunnel.enabled` and receives the
tunnel token as a mounted token file, never in its arguments or environment.

| Method | Params | Result | Role | Transports |
|---|---|---|---|---|
| `tunnel.status` | none | `TunnelStatus` | viewer | all |
| `tunnel.configure` | `token` | `TunnelStatus` | admin | Unix socket, control plane |
| `tunnel.remove` | none | `TunnelStatus` | admin | Unix socket, control plane |

```json
{"enabled": true, "state": "connected", "configured": true, "ready": true, "connections": 4,
 "image": "cloudflare/cloudflared:2026.10.0", "warnings": []}
```

`state` is `disabled`, `unconfigured` (enabled, no token), `starting` (the
connector was just started, up to a minute), `connected`, `disconnected` or
`error` (with `last_error`). `ready` and `connections` come from the
connector's own `/ready` endpoint. `warnings` lists configuration problems, for
example an edge that is not in behind mode. The token (1 to 4096 base64
characters) is never returned or logged; an invalid one is `-32602`. The
restricted methods answer `-32001` on the agent's HTTP listeners, so a control
plane can provision a tunnel but a panel session or API token cannot. The
audit log records the calls without their parameters.

## Updates

Capability `updates`. The agent checks GitHub Releases for a newer version
(`updates.check` / `updates.channel` in the config) and reports it; installing
is done by a root helper, never by the agent process.

| Method | Params | Result | Role | Transports |
|---|---|---|---|---|
| `updates.status` | none | `UpdateStatus` | viewer | all |
| `updates.check` | none | `UpdateStatus` | admin | all |
| `updates.apply` | `version` | `{"id": "...", "state": "requested"}` | admin | WebSocket with a panel session only |

```json
{"current": "v0.3.0", "channel": "stable", "check_enabled": true,
 "checked_at": "2026-10-27T10:00:00Z", "check_error": null,
 "latest": {"tag": "v0.4.0", "name": "v0.4.0", "published_at": "2026-10-26T09:00:00Z",
            "url": "https://github.com/open-southeners/ki/releases/tag/v0.4.0",
            "notes": "markdown", "prerelease": false},
 "update_available": true, "apply_supported": true, "state": "idle", "last_result": null}
```

`current` is `dev` in source builds, where `update_available` is always false.
`updates.check` contacts GitHub at most once a minute and otherwise returns the
cached status. `apply_supported` is false when `ki-agent-update.path` is not
installed. `state` is `idle` or `requested` (the request is written and the
helper has not reported yet); `last_result` is the helper's outcome
(`succeeded`, `failed` or `rolled_back`, with `from`, `to` and `error`).

`updates.apply` accepts only the version that `latest` and `update_available`
name; anything else, a server without the helper, or an update already
requested is `-32602`. It answers `-32001` on the Unix socket, the control
plane, the HTTP API and for API tokens, so only a signed-in panel admin can
install. The call is recorded in the audit log.

## Enrollment

Enrollment connects an agent to a control plane without hand-editing its
config or copying a token. It is the OAuth 2.0 Device Authorization Grant
([RFC 8628](https://www.rfc-editor.org/rfc/rfc8628)) with a discovery document
and one extra response field. An operator runs `ki agent enroll <base-url>` on
the machine, approves a short code in a browser, and the agent stores the
credentials and starts its outbound connection. A control plane implements the
server side described here; [`examples/control-plane`](../examples/control-plane)
does so with `-enroll`. `B` below is the base URL given to `ki agent enroll`.

### Flow

1. **Discovery.** `GET B/.well-known/ki-agent-enrollment` answers 200 with

   ```json
   {"device_authorization_endpoint":"https://cp.example.com/enroll/device","token_endpoint":"https://cp.example.com/enroll/token"}
   ```

   Both URLs must be absolute and `https://`.
2. **Device authorization.** The agent POSTs
   `application/x-www-form-urlencoded` to `device_authorization_endpoint`:

   | Field | Value |
   |---|---|
   | `client_id` | always `ki-agent` |
   | `hostname`, `os`, `arch` | the machine, to help the operator recognise it |
   | `agent_version`, `protocol` | agent software version and protocol version |

   The answer is 200 JSON as in RFC 8628 section 3.2:

   | Field | Meaning |
   |---|---|
   | `device_code` | Opaque secret for polling; use at least 32 random bytes |
   | `user_code` | What the operator types or confirms; short and unambiguous, for example `BKQ4-7XM2` |
   | `verification_uri` | Page where the operator enters the code |
   | `verification_uri_complete` | Optional; same page with the code filled in |
   | `expires_in` | Seconds until the codes die. The agent stops waiting then, capped at 30 minutes |
   | `interval` | Optional minimum seconds between polls, default 5 |

   Showing the operator the agent's `hostname`, `os` and `arch` on the approval
   page is strongly advised: the code alone proves nothing about which machine
   is asking.
3. **Token polling.** Every `interval` the agent POSTs to `token_endpoint`:
   `grant_type=urn:ietf:params:oauth:grant-type:device_code`, `device_code`
   and `client_id=ki-agent`.

   | Answer | Meaning | Agent behaviour |
   |---|---|---|
   | 400 `{"error":"authorization_pending"}` | Not decided yet | Keeps polling every `interval` |
   | 400 `{"error":"slow_down"}` | Polled too fast | Adds 5 seconds to `interval`, keeps polling |
   | 400 `{"error":"access_denied"}` | The operator denied it | Fails with reason `denied` |
   | 400 `{"error":"expired_token"}` | The codes expired | Fails with reason `expired` |
   | 200 `{"access_token":"…","token_type":"Bearer","control_plane_url":"wss://…"}` | Approved | Stores the credentials and connects |

   Any other answer, malformed JSON or a body over 64 KiB fails the
   enrollment (`invalid_response`). Return each device code's token **once**.
4. **Connection.** `access_token` is what the agent then sends as
   `Authorization: Bearer` on the [outbound connection](#outbound-connection-contract)
   to `control_plane_url`, so the WebSocket endpoint there must accept it.

All requests carry `User-Agent: ki-agent/<version>` and
`Accept: application/json`, time out after 30 seconds and are verified against
the system roots plus `control_plane.ca_file`.

### `control_plane_url`

It is chosen by the server, usually the same for every agent. It follows the
rules of `control_plane.url`: `wss://`, or `ws://` only for a loopback host.
Anything else makes the enrollment fail with `invalid_response` and nothing is
stored.

### Security notes

- The base URL and both endpoints must be `https://`. `http://` is accepted
  only for loopback hosts (development).
- The agent never logs or prints the access token or the device code, and
  neither appears in errors. A server should not log them either; log the
  user code instead.
- Authenticate the operator on the verification page and show which machine is
  being approved. Tokens should be per agent and revocable on the server.
- Credentials are written to `<data_dir>/control-plane.json` (`data_dir`
  defaults to `/var/lib/ki-agent`) with mode `0600` through an atomic rename.
  The agent refuses a file readable by group or others. The file holds
  `url`, `token`, `enrolled_at` and `enrollment_url`.
- When that file exists it **replaces** `control_plane.url` and
  `control_plane.token_file` from the config; the other `control_plane.*`
  settings (CA file, heartbeat, backoff) still apply. The agent logs which
  source it uses at start. `ki agent unenroll` deletes the file and falls back
  to the config's control plane, if any.

### Local enrollment methods

Capability `enrollment`. These methods control the agent itself, so they are
**local-only**: any transport other than the Unix socket (HTTP listeners,
outbound connection) is refused with `-32001` ("only available on the local
socket"). This is what `ki agent enroll` and `unenroll` use.

| Method | Params | Result |
|---|---|---|
| `enrollment.start` | `url?` (default: the URL baked in at build time), `force?` | `{user_code, verification_uri, verification_uri_complete?, expires_at}` |
| `enrollment.wait` | none | `{control_plane_url}` once approved; blocks until then |
| `enrollment.remove` | none | `{}` |

- `start` without `url` and without a built-in default is `-32602`. If the agent
  is already enrolled it fails with `-32009` unless `force` is true; so does a
  second `start` while one enrollment is in progress. Polling runs in the
  background and is **bound to the connection that started it**: it is
  cancelled when that connection closes. On approval the credentials are saved
  and the outbound connection starts at once, without a restart.
- `wait` works only on the connection that called `start` (otherwise `-32602`).
- `remove` deletes the credentials and stops that connection, falling back to
  the config's control plane if there is one. When not enrolled it fails with
  `-32006`.
- Failures are `-32011` EnrollmentError with `data: {reason, detail}`:

  | `reason` | Meaning |
  |---|---|
  | `unreachable` | The enrollment server could not be contacted (DNS, network, TLS, timeout) |
  | `invalid_response` | A response was malformed, too large, used a disallowed URL or was an unexpected error |
  | `denied` | The operator denied the request |
  | `expired` | The code expired before it was approved |
  | `failed` | Any other failure, for example saving the credentials |

`agent.info` reports where the active control plane came from in
`control_plane.source`: `"config"` or `"enrollment"` (absent while disabled).

## AWS

Capability `aws`. `aws.status` (role `viewer`) reports whether the server is an
EC2 instance and which AWS agents run on it:

```json
{"ec2": {"detected": true, "instance_id": "i-0123456789abcdef0", "instance_type": "t3.small",
         "region": "eu-west-1", "availability_zone": "eu-west-1a", "ami_id": "ami-0123456789abcdef0",
         "instance_profile_arn": "arn:aws:iam::123456789012:instance-profile/example",
         "imds_v1_enabled": false},
 "agents": {"cloudwatch": {"installed": false, "active": false, "enabled": false, "source": ""},
            "ssm": {"installed": true, "unit": "snap.amazon-ssm-agent.amazon-ssm-agent.service",
                    "active": true, "enabled": true, "version": "3.3.0.0", "source": "snap"}}}
```

Off EC2 the result is `{"ec2": {"detected": false}, ...}` within about a
second. The identity is read with IMDSv2 only and cached for 10 minutes; the
agent never reads instance credentials. `imds_v1_enabled` is true when an
unauthenticated metadata request works. Agent state comes from systemd, without
root. Whether an agent is registered with its service is not visible locally;
see [`aws-agents.md`](aws-agents.md).

## Monitoring

Capability `stats`. All six methods are `viewer` methods. The agent samples
the host every `monitor.host_interval` (default 15 s) and the running
ki-managed containers (plus the edge proxy) every `monitor.container_interval`
(default 30 s). While subscriptions exist the interval is the smallest one
requested, but never above the configured base. Rates (`cpu.percent`,
`cpu_percent` and every `*_per_s` field) are computed by the agent from
consecutive samples and are `null` in the first sample after the agent starts,
for a container's first sample, and right after a container restarted.
`time` in a sample is when the agent took it. History is in memory only: one
sample per `monitor.history_resolution` (default 15 s) for `monitor.history`
(default 1 h), per host and per container; it is empty after an agent restart.

### Host

`host.stats {history?, per_core?}` returns `{"current": HostSample | null,
"history"?: [HostSample]}` (oldest first). It never waits for the sampler,
so `current` is `null` in the first moments after start. With `per_core: true`
`cpu.per_core` is included and the call may wait up to 2 s for a sample that
carries it.

`host.subscribe {interval_ms?, per_core?}` returns `{"subscription": id,
"interval_ms": n}`; `interval_ms` (default 2000) is clamped to 1000..60000 and
the result reports the value applied. It is a snapshot subscription: each
`host.update` has `data: {"sample": HostSample}`, the first one as soon as a
sample exists, and a slow consumer receives the latest one. `host.unsubscribe
{subscription}` ends it (`-32004` for an unknown id).

A `HostSample` holds `time`, `cpu {percent, cores, per_core?}`, `load {1, 5,
15}`, `memory {total, used, available, percent}`, `swap {total, used,
percent}`, `disks [{path, device, fstype, total, used, free, percent,
inodes_percent}]` (the filesystems holding `/` and the data directory plus
`monitor.disk_paths`, one entry per device), `disk_io` (bytes and operations
per second, summed over whole block devices), `network` (bytes per second
summed over the interfaces except `lo`, `veth*`, `docker*`, `br-*`, `cni*`,
`flannel*` and `virbr*`, plus the same per interface) and `uptime_s`. Sizes are
bytes. When the agent runs in a container the figures are that container's
view of the host unless the host's `/proc` and `/sys` are mounted and
`HOST_PROC` / `HOST_SYS` are set (see the README).

### Containers

`containers.stats {project?, id?, history?}` returns `{"available": bool,
"containers": [{"current": ContainerSample | null, "history"?: [...]}]}`.
`id` is a full container id or a prefix of at least 12 characters of a
**managed** container; an unmanaged or unknown id answers `-32006`, as in
`containers.inspect`. `available` is `false` (with an empty list, not an
error) when the container engine cannot be reached.

`containers.stats.subscribe {project?, id?, interval_ms?}` behaves like
`host.subscribe` (default 5000, clamped to 2000..60000, result with
`interval_ms`); each `containers.stats.update` has `data: {"available": bool,
"containers": [ContainerSample]}`. An unmanaged id is `-32006` and an
unreachable engine `-32008` at subscribe time. `containers.stats.unsubscribe
{subscription}` ends it.

A `ContainerSample` holds `time`, `id`, `name`, `project`, `service`,
`component`, `cpu_percent` (of one core, so it can exceed 100), `memory
{usage, limit, percent}` (usage excludes reclaimable file cache, as `docker
stats` reports it; `percent` is `null` without a limit), `network
{rx_bytes_per_s, tx_bytes_per_s}`, `block_io {read_bytes_per_s,
write_bytes_per_s}` and `pids`. Series for containers that disappeared are
dropped after one history length.

## Errors

Errors are JSON-RPC error objects `{code, message, data?}`.

| Code | Name | Meaning |
|---|---|---|
| -32700 | Parse error | Payload is not valid JSON |
| -32600 | Invalid request | Malformed JSON-RPC request (bad `jsonrpc`, `method`, `id`, `params`, or empty batch) |
| -32601 | Method not found | Unknown method |
| -32602 | Invalid params | Params missing or wrong (also a non-semver `protocol` in `agent.hello`) |
| -32603 | Internal error | Handler failed or panicked |
| -32001 | Unauthorized | `forbidden: requires <role>` (the principal's role is below the method's `x-role`), a local-only method (`enrollment.*`) called over a transport other than the Unix socket, or a transport-restricted one (`edge.csr`, `edge.certificate.install`, `edge.certificate.remove`: Unix socket and control plane only) called over another. Authentication failures are HTTP 401 before the upgrade, not JSON-RPC errors |
| -32002 | Handshake required | Method called before a successful `agent.hello` |
| -32003 | Incompatible version | Major version mismatch; `data.supported` is the agent's version; the connection is closed |
| -32004 | Subscription not found | Unknown subscription id on unsubscribe |
| -32005 | Overloaded | Reserved; not currently returned |
| -32006 | Not found | Container or project does not exist (containers not managed by ki count as missing) |
| -32007 | Policy violation | Reserved; not currently returned. Deploys report violations on the failed deploy (`error.violations`, `{service, rule, detail}`) |
| -32008 | Runtime unavailable | The container engine cannot be reached |
| -32009 | Already exists | `projects.create` with a slug that is taken; or a domain that is already exposed by another project, with `data` `{domain, project}` (`projects.expose`, and the `edge.*` methods, which add `service`) |
| -32010 | Repository error | A Git operation failed; `data` is `{reason, detail}`, see [Projects](#projects) |
| -32011 | Enrollment error | Enrollment failed; `data` is `{reason, detail}`, see [Enrollment](#local-enrollment-methods) |
| -32012 | Deploy error | `deploys.trigger` or `deploys.rollback` was rejected; `data` is `{reason, detail}`, see [Deploys](#deploys) |

## Versioning

`api.ProtocolVersion` (`info.version` of the OpenRPC document, currently
`1.0.0`) follows [semantic versioning](https://semver.org/).

Until the first tagged release of the agent, the protocol is still taking
shape and stays at `1.0.0` whatever changes; clients should rely on
`capabilities`, not the version, to detect methods. The version is **frozen at
`1.0.0` as of the first tagged release**, and from then on the rules below
apply.

- Additive changes (new methods, new optional params, new result fields, new
  capabilities) bump the **minor** version. Clients must ignore unknown fields
  in results and notifications.
- Breaking changes bump the **major** version. `agent.hello` refuses a
  different major version (`-32003`), so a mismatch is caught on the first
  message.
- A client should check `capabilities` rather than guessing from the agent's
  software version.

## Wire example

Captured from a real agent over its Unix socket (`>` client to agent, `<`
agent to client). A WebSocket transcript is identical apart from the framing:
each line is one text message. Container engine was not running, hence
`-32008`.

```
> {"jsonrpc":"2.0","id":1,"method":"agent.info"}
< {"jsonrpc":"2.0","id":1,"error":{"code":-32002,"message":"handshake required: call agent.hello first"}}
> {"jsonrpc":"2.0","id":2,"method":"agent.hello","params":{"protocol":"1.0.0","client":{"name":"example-cp","version":"0.1.0"}}}
< {"jsonrpc":"2.0","id":2,"result":{"agent":{"commit":"dev","name":"ki-agent","version":"dev"},"capabilities":["heartbeat","containers","projects","deploys","edge","enrollment","auth","stats","aws","tunnel","updates"],"protocol":"1.0.0"}}
> {"jsonrpc":"2.0","id":3,"method":"agent.info"}
< {"jsonrpc":"2.0","id":3,"result":{"arch":"amd64","commit":"dev","go_version":"go1.26.1","hostname":"example-host","os":"linux","uptime_ms":1075,"version":"dev"}}
> {"jsonrpc":"2.0","id":4,"method":"agent.heartbeat.subscribe","params":{"interval_ms":1000}}
> {"jsonrpc":"2.0","id":5,"method":"containers.list"}
< {"jsonrpc":"2.0","id":5,"error":{"code":-32008,"message":"container runtime unavailable"}}
< {"jsonrpc":"2.0","id":4,"result":{"interval_ms":1000,"subscription":"83a9c71c24513e7e7166740efc2335b0"}}
< {"jsonrpc":"2.0","method":"agent.heartbeat.update","params":{"subscription":"83a9c71c24513e7e7166740efc2335b0","seq":1,"data":{"time":"2026-10-14T17:35:36.072304681Z","uptime_ms":2076}}}
< {"jsonrpc":"2.0","method":"agent.heartbeat.update","params":{"subscription":"83a9c71c24513e7e7166740efc2335b0","seq":2,"data":{"time":"2026-10-14T17:35:37.072303381Z","uptime_ms":3076}}}
> {"jsonrpc":"2.0","id":6,"method":"agent.heartbeat.unsubscribe","params":{"subscription":"nope"}}
< {"jsonrpc":"2.0","id":6,"error":{"code":-32004,"message":"subscription not found"}}
> {"jsonrpc":"2.0","id":7,"method":"agent.hello","params":{"protocol":"2.0.0"}}
< {"jsonrpc":"2.0","id":7,"error":{"code":-32003,"message":"incompatible protocol version 2.0.0: agent speaks 1.0.0","data":{"supported":"1.0.0"}}}
```

After the last response the agent closes the connection.
