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
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ki-agent /usr/local/bin/ki-agent
ENTRYPOINT ["/usr/local/bin/ki-agent"]
CMD ["run"]
