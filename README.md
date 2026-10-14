# ki-agent

An open-source (Apache-2.0) background agent that manages one Linux server's
services for an external control plane. It is written in Go and ships as a
single static binary.

**Status: pre-alpha.** The service, configuration, systemd packaging and the
JSON-RPC endpoint and Docker container management exist; deploying compose
projects is under development.

## Build

```sh
go build -o ki-agent ./cmd/ki-agent
```

Stamp a release version with
`-ldflags "-X github.com/open-southeners/ki-agent/internal/version.Version=1.2.3"`.

## Run

```sh
./ki-agent run --config packaging/config.example.yaml
./ki-agent version
```

The agent logs JSON to stderr and stops on SIGINT or SIGTERM. For a system
install, copy the binary to `/usr/local/bin/ki-agent`, the config to
`/etc/ki-agent/config.yaml` and `packaging/systemd/ki-agent.service` to
`/etc/systemd/system/`.

## Configuration

Config is read from `/etc/ki-agent/config.yaml` (or `--config`), then
overridden by `KI_*` environment variables such as `KI_DATA_DIR` or
`KI_WEBSOCKET_LISTEN`. Every key is documented in
[`packaging/config.example.yaml`](packaging/config.example.yaml). The
WebSocket listener is off by default and, when enabled, requires a bearer
token file. `docker.host` (env `KI_DOCKER_HOST`, default
`unix:///var/run/docker.sock`) selects the container engine; the agent starts
even if the engine is down.

## Protocol

The agent speaks [JSON-RPC 2.0](https://www.jsonrpc.org/specification) over a
local Unix socket (`socket.path`, default `/run/ki-agent/ki.sock`, one JSON
message per line) and, when `websocket.listen` is set, over a WebSocket at
`websocket.path` that requires `Authorization: Bearer <token>`. Both ends are
symmetric peers. The full contract is the OpenRPC document
[`api/openrpc.json`](api/openrpc.json), also returned by `rpc.discover`.

Every connection starts with `agent.hello` (protocol major versions must
match; a mismatch is answered with `-32003` and the connection is closed);
other methods fail with `-32002` until it succeeds. Requests may be pipelined
right behind `agent.hello` without waiting for its response. Subscriptions
(`<ns>.subscribe` / `<ns>.unsubscribe`) deliver `<ns>.update` notifications
carrying `subscription`, a monotonically increasing `seq` and `data`; ordered
streams that overflow send a `gap` marker instead (`{"from","to"}`, with
`seq` equal to `to`).

```sh
# Unix socket: type one request per line
socat - UNIX-CONNECT:/run/ki-agent/ki.sock
{"jsonrpc":"2.0","id":1,"method":"agent.hello","params":{"protocol":"1.0.0"}}
{"jsonrpc":"2.0","id":2,"method":"agent.heartbeat.subscribe","params":{"interval_ms":1000}}

# WebSocket
websocat -H 'Authorization: Bearer YOUR_TOKEN' ws://127.0.0.1:8443/rpc
{"jsonrpc":"2.0","id":1,"method":"agent.hello","params":{"protocol":"1.0.0"}}
{"jsonrpc":"2.0","id":2,"method":"agent.info"}
```

## Connecting to a control plane

Besides serving the Unix socket and the optional WebSocket listener, the agent
can dial out to a control plane, which suits servers behind NAT or a firewall.
Set `control_plane.url` (`wss://`, or `ws://` for loopback development) and
`control_plane.token_file`; the agent then keeps one WebSocket open with
automatic reconnects, and the control plane drives the normal JSON-RPC API over
it. The connection rules, subscription semantics, error codes and versioning
policy are in [`docs/protocol.md`](docs/protocol.md), and
[`examples/control-plane`](examples/control-plane) is a small runnable control
plane written against that document alone.

## Container management

The agent manages Docker containers through these methods (all behind
`agent.hello`; the full schemas are in `api/openrpc.json`):

| Method | Purpose |
|---|---|
| `runtime.info` | Engine name, version, API version, OS and architecture |
| `containers.list {project?}` | Managed containers, optionally of one project |
| `containers.inspect {id}` | State, exit code, health, mounts, networks |
| `containers.start`, `containers.stop`, `containers.restart {id, timeout_s?}` | Lifecycle |
| `containers.remove {id, force?}` | Remove a container |
| `containers.logs.subscribe {id, tail?, since?, timestamps?}` | Follow logs |
| `containers.logs.unsubscribe {subscription}` | Stop following |

Only containers carrying the label `ki.managed=true` (plus `ki.project` and
`ki.service`) are visible: anything else on the host is untouchable, and an
unmanaged container is indistinguishable from a missing one (`-32006`). If the
engine is unreachable, methods fail with `-32008` until it is back.

`containers.logs.subscribe` returns an ordered subscription that delivers
`containers.logs.update` notifications with `data: {stream, time?, text}`
(`time` only with `timestamps`). `tail` defaults to 100 (max 10000). When the
container stops, a final `{eof: true}` update arrives and the subscription
closes. Subscriptions cannot be resumed by sequence number: after a reconnect,
subscribe again with `since` set to the time of the last line received.

### Safety policy (upcoming)

Docker group membership is root-equivalent on the host, so compose projects
will be checked before they run. The `internal/policy` library checks the
normalized model from `docker compose config --format json` and rejects, per
service: `privileged`, host/foreign `network_mode`, `pid`, `ipc`, `uts`,
`userns_mode` and `cgroup`, `cap_add` outside an allowlist (default
`NET_BIND_SERVICE`; `ALL` never), `devices` (including GPUs),
`device_cgroup_rules`, unsafe `security_opt`, `cgroup_parent`, `volumes_from`,
bind mounts outside the project folder (symlinks resolved), any mount of the
engine socket, named volumes that bind-mount host paths or use non-local
drivers, external volumes, build contexts, `env_file`, configs and secrets
outside the project folder, build SSH forwarding, and `sysctls` other than
`net.*`. These are the usual host-escape routes. The policy is not yet applied
by any method; deploys will enforce it, failing with `-32007` and the list of
violations.

## Development

```sh
go test ./... -race -count=1
docker compose up -d   # agent + Docker-in-Docker sidecar
```

The Docker integration tests only ever talk to the Docker-in-Docker sidecar,
never the host daemon. They are skipped unless `KI_TEST_DOCKER_HOST` is set;
run the whole suite with them enabled via:

```sh
docker compose run --rm test   # starts dind, waits until healthy, runs go test
# or just one package, with the sidecar already up (`docker compose up -d dind`):
docker compose run --rm test go test -race -count=1 ./internal/server/...
docker compose down -v         # clean up (including the dind image cache)
```

## License

Apache-2.0, see [LICENSE](LICENSE).
