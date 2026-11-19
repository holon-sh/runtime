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

FROM node:24.22.0-bookworm-slim@sha256:91acba91b842bec0bf7ea6f5e09c644c7ccad72c4996ed9616d861d1c7cd60d4
RUN apt-get update && apt-get install -y ca-certificates libatomic1 netbase && rm -rf /var/lib/apt/lists/*
COPY --from=ghcr.io/astral-sh/uv:0.13.1@sha256:4a1f3666816dafadd20ceb71eac9a7d56d7ef30279f7d64048519ab901ada336 /uv /uvx /usr/local/bin/
ENV UV_COMPILE_BYTECODE=1 PYTHONDONTWRITEBYTECODE=1
RUN UV_PYTHON_INSTALL_DIR=/opt/uv/python UV_PYTHON_BIN_DIR=/usr/local/bin uv python install 3.13 3.15 \
 && npm install -g pnpm@10.34.9 && npm cache clean --force && chmod -R a+rX /opt/uv
COPY --from=fnm /out/fnm /usr/local/bin/fnm
ENV HOLON_SOURCE_SEED=/opt/holon/seed FNM_VERSION_FILE_STRATEGY=recursive \
    FNM_DIR=/app/source/.toolchain/node FNM_MULTISHELL_PATH=/app/source/.toolchain/node/current
RUN version=/opt/fnm/node-versions/$(node --version) && seed=$HOLON_SOURCE_SEED/.toolchain/node \
 && mkdir -p "$version/installation" /opt/fnm/aliases "$seed/node-versions" "$seed/aliases" \
 && ln -s /usr/local/bin "$version/installation/bin" && ln -s "$version/installation" /opt/fnm/aliases/default \
 && ln -s "$version" "$seed/node-versions/" && ln -s /opt/fnm/aliases/default "$seed/aliases/"
ENV PATH="${FNM_MULTISHELL_PATH}/bin:${PATH}"
COPY --from=runtime /holon-mcp-runtime /holon-mcp-runtime
ENV HOME=/app npm_config_cache=/app/.cache/npm \
    UV_PYTHON_INSTALL_DIR=/app/source/.toolchain/python HOLON_RELOAD_DIR=/app/source \
    npm_config_audit=false npm_config_fund=false npm_config_update_notifier=false
WORKDIR /app
USER 10001:10001
STOPSIGNAL SIGTERM
ENTRYPOINT ["/holon-mcp-runtime"]
