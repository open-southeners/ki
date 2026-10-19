# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

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
  missing default file just means defaults. Keys: `data_dir`, `log_level`,
  `socket.path`, `socket.mode`, `websocket.listen`, `websocket.path`,
  `websocket.token_file`, `websocket.heartbeat_interval` and
  `websocket.heartbeat_timeout`. `--log-level` overrides `log_level`. The
  agent refuses to start with the WebSocket listener enabled unless
  `websocket.token_file` points to a readable, non-empty file. See
  `packaging/config.example.yaml`.
- **JSON-RPC 2.0 endpoint** on the local Unix socket (`socket.path`, newline
  delimited) and, when `websocket.listen` is set, on an authenticated
  WebSocket (`Authorization: Bearer` token, with ping/pong liveness that drops
  dead connections). Either side of a connection can send requests and
  notifications. Connection open and close are logged; the token never is.
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

- `projects.delete` now reports the anonymous volumes it kept (`kept_volumes`) when `volumes` is not set, and `ki-agent projects delete` prints their names with the `docker volume rm` command to remove them. With `volumes: true` anonymous volumes are removed and counted in `volumes`.
- The systemd service now runs as a dedicated `ki-agent` user and group instead of root; the CLI socket is accessible to members of the `ki-agent` group. Existing manual installs need that user created (the installer does it).
