FROM golang:1.26.2-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w \
      -X github.com/open-southeners/ki-agent/internal/version.Version=${VERSION} \
      -X github.com/open-southeners/ki-agent/internal/version.Commit=${COMMIT}" \
    -o /out/ki-agent ./cmd/ki-agent

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git openssh-client \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ki-agent /usr/local/bin/ki-agent
# Docker CLI and Compose plugin: deploys run `docker compose` against the
# engine in docker.host. Both are static binaries. The plugin must be new
# enough for `config --no-env-resolution` to leave env_file entries unresolved
# (2.40.3 inlines the files into the model, bypassing the policy): keep v5+.
COPY --from=docker:28-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker/compose-bin:v5.1.0 /docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose
ENTRYPOINT ["/usr/local/bin/ki-agent"]
CMD ["run"]
