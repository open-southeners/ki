# Image for the installer test: a plain Debian (no git, ssh or Docker) plus a
# freshly built ki-agent binary at /ki-agent. Build context is the repo root.
FROM golang:1.26.2-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/ki-agent ./cmd/ki-agent

FROM debian:bookworm-slim
COPY --from=build /out/ki-agent /ki-agent
