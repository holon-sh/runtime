FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS runtime
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY host host
ARG TARGETARCH
RUN CGO_ENABLED=0 GOARCH=$TARGETARCH go build -trimpath -buildvcs=false -ldflags='-s -w' -o /holon-mcp-runtime ./cmd/holon-mcp-runtime

# Carry the interpreters and package managers shipped in the previous release.
FROM ghcr.io/holon-sh/development@sha256:799756844538e51f867b2ecb0f82bd311a2090a8bf476c11141778509742975a AS cache
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libatomic1 netbase tzdata && rm -rf /var/lib/apt/lists/*
RUN --mount=from=cache,target=/donor cd /donor && cp -a --parents opt usr/local / && hardlink /opt /usr/local
COPY --from=runtime /holon-mcp-runtime /holon-mcp-runtime
ENV HOME=/app XDG_CACHE_HOME=/app/.cache HOLON_RELOAD_DIR=/app/source UV_PYTHON_INSTALL_DIR=.toolchain/python \
    UV_COMPILE_BYTECODE=1 PYTHONDONTWRITEBYTECODE=1 npm_config_cache=/app/.cache/npm \
    npm_config_audit=false npm_config_fund=false npm_config_update_notifier=false
WORKDIR /app
USER 10001:10001
STOPSIGNAL SIGTERM
ENTRYPOINT ["/holon-mcp-runtime"]
