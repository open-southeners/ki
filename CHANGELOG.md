# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

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
