# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- **Compose projects**: a project can be a stored Compose file instead of a Git repository, for apps whose images are built in CI. Create one with `ki-agent projects create <slug> --compose-file <file>`, release with `ki-agent projects compose set <slug> --file <file> --deploy`, and roll back with `ki-agent deploys rollback` (API: `projects.create` with `compose`, `projects.compose.get|set|history`). Every version is checked against the safety policy when stored and may not build images (`build_not_allowed`). Compose projects never contact a Git host, so they work on servers without GitHub access; the panel shows their versions in a Compose tab.
- **Image projects**: a single-container app no longer needs a Compose file. `ki-agent projects create <slug> --image <ref> --port 8080 --domain app.example.com [--volume name:/path] [--env KEY=VALUE]` (or **New project** in the panel, which also creates Git and Compose-file projects) generates one, with the image pinned by tag or digest (`latest` is refused) and named volumes only. `ki-agent projects image set <slug> <tag> --deploy` (API: `projects.image.set`, panel: Update image) releases a new tag on any compose project, so CI only has to send the tag.
- **Cloudflare Tunnel**: with `tunnel.enabled: true` (and `edge.mode: behind`) the agent runs a managed `cloudflared` connector that links the server to a remotely managed Cloudflare Tunnel, so it needs no public IP or open ports. `ki-agent tunnel configure --token-stdin|--token-file <file>` stores the tunnel token, `ki-agent tunnel status` shows the connection state, and `ki-agent tunnel remove` deletes the token and the connector; the same are the `tunnel.status`, `tunnel.configure` and `tunnel.remove` methods (new `tunnel` capability; changing the tunnel is limited to the local socket and the control plane). The token is passed to `cloudflared` as a mounted file and never appears in its arguments, environment, logs or status. The Overview and Edge pages of the panel show a Tunnel card, and `install.sh --tunnel-token-file <file>` sets it all up on a fresh install. See the README.
- **AWS awareness**: `ki-agent aws status` and the `aws.status` method (new `aws` capability, viewer role) report whether the server is an EC2 instance (id, type, region, zone, AMI, instance profile, and whether the legacy IMDSv1 is still enabled) and whether the SSM and CloudWatch agents are installed, running and enabled, including the snap-packaged SSM agent. It uses IMDSv2 only, never reads instance credentials, and answers "not detected" within about a second off EC2. The panel's Overview shows it as an AWS card. See `docs/aws-agents.md`.
- **Migrating servers that already run a web server**: `edge.mode: behind` makes the edge listen on loopback (`edge.behind_listen`, default `127.0.0.1:8480`) over plain HTTP so an existing nginx (for example Forge) or Caddy keeps ports 80/443 and TLS, and `ki-agent edge proxy-config [--format nginx|caddy] [--domain d]` prints the server block to paste for each migrated domain. Switch to `front` mode when the last site has moved. `edge.status` and the Edge page show the mode.
- **Bring your own certificate**: `ki-agent edge certificate import --cert <file> --key <file> [--server]` (method `edge.certificate.import`, local socket only) installs an existing certificate and key, including wildcards such as `*.example.com`, and serves every project whose domains it covers with it instead of ACME. `ki-agent edge certificate remove <id>` removes it. Imported certificates are listed with names, expiry and server flag in `edge.status` and on the Edge page; keys never go through the browser or the network API.
- **Private registries**: deploys run Compose with their own Docker client configuration (`docker.config_dir`, default `<data_dir>/docker`), so `docker login` or a credential helper such as `docker-credential-ecr-login` works for the agent; see the README for ECR.
- **Image updates on deploy**: a new `pull` stage refreshes image-only services before every deploy so moving tags update, with the failure reason `pull_failed`. Set `deploys.pull: missing` to pull only absent images.
- `scripts/local-release.sh` builds release-shaped archives, `checksums.txt` and `install.sh` from a checkout so a server can be installed with `KI_INSTALL_BASE_URL=file://...` before a public release.
- **Web panel**: the agent now serves a full management app at `/` (through the edge over HTTPS, and on `127.0.0.1:8091`), replacing the bare sign-in page. It has live host and container charts, the project list and project pages (services with health, deploy history with a live log, container logs and stats), and for admins user, API token, audit log, environment variable, exposure and edge pages. Actions are hidden below the role that allows them, and it follows the system light or dark theme. The app is built into the binary and the container image; building from source needs [bun](https://bun.sh), and without it the agent falls back to the minimal sign-in page.
- **Rollback**: `ki-agent deploys rollback <project> [--to <deploy-id>]` and `deploys.rollback` deploy the previous good version again, pinned to its exact commit and run through the full pipeline. The new deploy shows which one it re-deploys in `rollback_of`; with nothing to return to the call fails with `-32012` and reason `no_rollback_target`.
- **Service view and control**: `ki-agent services list|start|stop|restart` and the `projects.services` methods show each service of a project with its containers, health and exposure, and let operators start, stop or restart a service. Viewers can read, operators can act.

- **Resource monitoring**: the agent now measures the host (CPU, load, memory, swap, disk space and inodes, disk and network throughput, uptime) and every running managed container plus the edge proxy (CPU, memory, network and block I/O, processes), and keeps the last hour in memory. `ki-agent stats` prints a host summary with the busiest containers, `ki-agent containers stats [--project <slug>]` lists every container, and `--watch` (with `--interval`) streams live updates; both take `--json`. Rates need two samples, so a fresh value shows `-` until the next one.
- **Stats methods**: `host.stats` and `containers.stats` return the latest sample (and optionally the history), and `host.subscribe` / `containers.stats.subscribe` stream `host.update` / `containers.stats.update` at a requested interval (clamped to 1 to 60 s for the host and 2 to 60 s for containers). They are viewer methods behind the new `stats` capability; the protocol stays at `1.0.0`.
- New `monitor.*` configuration (`host_interval`, `container_interval`, `history`, `history_resolution`, `disk_paths`, env `KI_MONITOR_*`). The agent samples at the base intervals all the time and faster only while someone is subscribed. When the agent runs in a container, mount the host's `/proc` and `/sys` and set `HOST_PROC` / `HOST_SYS` to report the real host.
- **Users and roles**: three roles (`viewer`, `operator`, `admin`) now decide which methods a caller may use: viewers read, operators also start, stop and restart containers and trigger deploys, admins do everything else, including managing users and tokens. A call below its role fails with `-32001` (`forbidden: requires <role>`) and every method in the OpenRPC document carries its role as `x-role`. The Unix socket and the control-plane connection act as admin.
- **Web panel sign-in**: `ki-agent panel setup` (the installer runs it for you) prints a one-time link that creates the first admin with a password and an authenticator app (TOTP); `ki-agent users create|list|update|reset|delete` and the `users.*` methods manage the rest. Sign-in is rate limited per address and per account, accounts lock after repeated failures, and sessions are stored hashed in a `__Host-` cookie. Disabling, deleting or resetting a user, or changing its role, also closes its open connections. The panel is served at `/` through the edge (HTTPS) and on `127.0.0.1:8091`; turn it off with `panel.enabled: false`, `KI_PANEL_ENABLED=false` or `ki-agent run --no-panel`.
- **API tokens**: `ki-agent tokens create|list|revoke` (and `tokens.*`) mint named bearer tokens with a role and an optional expiry; the secret is shown once. Tokens work on the loopback listener, and through the edge only with `api.public_tokens: true`; revoking one closes its connections.
- **Audit log**: `ki-agent audit` and `audit.list` show who did what: every operator or admin call (with its result) and sign-in, setup, logout and lockout events, with the client address. Denied calls are not recorded and the newest 10 000 entries are kept.
- `auth.whoami` reports the caller's identity and role, and a new `auth` capability is advertised by `agent.hello`.
- **Edge proxy with HTTPS**: the agent starts a managed reverse proxy container (ports 80 and 443, configurable with the `edge.*` keys or `KI_EDGE_*`) that publishes project services on their domains with automatic Let's Encrypt certificates. It never stops the agent when it cannot start (port in use, image pull failure): `edge.status` and `ki-agent edge status` report the state, ports, certificates and routes, and the agent keeps retrying. Disable it with `edge.enabled: false`.
- **Expose services on domains**: label a Compose service with `ki.expose.port`, `ki.expose.domains` and optionally `ki.expose.path`, or use `projects.expose` / `projects.unexpose` (and `ki-agent projects expose|unexpose`), which override the labels. `projects.get` shows the effective `exposure` and any `exposure_warnings`. A domain can belong to only one project and service on a server: `projects.expose` answers `-32009` with the domain and project, and a deploy that collides fails with the new reason `domain_conflict`.
- **Safety policy** rule `reserved_label`: services may not set `traefik.*` labels or any `ki.*` label other than `ki.expose.port`, `ki.expose.domains` and `ki.expose.path`.
- **Default certificate**: requests to the bare IP or an unknown host are answered with a persistent self-signed certificate whose SHA-256 fingerprint is shown by `edge.status`.
- **Control-plane certificates**: `edge.csr`, `edge.certificate.install` and `edge.certificate.remove` let a control plane provision a certificate for the server's own hostname without the key leaving the server (the chain does not need to verify against system roots; it must be valid for at least 24 more hours). They are available on the local socket and the control-plane connection only; other transports get `-32001`.
- **Webhooks through the edge**: with the edge enabled and `webhooks.listen` empty, the webhook listener starts on `127.0.0.1:8090` (`edge.webhooks_port`) and `https://<server>/hooks/<id>` reaches it on the server's hostnames and the bare IP; plain HTTP is redirected to HTTPS. `/hooks` is not routed on project domains.
- New `edge` capability, and `install.sh --no-edge` to install with the edge disabled (it also warns when ports 80 or 443 are already in use).
- **Deploys**: `deploys.trigger` (and `ki-agent deploy --project <slug>`) fetches a project's trigger branch head, or the newest matching tag, and runs its Compose file, waiting until services are running and healthy. `--ref` or `ref` deploys another branch, tag or commit that matches the project's trigger. One deploy runs per project at a time and a newer request replaces one still waiting. Failures are recorded on the deploy with a `reason` such as `policy_violation`, `build_failed` or `health_failed`, and leave running containers untouched.
- **Deploy history and live logs**: `deploys.get`, `deploys.list` and `deploys.subscribe` (streaming `deploys.update`, resumable with `since_seq`) expose each deploy's stages and output; `ki-agent deploys list|get|logs` print them. The newest `deploys.keep` (default 50) deploys and up to `deploys.log_max_bytes` (default 1 MiB) of log per deploy are kept. Deploys cut short by a restart are marked `interrupted`. New `deploys` capability and error `-32012` with a `reason` of `project_not_ready`, `ref_not_allowed` or `ref_not_found`.
- **Safety policy on every deploy**: the Compose file is normalized, checked and labelled (`ki.managed`, `ki.project`, `ki.service`), and exactly that checked model is built and started; violations fail the deploy with the full list. `include`, `extends`, `env_file` and `label_file` paths that leave the checkout are rejected before Compose reads them (whatever the Compose version), symlinked Compose files and a symlinked `.env` fail the deploy, and services using `provider` or a non-default `runtime` are refused.
- **Push webhooks**: with `webhooks.listen` set (env `KI_WEBHOOKS_LISTEN`), `POST /hooks/<id>` deploys the exact commit pushed to the trigger branch or to a matching tag. GitHub deliveries are verified by HMAC signature and GitLab ones by token; duplicate deliveries, other events and non-matching refs are ignored. A non-loopback listener requires `webhooks.tls_cert_file` and `webhooks.tls_key_file`.
- **Project environment variables**: `projects.env.set`, `projects.env.unset` and `projects.env.list` (and `ki-agent projects env list|set|import|unset`) store encrypted per-project variables that are never shown again. They are written to the deploy's `.env` and used for `${VAR}` interpolation in the Compose file.
- New `deploys.*` and `webhooks.*` configuration keys, and `docker.cli` to choose the Docker CLI (needs the Compose v2 plugin); the container image now includes both.
- **One-line installer**: `curl -fsSL .../releases/latest/download/install.sh | sudo sh` installs the latest release for amd64 or arm64, creates the `ki-agent` user and group, installs and starts the systemd service and writes a default config only if none exists. Re-running it upgrades and keeps your config; `--version`, `--with-docker`, `--enroll [URL]`, `--no-start` and `--add-user` adjust it. The sha256 checksum is always verified, and the cosign signature too when `cosign` is installed.
- **Signed releases**: tagged versions publish Linux amd64 and arm64 archives, `checksums.txt` and a keyless cosign signature on GitHub Releases.
- **Device-flow enrollment**: `ki-agent enroll [URL]` prints a code to approve in a browser, then stores the credentials and connects to the control plane without a restart; `--force` enrolls again and `ki-agent unenroll` forgets the enrollment. Credentials are kept in `control-plane.json` in the data directory (mode 0600) and replace `control_plane.url` and `control_plane.token_file` while present.
- `enrollment.start`, `enrollment.wait` and `enrollment.remove` methods behind a new `enrollment` capability, plus error `-32011` with a `reason` such as `denied`, `expired` or `unreachable`. They work only on the local Unix socket; other transports get `-32001`.
- `ki-agent status` and `agent.info` report whether the control-plane connection comes from the config or from enrollment (`control_plane.source`).
- The example control plane serves the enrollment endpoints with `-enroll`, and `docs/protocol.md` specifies what a control plane must implement.
- **Projects**: `projects.create` registers a Git repository, inferring the
  slug, default branch and Compose file, and returns a webhook path and secret
  that is shown only once. `projects.get`, `projects.list`, `projects.update`,
  `projects.verify` and `projects.delete` manage them; `agent.hello` advertises
  the `projects` capability.
- **Private repositories**: SSH URLs get a generated deploy key and wait in
  `pending_access` until `projects.verify` confirms the key was added; HTTPS
  repositories take an access token. Keys and tokens are encrypted at rest and
  never returned by the API.
- **Project deletion** removes the project's containers and networks (and
  volumes with `volumes: true`) before its checkout, and changes nothing if the
  container engine is unreachable.
- New errors `-32009` AlreadyExists and `-32010` RepositoryError (with a
  `reason` such as `auth_failed` or `compose_not_found`), documented in the
  OpenRPC specification and `docs/protocol.md`.
- **Command-line client**: `ki-agent status`, `ki-agent projects
  create|list|get|verify|update|delete` and `ki-agent logs <container>` talk to
  the running agent over its Unix socket (`--socket`, `KI_SOCKET_PATH` or
  `socket.path`), print `--json` on request, and take tokens only from
  `--token-stdin` or `--token-file`.
- The agent now keeps its state in `state.db` and `secret.key` in the data
  directory, with project checkouts under `projects/`. `git` and an `ssh`
  client are required on the host and are included in the container image.
- **Outbound control-plane connection**: with `control_plane.url` set (env
  `KI_CONTROL_PLANE_*`), the agent dials out to a control plane over
  `wss://` (plain `ws://` only for loopback hosts) with a bearer token from
  `control_plane.token_file`, and the control plane then drives the normal
  JSON-RPC API over that connection, so agents behind NAT or firewalls need no
  inbound port. The connection is kept alive with pings, re-established with
  jittered exponential backoff (`reconnect_min`/`reconnect_max`, backing off
  to the maximum when credentials are rejected), and trusts the system roots
  plus an optional `control_plane.ca_file`. Subscriptions do not survive a
  reconnect; the control plane re-subscribes.
- `agent.info` reports the connection in a new `control_plane` field (`state`,
  sanitized `url`, `connected_since`, `last_error`, `reconnects`,
  `next_attempt`).
- **Container management**: `containers.list`, `containers.inspect`,
  `containers.start`, `containers.stop`, `containers.restart` and
  `containers.remove` operate on Docker containers, and `runtime.info`
  reports the engine. Only containers labelled `ki.managed=true` are visible;
  unmanaged containers answer `-32006` exactly like missing ones. The agent
  starts without Docker and answers `-32008` until the engine is reachable.
- **Live container logs**: `containers.logs.subscribe` (`tail`, `since`,
  `timestamps`) streams `containers.logs.update` notifications until
  `containers.logs.unsubscribe` or the container stops, which ends the stream
  with `{eof: true}`.
- **Docker configuration**: `docker.host` (env `KI_DOCKER_HOST`, default
  `unix:///var/run/docker.sock`), and the systemd unit now runs with the
  `docker` group so the agent can reach the socket.
- `agent.hello` advertises the `containers` capability; new error codes
  `-32006` NotFound, `-32007` PolicyViolation (reserved for the upcoming
  deploy safety checks) and `-32008` RuntimeUnavailable are documented in the
  OpenRPC specification.
- **Agent service**: `ki-agent run` starts the agent in the foreground, logs
  as JSON on stderr, creates its data directory, and shuts down cleanly on
  SIGINT or SIGTERM. `ki-agent version` prints the version and commit.
- **Configuration**: settings are read from `/etc/ki-agent/config.yaml`
  (override with `--config`), then from `KI_*` environment variables; a
  missing default file just means defaults. Core keys: `data_dir`,
  `log_level`, `socket.path`, `socket.mode`, and `api.*` for the HTTP
  listeners. `--log-level` overrides `log_level`. See
  `packaging/config.example.yaml`.
- **JSON-RPC 2.0 endpoint** on the local Unix socket (`socket.path`, newline
  delimited) and over an authenticated WebSocket at `/rpc` (API token or panel
  session, with ping/pong liveness that drops dead connections). Either side
  of a connection can send requests and notifications. Connection open and
  close are logged; credentials never are.
- **Session handshake**: `agent.hello` negotiates the protocol version
  (`1.0.0`; a different major version is rejected with `-32003` and the
  connection is closed) and every other method except `rpc.discover` returns
  `-32002` until it succeeds. Clients may send requests right behind
  `agent.hello` without waiting for its response.
- **Agent methods**: `agent.info` (version, host, OS, uptime), `agent.ping`
  and `rpc.discover`.
- **OpenRPC specification** of the whole API, returned by `rpc.discover` and
  shipped as `api/openrpc.json`, including the error codes and the
  subscription update format.
- **Subscriptions**: `*.subscribe` returns a `subscription` id and updates
  arrive as `*.update` notifications with an increasing `seq`. Slow consumers
  get the latest value for snapshot streams, or a `gap` marker for ordered
  streams; subscriptions end with their connection. `agent.heartbeat.subscribe`
  (`interval_ms`, clamped to 250 ms - 60 s) demonstrates the pattern.
- **Graceful shutdown**: on SIGINT or SIGTERM the agent stops listening,
  closes open connections and removes the socket file.
- **systemd unit** (`packaging/systemd/ki-agent.service`) running the agent
  sandboxed: the data directory is the only writable path, with
  `NoNewPrivileges`, `PrivateTmp` and `ProtectHome` enabled.
- **Development stack**: a `Dockerfile` and a Compose file that runs the
  agent next to a Docker-in-Docker sidecar on the `devmesh` network.

### Changed

- `edge.certificate.remove` also accepts an `id`, and `edge.status` certificate entries now carry `id`, `names` and `server` with a `source` of `imported` for imported ones; `edge.status` reports `mode` and, in behind mode, `listen`. Deploy history can now show the `pull` stage.

- The agent's HTTP API now listens on two loopback addresses, `api.listen` (default `127.0.0.1:8091`, for local scripts) and `api.edge_listen` (default `127.0.0.1:8092`, the only address the edge routes to). The edge serves `/rpc`, `/auth/` and `/` on the server's own hostnames and bare IP, never on project domains. `install.sh --no-panel` writes `panel.enabled: false` into a new config.
- `projects.delete` now reports the anonymous volumes it kept (`kept_volumes`) when `volumes` is not set, and `ki-agent projects delete` prints their names with the `docker volume rm` command to remove them. With `volumes: true` anonymous volumes are removed and counted in `volumes`.
- The systemd service now runs as a dedicated `ki-agent` user and group instead of root; the CLI socket is accessible to members of the `ki-agent` group. Existing manual installs need that user created (the installer does it).

### Removed

- The `websocket` configuration section and its single shared bearer-token file are gone: the agent now refuses to start with a `websocket:` key (use `api.listen` and `ki-agent tokens create`), and `KI_WEBSOCKET_*` variables are ignored with a warning (use `KI_API_*`). The fixed WebSocket path is `/rpc`.
