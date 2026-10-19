# ki-agent

An open-source (Apache-2.0) background agent that manages one Linux server's
services for an external control plane. It is written in Go and ships as a
single static binary.

**Status: pre-alpha.** The service, one-line installer, configuration, systemd
packaging, the JSON-RPC endpoint, Docker container management, Git-backed
projects, the command-line client and device-flow enrollment into a control
plane exist; deploying compose projects is under development.

## Install

On a Linux server (amd64 or arm64) with systemd:

```sh
curl -fsSL https://github.com/open-southeners/ki-agent/releases/latest/download/install.sh | sudo sh
```

The script downloads the release archive for your architecture, verifies its
sha256 checksum (always) and then installs:

- the binary at `/usr/local/bin/ki-agent`;
- a `ki-agent` system user and group, and the `ki-agent.service` unit, which
  runs the agent as that user (with the `docker` group so it can reach the
  engine), enabled and started;
- a default `/etc/ki-agent/config.yaml`, written only if none exists.

| Flag | Effect |
|---|---|
| `--version vX.Y.Z` | Install that release instead of the latest |
| `--with-docker` | Install Docker Engine (via get.docker.com) if it or the Compose plugin is missing |
| `--enroll [URL]` | Run `ki-agent enroll [URL]` once the service is up (see below) |
| `--no-start` | Enable the service but do not start it |
| `--add-user USER` | Add `USER` to the `ki-agent` group (default: the user who ran `sudo`) |

Pass flags with `sh -s -- --version v1.2.3 --enroll`. To **upgrade**, run the
same command again: it replaces the binary, restarts the service and keeps your
edited config.

Every release has a `checksums.txt` signed keylessly with
[cosign](https://github.com/sigstore/cosign) in CI. If `cosign` is installed the
installer also verifies that signature and refuses to continue when it fails;
otherwise it says so and relies on the checksum alone.

The CLI talks to the agent over a socket owned by the `ki-agent` group, so
users who should run `ki-agent status` and friends must be in that group
(`sudo usermod -aG ki-agent USER`, then log in again) or use `sudo`.

## Prerequisites

On the server: Docker with the Compose plugin, `git` and an `ssh` client (the
agent shells out to them). The container image already includes `git` and
`openssh-client`.

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

The agent logs JSON to stderr and stops on SIGINT or SIGTERM. To install by hand
instead of with the installer, create a `ki-agent` user and group (member of
`docker`), copy the binary to `/usr/local/bin/ki-agent`, the config to
`/etc/ki-agent/config.yaml` and `packaging/systemd/ki-agent.service` to
`/etc/systemd/system/`; the service runs as the `ki-agent` user.

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

## Enroll into a control plane

Instead of editing config and copying a token, you can approve the server in a
browser. The control plane must support the device-flow enrollment described in
[`docs/protocol.md`](docs/protocol.md) (the example control plane does, with
`-enroll`):

```sh
ki-agent enroll https://cp.example.com
# Open https://cp.example.com/activate?user_code=ABCD-EFGH and confirm the code ABCD-EFGH
```

Open the printed address, check the code and approve; the agent stores the
credentials and connects at once, without a restart. The URL may be omitted when
the binary was built with a default. Ctrl-C cancels. If the agent is already
enrolled the command stops with a hint; pass `--force` to enroll again.
`ki-agent unenroll` (`--yes` skips the prompt) deletes the credentials and
disconnects; `ki-agent status` shows whether the connection came from
enrollment or from the config.

Credentials live in `<data_dir>/control-plane.json` (mode 0600). While that file
exists it replaces `control_plane.url` and `control_plane.token_file`; the other
`control_plane.*` settings (CA file, heartbeat, reconnect) still apply.
Enrollment is local-only: `enrollment.*` methods are refused with `-32001` on
the WebSocket listener and on the control-plane connection itself, so a remote
peer can never re-point the agent.

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

## Projects

A project is a Git repository with a Compose file that the agent keeps checked
out under its data directory. Create one from a URL; the agent infers the rest:

```sh
ki-agent projects create https://github.com/acme/app.git
```

The slug comes from the repository name (override with `--slug`), the
deployment trigger is the repository's default branch (`--branch NAME` or
`--tags GLOB` to change it) and the Compose file is the first of `compose.yaml`,
`compose.yml`, `docker-compose.yaml`, `docker-compose.yml` (`--compose PATH` to
choose another). Nothing is stored if the clone fails.

**Private repositories** use one of two credentials:

- **Deploy key (SSH URLs).** `ki-agent projects create git@github.com:acme/app.git`
  makes the agent generate a key and print its public half. The project starts
  as `pending_access`; add the key to the repository as a read-only deploy key,
  then run `ki-agent projects verify app` to clone it and mark it `ready`.
- **Access token (HTTPS URLs).** Pass it on standard input or from a file so it
  never reaches the process list: `ki-agent projects create URL --token-stdin`
  or `--token-file PATH` (`--token-user` if the host wants a username other
  than `x-access-token`).

Keys and tokens are encrypted at rest and never returned by the API. Create
also prints a **webhook** path and secret, shown only once; configure it in
GitHub (content type `application/json`, "push" event) or GitLab ("Push events"
and "Tag push events"). Rotate it with `projects update --rotate-webhook-secret`.
Receiving webhooks and deploying are still under development.

`projects delete` removes the project's containers and networks, then its
checkout and record. Volumes are kept unless you pass `--volumes`. It asks you
to type the slug (`--yes` skips this) and aborts without removing anything if
Docker is unreachable.

### Command-line client

The same binary talks to a running agent over its Unix socket:

| Command | Purpose |
|---|---|
| `ki-agent status` | Agent version and uptime, control-plane state, engine, project count |
| `ki-agent projects create <git-url>` | Create a project (`--slug`, `--branch`/`--tags`, `--compose`, `--token-stdin`/`--token-file`, `--token-user`) |
| `ki-agent projects list` / `get <slug>` | Show projects |
| `ki-agent projects verify <slug>` | Check access, clone or update the checkout |
| `ki-agent projects update <slug>` | Change trigger, Compose file or credentials; `--rotate-webhook-secret`, `--rotate-deploy-key` |
| `ki-agent projects delete <slug>` | Delete (`--volumes`, `--yes`) |
| `ki-agent logs <container>` | Follow logs (`--tail`, `--since`, `--timestamps`) |

`--socket PATH` selects the socket (default: `KI_SOCKET_PATH`, then
`socket.path` from the config file, then `/run/ki-agent/ki.sock`). Every client
command accepts `--json` to print the raw result. You need access to the socket:
add your user to the `ki-agent` group or use `sudo`.

### Data directory

```
<data_dir>/state.db            SQLite database (projects)
<data_dir>/secret.key          key that encrypts stored credentials (0600; back it up)
<data_dir>/projects/<slug>/    each project's Git checkout
<data_dir>/git/                Git's home directory and SSH known_hosts
<data_dir>/tmp/                staging area for clones
```

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
