# ki-agent

An open-source (Apache-2.0) background agent that manages one Linux server's
services for an external control plane. It is written in Go and ships as a
single static binary.

**Status: pre-alpha.** The service, configuration, systemd packaging and the
JSON-RPC endpoint exist; container management is under development.

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
token file.

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

## Development

```sh
go test ./... -race -count=1
docker compose up -d   # agent + Docker-in-Docker sidecar on the devmesh network
```

## License

Apache-2.0, see [LICENSE](LICENSE).
