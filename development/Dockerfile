FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS runtime
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY host host
ARG TARGETARCH
RUN CGO_ENABLED=0 GOARCH=$TARGETARCH go build -trimpath -buildvcs=false -ldflags='-s -w' -o /holon-mcp-runtime ./cmd/holon-mcp-runtime

FROM runtime AS fnm
ADD --checksum=sha256:4eaff58b2c5bf30d0934027572dd0b5bbb60d2a1af309230b53662d4b1d45599 https://github.com/Schniz/fnm/releases/download/v1.39.0/fnm-arm64.zip /fnm-arm64.zip
ADD --checksum=sha256:7807664f39d39fc518da1c35ba0181e4b3267603c4b1dedeb4b5fc6ae440a224 https://github.com/Schniz/fnm/releases/download/v1.39.0/fnm-linux.zip /fnm-amd64.zip
ARG TARGETARCH
RUN unzip -q /fnm-$TARGETARCH.zip -d /out && chmod 0755 /out/fnm

# Keep one warm Node and the donor's oldest installed Python as optional resolver caches.
FROM ghcr.io/holon-sh/development@sha256:799756844538e51f867b2ecb0f82bd311a2090a8bf476c11141778509742975a AS cache
USER 0
RUN set -eu; cd /; set -- /opt/uv/python/cpython-*/bin/python3; python=$(realpath "$1"); \
    node=/warm/opt/holon/node/versions/$(node --version); mkdir -p "$node"; ln -s /usr/local "$node/installation"; \
    cp -a --parents "$(dirname "$(dirname "$python")")" \
      /usr/local/bin/node /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/pnpm /usr/local/bin/pnpx \
      /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/pnpm /usr/local/bin/uv /usr/local/bin/uvx /warm; \
    ln -s "$python" "/warm/usr/local/bin/${python##*/}"

FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libatomic1 netbase tzdata && rm -rf /var/lib/apt/lists/*
COPY --from=cache /warm/ /
COPY --from=fnm /out/fnm /usr/local/bin/fnm
COPY --from=runtime /holon-mcp-runtime /holon-mcp-runtime
COPY development/resolve.cjs /usr/local/lib/holon/resolve.cjs
ENV HOME=/app XDG_CACHE_HOME=/app/.cache HOLON_RELOAD_DIR=/app/source \
    FNM_DIR=/app/source/.toolchain/node UV_PYTHON_INSTALL_DIR=/app/source/.toolchain/python \
    UV_COMPILE_BYTECODE=1 PYTHONDONTWRITEBYTECODE=1 npm_config_cache=/app/.cache/npm \
    npm_config_audit=false npm_config_fund=false npm_config_update_notifier=false
ENV PATH=/app/.cache/holon-manager/bin:${HOLON_RELOAD_DIR}/.toolchain/node/bin:${PATH}
WORKDIR /app
USER 10001:10001
STOPSIGNAL SIGTERM
ENTRYPOINT ["/holon-mcp-runtime"]
