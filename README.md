<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo-light.svg" alt="ki-agent logo" width="120" height="120">
  </picture>
</p>

# ki-agent

An open-source (Apache-2.0) background agent that manages one Linux server's
services for an external control plane. It is written in Go and ships as a
single static binary.

**Status: pre-alpha.** The service, one-line installer, configuration, systemd
packaging, the JSON-RPC endpoint, Docker container management, Git-backed
projects, the command-line client, device-flow enrollment into a control
plane, policy-checked deploys of compose projects (from the CLI, the API or
push webhooks) and an HTTPS edge proxy that publishes services on domains exist.

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
| `--no-panel` | Write `panel.enabled: false` into a newly created config, so the web panel is not served (see [Users, roles and tokens](#users-roles-and-tokens)) |
| `--no-edge` | Write `edge.enabled: false` into a newly created config, so no reverse proxy is started (see [Exposing services](#exposing-services)) |
| `--tunnel-token-file FILE` | Expose the server through a Cloudflare Tunnel: a new config gets `edge.mode: behind` and `tunnel.enabled: true`, and the token in `FILE` is stored once the service is up (see [Exposing a server through Cloudflare Tunnel](#exposing-a-server-through-cloudflare-tunnel)) |
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

### Installing a local build

Before a public release, or to try an unreleased branch, build release-shaped
archives from a checkout (needs `go` and [bun](https://bun.sh)) and install
from them:

```sh
scripts/local-release.sh                 # optional: [VERSION] [OUTDIR]; writes ./dist-local
KI_INSTALL_BASE_URL=file://$PWD/dist-local sh dist-local/install.sh --no-start

# or on another server:
scp -r dist-local user@server:/tmp/ki-agent-dist
ssh user@server 'sudo KI_INSTALL_BASE_URL=file:///tmp/ki-agent-dist sh /tmp/ki-agent-dist/install.sh'
```

The directory holds `ki-agent_linux_amd64.tar.gz`, `ki-agent_linux_arm64.tar.gz`,
`checksums.txt` and `install.sh`, named like a GitHub release, so the
installer verifies the sha256 checksum as usual. There is no cosign signature
for a local build, so the installer must run on a machine without `cosign`
installed (it would otherwise insist on the signature bundle).

## Prerequisites

On the server: Docker with the Compose plugin, `git` and an `ssh` client (the
agent shells out to them). The container image already includes `git`,
`openssh-client`, the Docker CLI and the Compose plugin. Use a current Compose
plugin (the image ships v5.1.0): older releases such as 2.40.3 ignore
`--no-env-resolution` for `env_file`, which the deploy safety checks rely on.

## Build

```sh
go build -o ki-agent ./cmd/ki-agent
```

Stamp a release version with
`-ldflags "-X github.com/open-southeners/ki-agent/internal/version.Version=1.2.3"`.

### Web panel

The release binary embeds the panel. To build it from source you need
[bun](https://bun.sh) (1.4); without it the agent still builds and serves a
minimal sign-in page.

```sh
cd web && bun install --frozen-lockfile && bun run build   # writes internal/panel/dist
cd .. && go build -o ki-agent ./cmd/ki-agent               # embeds it
```

For frontend work run the agent (the panel listens on `127.0.0.1:8091`) and
`cd web && bun run dev`: Vite serves the app on `http://localhost:5173` and
proxies `/rpc` and `/auth` to `127.0.0.1:8091`. `bun run typecheck` and
`bun run test` check the code.

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
`KI_API_LISTEN`. Every key is documented in
[`packaging/config.example.yaml`](packaging/config.example.yaml). The HTTP API
(`api.*`) and the web panel (`panel.*`) are on by default and bind loopback
only; see [Users, roles and tokens](#users-roles-and-tokens). `docker.host` (env `KI_DOCKER_HOST`, default
`unix:///var/run/docker.sock`) selects the container engine; the agent starts
even if the engine is down.

## Monitoring

The agent measures the host and the containers it manages and keeps the last
hour in memory (nothing is written to disk, so history starts empty after a
restart).

```sh
ki-agent stats                       # host summary plus the 10 busiest containers
ki-agent stats --watch --interval 2s # live view (needs a few seconds for rates)
ki-agent stats --json
ki-agent containers stats --project app [--watch] [--json]
```

What is measured:

- **Host**: CPU percent (all cores, optionally per core), load averages,
  memory and swap, the disks holding `/` and the data directory (plus
  `monitor.disk_paths`) with space and inode use, disk throughput summed over
  whole block devices, network throughput summed over the physical and VPN
  interfaces (loopback, `veth*`, `docker*`, `br-*` and the like are left out),
  and uptime.
- **Containers**: for every running ki-managed container and the edge proxy,
  CPU percent (100 is one fully used core, so it can exceed 100), memory use
  without reclaimable file cache (as `docker stats` shows it) and the limit,
  network and block I/O throughput, and the process count.

Rates come from two consecutive samples, so they are empty (`null`, shown as
`-`) in the first sample after the agent starts and for a container that has
just started or restarted. The agent samples the host every
`monitor.host_interval` (default 15s) and containers every
`monitor.container_interval` (default 30s); while clients subscribe it samples
as fast as the fastest subscriber asks (at least every 1s for the host and 2s
for containers) and goes back to the base interval afterwards. History keeps one
sample per `monitor.history_resolution` (default 15s) for `monitor.history`
(default 1h, at most 24h). Viewers may read all of it
(`host.stats`, `containers.stats` and their `subscribe` variants, see
[`docs/protocol.md`](docs/protocol.md)).

When the agent itself runs in a container (as in the development compose
file), the host figures describe what that container can see. To report the
real host, mount the host's `/proc` and `/sys` read-only (for example at
`/host/proc` and `/host/sys`) and set `HOST_PROC=/host/proc` and
`HOST_SYS=/host/sys`, which the sampling library honours. The packaged
systemd install is not containerised and needs nothing.

## Protocol

The agent speaks [JSON-RPC 2.0](https://www.jsonrpc.org/specification) over a
local Unix socket (`socket.path`, default `/run/ki-agent/ki.sock`, one JSON
message per line) and over a WebSocket at `/rpc` on the loopback listener
`api.listen` (default `127.0.0.1:8091`) that requires an API token
(`Authorization: Bearer <token>`) or a panel session. Both ends are
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
websocat -H 'Authorization: Bearer YOUR_TOKEN' ws://127.0.0.1:8091/rpc
{"jsonrpc":"2.0","id":1,"method":"agent.hello","params":{"protocol":"1.0.0"}}
{"jsonrpc":"2.0","id":2,"method":"agent.info"}
```

## Connecting to a control plane

Besides serving the Unix socket and the HTTP listeners, the agent
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
the HTTP listeners and on the control-plane connection itself, so a remote
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
| `containers.stats {project?, id?, history?}` | Latest resource usage per container (see [Monitoring](#monitoring)) |

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
choose another). Nothing is stored if the clone fails. To deploy prebuilt
images without a repository, see
[Compose projects](#compose-projects-images-built-in-ci).

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
and "Tag push events"). Rotate it with `projects update --rotate-webhook-secret`. See
[Deploying](#deploying) for what happens when it fires.

`projects delete` removes the project's containers and networks, then its
checkout and record. Volumes are kept unless you pass `--volumes`; the
anonymous ones (for example a database image's data volume) cannot be found
once their container is gone, so the command lists their names to remove by
hand. It asks you
to type the slug (`--yes` skips this) and aborts without removing anything if
Docker is unreachable.

### Command-line client

The same binary talks to a running agent over its Unix socket:

| Command | Purpose |
|---|---|
| `ki-agent status` | Agent version and uptime, control-plane state, engine, project count |
| `ki-agent projects create <git-url>` | Create a project (`--slug`, `--branch`/`--tags`, `--compose`, `--token-stdin`/`--token-file`, `--token-user`) |
| `ki-agent projects create <slug> --compose-file <file>` | Create a [compose project](#compose-projects-images-built-in-ci) from a Compose file (`-` for stdin) |
| `ki-agent projects create <slug> --image <ref>` | Create an [image project](#image-projects) (`--port`, `--domain`, `--path`, `--volume`, `--env`, `--service`) |
| `ki-agent projects image set <slug> <tag>` | Release a new image tag (or `--image <ref>`) of a compose project; `--deploy` |
| `ki-agent projects compose get\|set\|history <slug>` | Show, store (`--file`, `--note`, `--deploy`) or list versions of a compose project's file |
| `ki-agent projects list` / `get <slug>` | Show projects |
| `ki-agent projects verify <slug>` | Check access, clone or update the checkout |
| `ki-agent projects update <slug>` | Change trigger, Compose file or credentials; `--rotate-webhook-secret`, `--rotate-deploy-key` |
| `ki-agent projects delete <slug>` | Delete (`--volumes`, `--yes`) |
| `ki-agent logs <container>` | Follow logs (`--tail`, `--since`, `--timestamps`) |
| `ki-agent deploy --project <slug>` | Deploy and follow the log (`--ref`, `--detach`); exits non-zero if the deploy fails |
| `ki-agent deploys list <slug>` / `get <id>` / `logs <id>` | Deploy history, one deploy, its log (`--limit`, `--follow`) |
| `ki-agent projects env list <slug>` | List a project's variable names (never values) |
| `ki-agent projects env set <slug> <NAME>` | Set a variable; value from `--value-stdin` or `--value-file` |
| `ki-agent projects env import <slug> --file <dotenv>` | Set several variables from a dotenv file |
| `ki-agent projects env unset <slug> <NAME>...` | Remove variables |

`--socket PATH` selects the socket (default: `KI_SOCKET_PATH`, then
`socket.path` from the config file, then `/run/ki-agent/ki.sock`). Every client
command accepts `--json` to print the raw result. You need access to the socket:
add your user to the `ki-agent` group or use `sudo`.

### Data directory

```
<data_dir>/state.db            SQLite database (projects)
<data_dir>/secret.key          key that encrypts stored credentials (0600; back it up)
<data_dir>/projects/<slug>/    each project's Git checkout (deploys write .env here)
<data_dir>/deploys/<slug>/     the checked, labelled compose model that last ran
<data_dir>/git/                Git's home directory and SSH known_hosts
<data_dir>/tmp/                staging area for clones
<data_dir>/edge/               edge proxy config, certificates and ACME storage (see Exposing services)
```

### Deploying

`ki-agent deploy --project app` checks out the project's trigger (the branch
head, or the newest matching tag), runs `docker compose` on it and waits until
the services are running and healthy, streaming the log as it goes. `--ref`
deploys another branch, tag or commit as long as it matches the project's
trigger; `--detach` returns at once. Only one deploy runs per project at a
time, and a newer request replaces one that is still waiting. A failed deploy
leaves the containers as they are; the reason (`policy_violation`,
`build_failed`, `health_failed`, ...) is shown by the command and by
`ki-agent deploys get <id>`.

**Rolling back.** `ki-agent deploys rollback app` deploys the previous good
version again: the commit of the newest successful deploy before the latest
one (`--to <deploy-id>` picks a specific successful deploy; `--detach` returns
at once). It is a normal deploy of that exact commit, so the safety policy,
build and health checks all run again, and it is marked `rollback_of` in
`deploys get`. A commit that was force-pushed away fails at the fetch stage.

**Services.** `ki-agent services list app` shows each service with its state
(`running`, `partial`, `stopped`, `unhealthy`, `missing`), containers and
health; `ki-agent services start|stop|restart app web` acts on every container
of a service.

The Compose file is never run as written. The agent normalizes it, checks it
against the safety policy below and runs exactly that checked, labelled model.
Services, networks and volumes get `ki.managed`, `ki.project` and `ki.service`
labels, so they show up in `containers.*` and are removed with the project.

**Environment variables** are set per project and stored encrypted; values are
never shown again:

```sh
printf %s "$SECRET" | ki-agent projects env set app API_KEY --value-stdin
ki-agent projects env import app --file .env.production
ki-agent projects env list app
```

They are written to the checkout's `.env` at deploy time and are also used for
`${VAR}` interpolation of the Compose file. Redeploy to apply a change.

**Webhooks.** With the edge enabled (the default) the webhook listener starts
by itself on `127.0.0.1:8090` and the edge publishes it as
`https://<your host>/hooks/<id>`; see [Exposing services](#exposing-services).
Without the edge, set `webhooks.listen` (for example `127.0.0.1:8090`, behind a
reverse proxy that terminates TLS, or any address together with
`webhooks.tls_cert_file` and `webhooks.tls_key_file`) and point GitHub or
GitLab at `https://<your host>/hooks/<id>` using the path and secret printed by
`projects create`. A push to the trigger branch (or a tag matching the tag
pattern) deploys that exact commit; other pushes are ignored. GitHub requests
are verified with the HMAC signature, GitLab requests with the secret token.

```yaml
webhooks:
  listen: 127.0.0.1:8090
deploys:
  health_timeout: 5m   # how long to wait for healthy services
  keep: 50             # deploys kept per project
```

The agent runs `docker compose` through the Docker CLI at `docker.cli`
(default `docker`), which must have the Compose v2 plugin; the container image
includes both.

### Safety policy

Docker group membership is root-equivalent on the host, so every deploy is
checked before anything runs. The `internal/policy` library checks the
normalized model from `docker compose config --format json` and rejects, per
service: `privileged`, host/foreign `network_mode`, `pid`, `ipc`, `uts`,
`userns_mode` and `cgroup`, `cap_add` outside an allowlist (default
`NET_BIND_SERVICE`; `ALL` never), `devices` (including GPUs),
`device_cgroup_rules`, unsafe `security_opt`, `cgroup_parent`, `volumes_from`,
bind mounts outside the project folder (symlinks resolved), any mount of the
engine socket, named volumes that bind-mount host paths or use non-local
drivers, external volumes, build contexts, `env_file`, configs and secrets
outside the project folder, build SSH forwarding, `sysctls` other than `net.*`,
`provider` services and any `runtime` other than `runc`. These are the usual
host-escape routes. A deploy with violations fails with `policy_violation` and
the list of violations; nothing is built or started.

### Compose projects (images built in CI)

A **compose project** has no repository: the agent stores the Compose file
itself, as numbered versions, and only runs prebuilt images. Use it when CI
builds and pushes the images and the server never needs access to your source
(or to GitHub); use a [git project](#projects) when the server should check out
the repository and build from it, or deploy on a push webhook. A compose
project has no webhook, trigger or credentials, and `projects verify` and
`projects update --rotate-*` do not apply to it.

```sh
# Create it from a file (the slug is required); this stores version 1
ki-agent projects create demo --compose-file compose.yaml
ki-agent deploy --project demo

# Store a new version and deploy it in one step
ki-agent projects compose set demo --file compose.yaml --note "bump web to v2" --deploy

# Inspect and roll back
ki-agent projects compose history demo
ki-agent projects compose get demo --version 1
ki-agent deploys rollback demo
```

A file is checked when it is stored, not only when it is deployed: it must
parse, pass the [safety policy](#safety-policy) and must not use `build:`,
`include`, `extends` or local `env_file` paths, since there is no checkout next
to it. A rejected file is not stored and the error says why (`compose_invalid`,
or `policy_violation` with `build_not_allowed` for a `build:` section). Pin
images by tag or digest (`image: ghcr.io/acme/web:1.4.2`); a deploy pulls them
first, and anything that changes the running services must be a new version.
Environment variables work as for git projects (`projects env set`) and are
used for `${VAR}` interpolation. Deploys are `compose@v<N>` (`ki-agent deploy
--project demo --ref v2`), and `deploys rollback` re-deploys the previous
version that ran successfully.

**From CI.** Create an admin API token on the server
(`ki-agent tokens create ci --role admin --expires-in 2160h`; storing a compose
version needs the admin role) and keep it as a secret. The RPC API takes the
same JSON as above over the WebSocket (see [Protocol](#protocol)); the token
works on the loopback listener, and through the edge when `api.public_tokens`
is `true`. A GitHub Actions job that builds the images, then publishes the new
Compose file and deploys it:

```yaml
- name: Deploy to the server
  env:
    KI_URL: wss://ki.example.com/rpc          # your agent's API address
    KI_TOKEN: ${{ secrets.KI_TOKEN }}
  run: |
    sed -i "s|:latest|:${GITHUB_SHA}|" compose.yaml     # pin the image this run pushed
    hello='{"jsonrpc":"2.0","id":1,"method":"agent.hello","params":{"protocol":"1.0.0"}}'
    set=$(jq -nc --rawfile c compose.yaml --arg n "$GITHUB_SHA" \
      '{jsonrpc:"2.0",id:2,method:"projects.compose.set",
        params:{slug:"demo",compose:$c,note:$n,deploy:true}}')
    { printf '%s\n%s\n' "$hello" "$set"; sleep 5; } |
      websocat -H "Authorization: Bearer $KI_TOKEN" "$KI_URL" | tee reply.json
    jq -e 'select(.id==2) | .result.deploy.id' reply.json
```

The reply carries the queued `deploy`; the step fails when the file is
rejected (`error.data` has the reason and violations). Follow the result with
`deploys.get`, or `ki-agent deploys get <id>` on the server.

#### Image projects

Most apps are one container. For those you do not need to write a Compose
file: give the agent an image, a port and a domain and it generates the file,
stores it as version 1 and runs it like any compose project (policy, history,
rollback, environment variables).

```sh
ki-agent projects create demo --image registry.example.com/team/app:1.4.2 \
  --port 8080 --domain demo.example.com --volume data:/app/data --env GREETING=hi
ki-agent projects compose get demo        # the generated file
ki-agent deploy --project demo
```

```yaml
services:
  app:
    image: registry.example.com/team/app:1.4.2
    restart: unless-stopped
    env_file:
      - path: .env
        required: false
    environment:
      GREETING: "hi"
    expose:
      - "8080"
    volumes:
      - data:/app/data
    labels:
      ki.expose.port: "8080"
      ki.expose.domains: "demo.example.com"
volumes:
  data:
```

The image must carry a tag or digest (`nginx:1.27`, `ghcr.io/acme/web@sha256:...`);
`latest` and untagged references are refused. Volumes are named volumes only
(`name:/path[:ro]`; bind mounts are refused), `--env` is for plain values and
secrets go through `projects env set`. The panel's **New project** button
offers the same form. For anything more (several services, commands), use a
Compose file.

To release, change the tag; this works on any compose project and keeps the
rest of the file:

```sh
ki-agent projects image set demo 1.4.3 --deploy
ki-agent projects image set demo --image registry.example.com/team/app:1.4.3   # full reference
ki-agent deploys rollback demo                                                  # back to the previous version
```

**From CI.** The pipeline only has to send the new tag; it does not need the
Compose file. With the same admin token and connection as above, replace the
`projects.compose.set` call by:

```sh
set=$(jq -nc --arg t "$GITHUB_SHA" \
  '{jsonrpc:"2.0",id:2,method:"projects.image.set",
    params:{slug:"demo",tag:$t,note:$t,deploy:true}}')
```

The reply is `{version, image, deploy}`. For an image in a private registry the
agent logs in with its own Docker configuration, as for git projects (see
[Private registries](#private-registries-ecr)); a Registries tab in the panel
to manage logins is planned.

**IPv6-only servers.** A compose project never contacts GitHub (or any Git
host), so a server whose projects are all compose projects can run IPv6-only,
provided the registries it pulls from are reachable over IPv6. A git project
clones from the repository's host, and GitHub does not serve Git over IPv6, so
a server with any git project on GitHub needs IPv4 (or a proxy).

## Users, roles and tokens

Remote access is authenticated. Three roles, each including the ones before it:
**viewer** (read-only: list and inspect containers, projects, deploys, edge
status), **operator** (also start, stop and restart containers and trigger
deploys) and **admin** (everything else, including projects, environment
variables, exposure, users and tokens). The local Unix socket and the control
plane act as admin. The full method-to-role table is in
[`docs/protocol.md`](docs/protocol.md) and in `x-role` of
[`api/openrpc.json`](api/openrpc.json).

```sh
ki-agent panel setup                       # first admin: prints a one-time link (the installer runs this)
ki-agent users create bob --role operator  # prints bob's one-time setup link
ki-agent users list
ki-agent users update bob --role viewer    # or --disable / --enable
ki-agent users reset bob                   # lost password or authenticator: new link
ki-agent users delete bob
ki-agent tokens create ci --role operator --expires-in 720h   # shown once
ki-agent tokens list
ki-agent tokens revoke ci
ki-agent audit --limit 50                  # who did what
```

Open the link in a browser to choose a password and enrol an authenticator app;
links are valid for 30 minutes and work once. The panel is served at `/` on the
edge's hostnames and IP (HTTPS) and at `http://127.0.0.1:8091/` on the server
itself. Its session cookie is `Secure`, so it needs HTTPS or `localhost`.
Disable the panel with `panel.enabled: false`, `KI_PANEL_ENABLED=false`,
`ki-agent run --no-panel` or `install.sh --no-panel`; the API and tokens keep
working on `127.0.0.1:8091`.

API tokens authenticate WebSocket clients on `api.listen`. Through the edge they
are refused unless `api.public_tokens: true`; panel sessions always work there.
With `--no-edge` there is no remote route at all (the panel and API stay on
loopback). Setup links use `panel.public_url`, else `edge.domain`, else the
server's IP address together with the self-signed certificate fingerprint to
compare in the browser.

**Migrating from `websocket.*`**: the `websocket` section and its token file
were removed before the first release. Replace `websocket.listen` with
`api.listen` (loopback only, default `127.0.0.1:8091`), the heartbeat keys with
`api.heartbeat_interval` / `api.heartbeat_timeout`, `KI_WEBSOCKET_*` with
`KI_API_*`, and create tokens with `ki-agent tokens create` instead of a token
file. The agent refuses to start while a `websocket:` key remains.

## Web panel

Sign in at the panel (`https://<your host>/`, or `http://127.0.0.1:8091/` on the
server) after following the setup link from `ki-agent panel setup`. It follows
the system light or dark theme, shows the connection state in the header and
hides the actions your role does not allow.

| Page | What it does | Role |
|---|---|---|
| Overview | Live host CPU, load, memory, swap, disk and network with the last hour of history, a summary of every project and the edge status | viewer |
| Projects | All projects with status, last deploy and service health | viewer |
| Project | Repository and trigger, the services table (state, health, containers, exposure links), deploy history with a live log, container logs and stats | viewer |
| Project: Deploy now, Rollback, start/stop/restart a service | Trigger a deploy (optionally of another ref), return to a previous good deploy, act on a service | operator |
| Users | Create users (shows the setup link), change roles, disable, reset, delete | admin |
| API tokens | Create (the secret is shown once) and revoke | admin |
| Audit log | Who did what, paged | admin |
| Environment, Exposure | Set and unset a project's variables (names only are shown) and publish or unpublish services | admin |
| Edge | Proxy state, ports, certificates and routes | admin |

## Exposing services

The agent runs a managed reverse proxy (the **edge**) as a Docker container
that publishes project services on your domains with HTTPS, and forwards push
webhooks. It is enabled by default and binds host ports **80 and 443**. Set
`edge.enabled: false` (or install with `--no-edge`) if something else owns
those ports.

Expose a service with labels in the project's Compose file:

```yaml
services:
  web:
    image: nginx
    labels:
      ki.expose.port: "80"
      ki.expose.domains: "app.example.com,www.example.com"
      ki.expose.path: "/"        # optional path prefix
```

or from the server, which overrides the labels of that service:

```sh
ki-agent projects expose app web --port 80 --domain app.example.com
ki-agent projects unexpose app web
```

A domain belongs to one project and service on the server; a second project
claiming it is refused (a deploy fails with `domain_conflict`). Other `ki.*`
labels and all `traefik.*` labels are rejected by the safety policy. For each
domain, DNS must point at the server and port 80 must be reachable from the
internet, because certificates are obtained from Let's Encrypt on demand.

```yaml
edge:
  enabled: true
  domain: ""           # this server's own domain (ACME certificate); optional
  acme_email: ""       # optional ACME account contact
  http_port: 80
  https_port: 443
  webhooks_port: 8090  # loopback port of the webhook listener
```

`ki-agent edge status` shows the proxy state, ports, server hostnames, routes
and certificates. Requests to the bare IP (or an unknown host) get a persistent
**self-signed** certificate; the same command prints its SHA-256 fingerprint
so you can compare it with what your browser or webhook sender shows before
trusting it. Prefer a domain and let ACME certify it. Webhooks must use
`https://`; plain HTTP is redirected. Restarting the agent leaves the proxy
running. A port conflict or an engine problem does not stop the agent: it shows
up as `state: error` in `edge status`. A control plane can install an origin
certificate for the server's hostname with `edge.csr` and
`edge.certificate.install` (see [`docs/protocol.md`](docs/protocol.md)).

## AWS

`ki-agent aws status` (and the `aws.status` method, and the Overview page of the
web panel) tells whether the server is an EC2 instance and which AWS agents run
on it:

```
Instance          i-0123456789abcdef0 (t3.small)
Region            eu-west-1 (eu-west-1a)
AMI               ami-0123456789abcdef0
Instance profile  arn:aws:iam::123456789012:instance-profile/example
IMDSv1            disabled
SSM agent         active, enabled, 3.3.0.0 (snap)
CloudWatch agent  not installed
```

The agent reads the instance metadata service with IMDSv2 only, never touches
instance credentials, and caches the answer for 10 minutes. Off EC2 the command
prints `EC2  not detected` within about a second. A warning is shown when the
unauthenticated IMDSv1 is still enabled. Agent state is read from systemd
without root. What the agents report and why registration is not shown is
explained in [`docs/aws-agents.md`](docs/aws-agents.md).

## Exposing a server through Cloudflare Tunnel

A server without a fixed public IPv4 address, or with ports 80/443 closed, can
be reached through a [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/):
the agent runs `cloudflared` (a managed `ki-tunnel` container, pinned image
`cloudflare/cloudflared`), which connects **outbound** to Cloudflare.
Cloudflare terminates TLS and forwards each public hostname through the tunnel
to the edge in behind mode. No inbound port needs to be open.

1. In the Cloudflare Zero Trust dashboard create a **remotely managed**
   tunnel and copy its token. Add one public hostname per domain (the panel
   hostname, every project domain) with the service `http://localhost:8480`
   (the default `edge.behind_listen`).
2. Configure the agent and restart it:

   ```yaml
   edge:
     mode: behind
   tunnel:
     enabled: true
   ```

3. Store the token (read from standard input or a file, never from an
   argument, so it does not reach the process list or shell history):

   ```sh
   ki-agent tunnel configure --token-stdin      # paste the token, Ctrl-D
   ki-agent tunnel configure --token-file /root/tunnel.token
   ki-agent tunnel status
   ```

`ki-agent tunnel status` (also on the Overview and Edge pages of the web panel)
shows the state: `unconfigured` (enabled, no token yet), `starting`,
`connected` (with the number of active connections, from the connector's
`/ready` endpoint on `127.0.0.1:tunnel.metrics_port`, default 20241),
`disconnected` or `error`. It warns when the edge is not in behind mode.
`ki-agent tunnel remove` deletes the token and the container. The installer can
do steps 2 and 3 on a fresh install with `--tunnel-token-file FILE`.

The token is stored in a root-only file under `<data_dir>/tunnel/secret` and is
handed to `cloudflared` as a read-only mounted file (`--token-file`, cloudflared
2025.4.0 or later), so it is not in the command line, the container environment,
the generated Compose file, the logs or any RPC result. The connector runs as
the agent's user with all capabilities dropped and a read-only root filesystem.
Changing the token restarts the container. The agent never changes the tunnel's
public hostnames: they are managed in Cloudflare.

Since anyone who can reach a public hostname reaches the panel, protect the
panel hostname with [Cloudflare Access](https://developers.cloudflare.com/cloudflare-one/policies/access/)
in addition to the panel's own sign-in.

## Migrating a server that already runs a web server

Servers managed with Forge (nginx) or a hand-written Caddy already own ports 80
and 443, so the edge cannot start in its default **front** mode. Run it in
**behind** mode instead: Traefik listens on a loopback address in plain HTTP,
and the existing web server keeps its certificates and forwards the migrated
domains to it. Sites move one at a time.

1. Configure the agent (`/etc/ki-agent/config.yaml`) and restart it:

   ```yaml
   edge:
     mode: behind
     behind_listen: 127.0.0.1:8480   # default
     domain: ki.example.com          # optional: panel and webhooks hostname
   ```

2. Add `edge.domain` (if set) as a site on the existing web server with its own
   certificate, and make it forward as in the next step.
3. For each site you move, expose it from a project (`ki.expose.*` labels or
   `ki-agent projects expose`), deploy it, then print the block to add:

   ```sh
   ki-agent edge proxy-config                         # nginx, every exposed domain
   ki-agent edge proxy-config --domain app.example.com
   ki-agent edge proxy-config --format caddy
   ```

   The output proxies to the behind listener, preserving `Host`,
   `X-Forwarded-For`, `X-Forwarded-Proto` and WebSocket upgrades. With Forge,
   paste it into the site's **Edit Nginx Configuration** (replacing the old
   `location /` that served the legacy app). The agent never writes outside its
   data directory; you add the blocks yourself.
4. When the last site has moved, switch to **front** mode: stop the web server
   (`systemctl disable --now nginx`), set `edge.mode: front` and restart the
   agent (`systemctl restart ki-agent`). The edge now binds 80/443 and obtains
   certificates itself (or serves ones you import, see below). Until then
   nothing but the web server is public.

In behind mode the edge requests no ACME certificates, does not redirect to
HTTPS and does not publish bare-IP routes; the web server in front decides what
reaches the agent. `ki-agent edge status` and the Edge page show the mode and
the listen address.

## Bring your own certificate

Sites whose certificate you already have (for example a Cloudflare origin
wildcard) are served with it instead of ACME. Import it over the local socket,
as a user in the `ki-agent` group or with sudo:

```sh
ki-agent edge certificate import --cert fullchain.pem --key privkey.pem
ki-agent edge certificate import --cert fullchain.pem --key privkey.pem --server
ki-agent edge certificate remove <id>          # id as shown by `edge status`
```

The key must match the leaf, and the leaf must be valid for at least 24 more
hours. The names are the certificate's DNS SANs and may include wildcards such
as `*.example.com` (one label deep, so it covers `app.example.com` but not
`a.b.example.com`). A project whose every domain is covered by an imported (or
installed) certificate is served with it; other projects keep using ACME.
`--server` also makes the certificate's plain names server hostnames (panel,
webhooks), reserved against project exposure. The key and certificate are
stored under the data directory with mode 0600. Import is available on the
local socket only: private keys never go through the web panel or the network
API, which lists imported certificates (names, wildcard, server flag, expiry)
and can remove them.

## Private registries (ECR)

Deploys run `docker compose` with their own Docker client configuration, so
registry logins do not depend on a user's home directory. It lives in
`docker.config_dir` (default `<data_dir>/docker`, mode 0700). Put credentials
there with `docker --config <dir> login <registry>`, or use a credential helper.
For Amazon ECR use [`docker-credential-ecr-login`](https://github.com/awslabs/amazon-ecr-credential-helper):

1. Install the helper so it is in the `PATH` of the service (for example
   `/usr/local/bin/docker-credential-ecr-login`).
2. Write `<config_dir>/config.json` (owned by `ki-agent`):

   ```json
   {"credHelpers": {"123456789012.dkr.ecr.eu-west-1.amazonaws.com": "ecr-login"}}
   ```

3. Give the instance role read access to ECR (`ecr:GetAuthorizationToken` and
   the `ecr:BatchGetImage` / `ecr:GetDownloadUrlForLayer` family); the helper
   uses it through the instance metadata service, so no secrets are stored.

Image-only services are refreshed at the start of every deploy (a `pull` stage
running `docker compose pull --ignore-buildable`) so `latest`-style tags
update. A failure is reported as `pull_failed` with Docker's message. Set
`deploys.pull: missing` to skip the stage and let `up` pull only images that
are absent. With the systemd unit the directory must be writable by the
service: the default is inside the data directory; for another location add it
to `ReadWritePaths=` in a drop-in.

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
