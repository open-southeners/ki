# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- **`ki` for macOS and Windows**: releases include `ki_<version>_darwin_<arch>.tar.gz` and `ki_<version>_windows_<arch>.zip` (amd64 and arm64), and `ki` installs with Homebrew from the Open Southeners tap (`brew install open-southeners/tap/ki`), ready for the upcoming `ki login` and Kiarga commands. `ki agent` commands stay server-side on Linux.

### Changed

- The repository moved to `github.com/open-southeners/ki` (the Go module path too). The old `open-southeners/ki-agent` URLs redirect, so existing installs, updates and the install command keep working.

- **`ki` command**: the CLI is now `ki`, with the agent's commands under `ki agent` (for example `ki agent projects list` and `sudo ki agent update`), as groundwork for upcoming Kiarga commands. Installs place `/usr/local/bin/ki` with `ki-agent` as a symlink to it, and the services run `ki agent run`. If you self-updated from 1.0.x, run `sudo ki-agent update` once (or re-run the installer) to add `ki`.

### Deprecated

- The `ki-agent` command name still works exactly like `ki agent` throughout 1.x, but `ki` is the name to use going forward.

## [1.0.2] - 2026-10-27

### Fixed

- Releases include their binaries again: the 1.0.0 and 1.0.1 GitHub releases were published without archives, `checksums.txt`, signatures or `install.sh` because the signing step could not read the release key.

## [1.0.1] - 2026-10-27

### Fixed

- **Updates** now install: 1.0.0 shipped without the release signing key, so its updater refused every release. Because a 1.0.0 agent still cannot verify newer releases, move servers from 1.0.0 to 1.0.1 by re-running the installer (`curl -fsSL .../releases/latest/download/install.sh | sudo sh`); from 1.0.1 on, the panel's **Update now** and `sudo ki-agent update` work.
- Release builds report their version as `1.0.1` while tags are `v1.0.1`; the updater now treats both forms as the same version, so new releases show as available, downgrades are refused, and a successful update is no longer rolled back by its health check.

## [1.0.0] - 2026-10-27

First public release.

### Added

- **Install & service**
  - One-line installer, `curl -fsSL .../releases/latest/download/install.sh | sudo sh`, for amd64 and arm64: creates the `ki-agent` user and group, installs and starts the systemd service, and writes a default config only if none exists. Re-running it upgrades and keeps your config.
  - Installer flags: `--version`, `--with-docker`, `--enroll [URL]`, `--no-start`, `--add-user`, `--no-edge`, `--no-panel` and `--tunnel-token-file <file>`. The sha256 checksum is always verified, and the cosign signature too when `cosign` is installed.
  - `ki-agent run` runs the agent in the foreground with JSON logs and shuts down cleanly on SIGINT or SIGTERM; `ki-agent version` prints the version and commit.
  - The sandboxed systemd service runs as a dedicated `ki-agent` user and group; the CLI socket is available to members of the `ki-agent` group.
  - Configuration comes from `/etc/ki-agent/config.yaml` (override with `--config`) and then `KI_*` environment variables; see `packaging/config.example.yaml`. `--log-level` overrides `log_level`.
  - State lives in `state.db` and `secret.key` in the data directory, with project checkouts under `projects/`. `git` and an `ssh` client are required on the host and are included in the container image.
  - A `Dockerfile` and Compose file run the agent next to a Docker-in-Docker sidecar for development.

- **Projects & deploys**
  - **Git projects**: `projects.create` registers a repository, inferring the slug, default branch and Compose file, and returns a webhook path and secret shown only once. `projects.get|list|update|verify|delete` manage them.
  - **Private repositories**: SSH URLs get a generated deploy key and wait in `pending_access` until `projects.verify` confirms it was added; HTTPS repositories take an access token. Keys and tokens are encrypted at rest and never returned.
  - **Compose-file projects**: `ki-agent projects create <slug> --compose-file <file>` stores a Compose file instead of a repository, for apps built in CI and for servers without Git access. Release with `ki-agent projects compose set <slug> --file <file> --deploy`; versions are checked against the safety policy when stored, may not build images (`build_not_allowed`), and show in a Compose tab in the panel. API: `projects.compose.get|set|history`.
  - **Image projects**: `ki-agent projects create <slug> --image <ref> --port 8080 --domain app.example.com [--volume name:/path] [--env KEY=VALUE]` (or **New project** in the panel) generates the Compose file for a single container, with the image pinned by tag or digest (`latest` is refused) and named volumes only. `ki-agent projects image set <slug> <tag> --deploy` (`projects.image.set`) releases a new tag on any Compose project, so CI only sends the tag.
  - **Deploys**: `ki-agent deploy --project <slug>` (`deploys.trigger`) deploys the trigger branch head or newest matching tag and waits until services are running and healthy; `--ref` deploys another matching branch, tag or commit. One deploy runs per project at a time and a newer request replaces one still waiting. Failures carry a `reason` such as `policy_violation`, `build_failed`, `health_failed`, `pull_failed` or `domain_conflict` and leave running containers untouched.
  - **Pull stage**: a `pull` stage refreshes image-only services before every deploy so moving tags update; set `deploys.pull: missing` to pull only absent images.
  - **History and live logs**: `ki-agent deploys list|get|logs` and `deploys.get|list|subscribe` (resumable with `since_seq`) show each deploy's stages and output. The newest `deploys.keep` (default 50) deploys are kept, with up to `deploys.log_max_bytes` (default 1 MiB) of log each; deploys cut short by a restart are marked `interrupted`.
  - **Rollbacks**: `ki-agent deploys rollback <project> [--to <deploy-id>]` (`deploys.rollback`) redeploys the previous good version, pinned to its exact commit, through the full pipeline; the new deploy records `rollback_of`. With nothing to return to it fails with `-32012` and reason `no_rollback_target`.
  - **Safety policy** on every deploy: the Compose file is normalized, checked and labelled, and exactly that model is built and started. Violations fail the deploy with the full list; paths in `include`, `extends`, `env_file` and `label_file` that leave the checkout, symlinked Compose or `.env` files, and services using `provider` or a non-default `runtime` are refused. Services may not set `traefik.*` or other `ki.*` labels (`reserved_label`), except `ki.expose.*`.
  - **Environment variables**: `ki-agent projects env list|set|import|unset` (`projects.env.*`) store encrypted per-project variables that are never shown again, written to the deploy's `.env` and used for `${VAR}` interpolation.
  - **Push webhooks**: with `webhooks.listen` set, `POST /hooks/<id>` deploys the exact commit pushed to the trigger branch or a matching tag. GitHub deliveries are verified by HMAC signature, GitLab by token; duplicates, other events and non-matching refs are ignored. A non-loopback listener requires `webhooks.tls_cert_file` and `webhooks.tls_key_file`.
  - **Private registries**: deploys use their own Docker client configuration (`docker.config_dir`, default `<data_dir>/docker`), so `docker login` or a credential helper such as `docker-credential-ecr-login` works; see the README for ECR.
  - **Project deletion** removes the project's containers and networks (and volumes with `volumes: true`) before its checkout, and changes nothing if the container engine is unreachable. Without `volumes`, anonymous volumes are kept and reported as `kept_volumes`; `ki-agent projects delete` prints their names with the `docker volume rm` command.
  - **Containers and services**: `containers.list|inspect|start|stop|restart|remove` and `runtime.info` work on containers labelled `ki.managed=true` only (others answer `-32006` like missing ones). `ki-agent services list|start|stop|restart` (`projects.services`) show each service's containers, health and exposure. `containers.logs.subscribe` streams live logs, and `ki-agent logs <container>` prints them. The agent starts without Docker and answers `-32008` until the engine is reachable; configure it with `docker.host` and `docker.cli` (needs the Compose v2 plugin).

- **Edge & HTTPS**
  - A managed Traefik edge proxy publishes project services on their domains with automatic Let's Encrypt certificates; ports and behaviour come from the `edge.*` keys, and `edge.enabled: false` turns it off. If it cannot start (for example a port in use) the agent keeps running and retries; `ki-agent edge status` (`edge.status`) reports mode, ports, certificates and routes.
  - **Expose services on domains** with the `ki.expose.port`, `ki.expose.domains` and optional `ki.expose.path` labels, or `ki-agent projects expose|unexpose` (`projects.expose|unexpose`), which override the labels. `projects.get` shows the effective `exposure` and any `exposure_warnings`. A domain belongs to one project and service per server; conflicts answer `-32009`.
  - **Front and behind modes**: in the default `front` mode the edge owns ports 80 and 443. With `edge.mode: behind` it listens on loopback (`edge.behind_listen`, default `127.0.0.1:8480`) over plain HTTP, so an existing nginx (for example Forge) or Caddy keeps ports 80/443 and TLS. `ki-agent edge proxy-config [--format nginx|caddy] [--domain d]` prints the server block to paste for each migrated domain.
  - **Imported certificates**: `ki-agent edge certificate import --cert <file> --key <file> [--server]` installs an existing certificate and key, including wildcards, and serves every project domain it covers instead of ACME; `ki-agent edge certificate remove <id>` removes it. They are listed with names, expiry and server flag in `edge.status` and on the Edge page; keys never go through the browser or the network API.
  - **Default certificate**: the bare IP or an unknown host gets a persistent self-signed certificate, with its SHA-256 fingerprint in `edge.status`.
  - **Control-plane certificates**: `edge.csr`, `edge.certificate.install` and `edge.certificate.remove` let a control plane provision a certificate for the server's own hostname without the key leaving the server (valid for at least 24 more hours). Local socket and control-plane connection only.
  - **Webhooks through the edge**: with the edge enabled and `webhooks.listen` empty, `https://<server>/hooks/<id>` reaches the webhook listener on the server's hostnames and bare IP, with plain HTTP redirected to HTTPS; `/hooks` is not routed on project domains.
  - The agent's HTTP API listens on two loopback addresses: `api.listen` (default `127.0.0.1:8091`, for local scripts) and `api.edge_listen` (default `127.0.0.1:8092`, the only address the edge routes to). The edge serves `/rpc`, `/auth/` and `/` on the server's own hostnames and bare IP, never on project domains.

- **Cloudflare Tunnel**
  - With `tunnel.enabled: true` (and `edge.mode: behind`) the agent runs a managed `cloudflared` connector for a remotely managed Cloudflare Tunnel, so the server needs no public IP or open ports.
  - `ki-agent tunnel configure --token-stdin|--token-file <file>`, `ki-agent tunnel status` and `ki-agent tunnel remove` (`tunnel.configure|status|remove`) manage it; changes are limited to the local socket and the control plane. The token is passed to `cloudflared` as a mounted file and never appears in arguments, environment, logs or status.
  - The panel's Overview and Edge pages show a Tunnel card, and `install.sh --tunnel-token-file <file>` sets it up on a fresh install. See the README.

- **Panel & access control**
  - **Web panel** served at `/` through the edge (HTTPS) and on `127.0.0.1:8091`, with live host and container charts, project pages (services with health, deploy history with live log, container logs and stats) and, for admins, user, API token, audit log, environment variable, exposure and edge pages. Actions are hidden below the role that allows them, and it follows the system light or dark theme. The app is built into the binary and container image; building from source needs [bun](https://bun.sh), otherwise a minimal sign-in page is served. Turn it off with `panel.enabled: false`, `KI_PANEL_ENABLED=false` or `ki-agent run --no-panel`.
  - **Sign-in**: `ki-agent panel setup` (run by the installer) prints a one-time link that creates the first admin with a password and an authenticator app (TOTP). Sign-in is rate limited per address and per account, accounts lock after repeated failures, and sessions are stored hashed in a `__Host-` cookie.
  - **Users and roles**: `viewer` reads, `operator` also starts, stops and restarts containers and triggers deploys, `admin` does everything else, including managing users and tokens. `ki-agent users create|list|update|reset|delete` (`users.*`) manage accounts; disabling, deleting or resetting a user, or changing its role, closes its open connections. A call below its role fails with `-32001`, every OpenRPC method carries its role as `x-role`, and `auth.whoami` reports the caller's identity and role. The Unix socket and control-plane connection act as admin.
  - **API tokens**: `ki-agent tokens create|list|revoke` (`tokens.*`) mint named bearer tokens with a role and optional expiry, shown once. They work on the loopback listener, and through the edge only with `api.public_tokens: true`; revoking one closes its connections.
  - **Audit log**: `ki-agent audit` (`audit.list`) shows every operator or admin call with its result, plus sign-in, setup, logout and lockout events, with the client address. The newest 10 000 entries are kept.
  - The panel shows the ki-agent Kⁱ logo in the sidebar, sign-in pages and browser tab, following the light and dark themes.

- **Monitoring**
  - The agent measures the host (CPU, load, memory, swap, disk space and inodes, disk and network throughput, uptime) and every running managed container plus the edge proxy (CPU, memory, network and block I/O, processes), keeping the last hour in memory.
  - `ki-agent stats` prints a host summary with the busiest containers, and `ki-agent containers stats [--project <slug>]` lists every container; `--watch` (with `--interval`) streams live updates and both take `--json`.
  - `host.stats` and `containers.stats` return the latest sample (optionally with history); `host.subscribe` and `containers.stats.subscribe` stream updates at a requested interval (1 to 60 s for the host, 2 to 60 s for containers). They are viewer methods behind the `stats` capability.
  - Tune sampling with `monitor.*` (`host_interval`, `container_interval`, `history`, `history_resolution`, `disk_paths`, env `KI_MONITOR_*`). When the agent runs in a container, mount the host's `/proc` and `/sys` and set `HOST_PROC` / `HOST_SYS` to report the real host.

- **AWS awareness**: `ki-agent aws status` (`aws.status`, `aws` capability, viewer role) reports whether the server is an EC2 instance (id, type, region, zone, AMI, instance profile, whether the legacy IMDSv1 is still enabled) and whether the SSM and CloudWatch agents are installed, running and enabled, including the snap-packaged SSM agent. It uses IMDSv2 only, never reads instance credentials and answers "not detected" within about a second off EC2. Shown as an AWS card on the panel's Overview; see `docs/aws-agents.md`.

- **Updates**
  - The agent checks GitHub Releases for a newer version and shows it with its release notes under Admin, Updates in the panel and in `ki-agent update`. Control it with `updates.check` and `updates.channel` (`stable` or `prerelease`, env `KI_UPDATES_CHECK`, `KI_UPDATES_CHANNEL`).
  - Only an admin installs an update, with **Update now** in the panel or `sudo ki-agent update [--check] [--version vX.Y.Z] [--yes]`. It verifies the release signature and checksum, backs up the database, swaps the binary, restarts the agent and rolls back automatically if the new version does not start (exit status `2`), while containers keep running.
  - API: `updates.status`, `updates.check` and `updates.apply` (`updates` capability); installing is limited to panel sessions, so a control plane or API token cannot do it. `install.sh` enables the `ki-agent-update.path` unit that makes panel installs work. Updates install only once a release signing key is built in; see the README.

- **Control plane, API & CLI**
  - **Outbound control-plane connection**: with `control_plane.url` set (env `KI_CONTROL_PLANE_*`), the agent dials out over `wss://` (plain `ws://` only for loopback) with a bearer token from `control_plane.token_file`, and the control plane drives the normal JSON-RPC API over it, so agents behind NAT or firewalls need no inbound port. It reconnects with jittered exponential backoff (`reconnect_min`/`reconnect_max`) and trusts the system roots plus an optional `control_plane.ca_file`; subscriptions must be re-established after a reconnect.
  - **Device-flow enrollment**: `ki-agent enroll [URL]` prints a code to approve in a browser, then stores the credentials (`control-plane.json` in the data directory, mode 0600) and connects without a restart; `--force` enrolls again and `ki-agent unenroll` forgets it. `enrollment.start|wait|remove` (`enrollment` capability, local socket only) expose the same flow.
  - `ki-agent status` and `agent.info` report the connection (`control_plane` with `state`, sanitized `url`, `connected_since`, `last_error`, `reconnects`, `next_attempt`, and `source` of config or enrollment). The example control plane serves enrollment with `-enroll`, and `docs/protocol.md` specifies what a control plane must implement.
  - **JSON-RPC 2.0** on the local Unix socket (`socket.path`, newline delimited) and over an authenticated WebSocket at `/rpc` (API token or panel session, with ping/pong liveness). Either side may send requests and notifications.
  - **Session handshake**: `agent.hello` negotiates the protocol version (`1.0.0`; a different major is rejected with `-32003`) and advertises capabilities; every other method except `rpc.discover` returns `-32002` until it succeeds. Also available: `agent.info` and `agent.ping`.
  - **OpenRPC specification** of the whole API, including error codes and the subscription format, is returned by `rpc.discover` and shipped as `api/openrpc.json`.
  - **Subscriptions**: `*.subscribe` returns a `subscription` id and updates arrive as `*.update` notifications with an increasing `seq`; slow consumers get the latest value for snapshot streams or a `gap` marker for ordered streams. `agent.heartbeat.subscribe` demonstrates the pattern.
  - **CLI**: `ki-agent` subcommands talk to the running agent over its Unix socket (`--socket`, `KI_SOCKET_PATH` or `socket.path`), print `--json` on request and take tokens only from `--token-stdin` or `--token-file`.
  - Errors are documented in the OpenRPC specification and `docs/protocol.md`, including `-32006` NotFound, `-32007` PolicyViolation, `-32008` RuntimeUnavailable, `-32009` AlreadyExists, `-32010` RepositoryError and `-32011` enrollment failures (each with a `reason`).

- **Releases**
  - Tagged versions publish Linux amd64 and arm64 archives, `checksums.txt`, a keyless cosign signature and an ed25519 signature (`checksums.txt.sig`) on GitHub Releases, with the matching `CHANGELOG.md` section as notes; tags with a suffix such as `-rc.1` are published as pre-releases. See "Release signing" in the README.
  - `scripts/local-release.sh` builds release-shaped archives, `checksums.txt` and `install.sh` from a checkout so a server can be installed with `KI_INSTALL_BASE_URL=file://...` before a public release.

[Unreleased]: https://github.com/open-southeners/ki/compare/v1.0.2...HEAD
[1.0.2]: https://github.com/open-southeners/ki/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/open-southeners/ki/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/open-southeners/ki/releases/tag/v1.0.0
