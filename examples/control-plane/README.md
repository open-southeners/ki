# Example control plane

A minimal control plane for ki-agent in about 300 lines. It accepts agents that
dial out (`control_plane.url`), checks their bearer token, then speaks the
agent API as a JSON-RPC client: `agent.hello` and `agent.info` (pipelined), a
heartbeat subscription and, optionally, a container log subscription. It uses
only the standard library and `github.com/coder/websocket`, and imports nothing
from `internal/`, to show that the protocol (see
[`docs/protocol.md`](../../docs/protocol.md)) can be implemented from outside.

Each connected agent gets its own goroutine and log prefix (its remote
address), so several agents can connect at once. Ctrl-C closes every
connection cleanly.

## Run it locally

1. Start the control plane:

   ```sh
   echo -n 's3cret-token' > /tmp/cp-token
   go run ./examples/control-plane -addr 127.0.0.1:8080 -token s3cret-token
   ```

   Flags: `-addr` (default `127.0.0.1:8080`), `-path` (default `/agent`),
   `-token` (required), `-logs <container>` (follow a managed container's logs).

2. Point an agent at it. In the agent's config (`ws://` is allowed because the
   host is loopback; use `wss://` everywhere else):

   ```yaml
   control_plane:
     url: ws://127.0.0.1:8080/agent
     token_file: /tmp/cp-token
   ```

   ```sh
   ki-agent run --config config.yaml
   ```

3. Watch the control plane print the session. Restart either side and the
   agent reconnects with backoff; stop the agent and the session ends.

To follow container logs, label a container `ki.managed=true` and add
`-logs <name>`; stopped containers end the stream with `{"eof":true}`.

## Sample output

Captured from a real agent (container engine not running) with the
configuration above; Ctrl-C was pressed on the control plane after a few
heartbeats and the agent started its reconnect backoff.

```
2026/10/14 18:46:01 control plane listening on ws://127.0.0.1:18080/agent
[127.0.0.1:33722] 2026/10/14 18:46:02.411644 connected (ki-agent/dev, protocol 1.0.0)
[127.0.0.1:33722] 2026/10/14 18:46:02.412545 hello: {"agent":{"commit":"dev","name":"ki-agent","version":"dev"},"capabilities":["heartbeat","containers"],"protocol":"1.0.0"}
[127.0.0.1:33722] 2026/10/14 18:46:02.412619 info: {"arch":"amd64","commit":"dev","control_plane":{"state":"connected","url":"ws://127.0.0.1:18080/agent","connected_since":"2026-10-14T18:46:02.411632064Z","reconnects":0},"go_version":"go1.26.1","hostname":"unikialab","os":"linux","uptime_ms":1,"version":"dev"}
[127.0.0.1:33722] 2026/10/14 18:46:04.414412 heartbeat #1: {"time":"2026-10-14T18:46:04.413041896Z","uptime_ms":2003}
[127.0.0.1:33722] 2026/10/14 18:46:06.415149 heartbeat #2: {"time":"2026-10-14T18:46:06.413041815Z","uptime_ms":4004}
[127.0.0.1:33722] 2026/10/14 18:46:08.414214 heartbeat #3: {"time":"2026-10-14T18:46:08.41304193Z","uptime_ms":6003}
[127.0.0.1:33722] 2026/10/14 18:46:09.369298 disconnected
```
