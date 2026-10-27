# Web panel: built with bun, embedded into the agent by the Go build below.
FROM oven/bun:1.4.2 AS web
WORKDIR /src/web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
# vite writes to ../internal/panel/dist (see web/vite.config.ts).
RUN bun run build

FROM golang:1.26.2-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/panel/dist ./internal/panel/dist
ARG VERSION=dev
ARG COMMIT=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w \
      -X github.com/open-southeners/ki-agent/internal/version.Version=${VERSION} \
      -X github.com/open-southeners/ki-agent/internal/version.Commit=${COMMIT}" \
    -o /out/ki ./cmd/ki

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git openssh-client \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ki /usr/local/bin/ki
# Old name, for scripts and units that still call ki-agent.
RUN ln -s ki /usr/local/bin/ki-agent
# Docker CLI and Compose plugin: deploys run `docker compose` against the
# engine in docker.host. Both are static binaries. The plugin must be new
# enough for `config --no-env-resolution` to leave env_file entries unresolved
# (2.40.3 inlines the files into the model, bypassing the policy): keep v5+.
COPY --from=docker:28-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker/compose-bin:v5.1.0 /docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose
ENTRYPOINT ["/usr/local/bin/ki"]
CMD ["agent", "run"]
