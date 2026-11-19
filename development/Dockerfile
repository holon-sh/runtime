FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
ARG TARGETARCH
ADD --checksum=sha256:4eaff58b2c5bf30d0934027572dd0b5bbb60d2a1af309230b53662d4b1d45599 https://github.com/Schniz/fnm/releases/download/v1.39.0/fnm-arm64.zip /fnm-arm64.zip
ADD --checksum=sha256:7807664f39d39fc518da1c35ba0181e4b3267603c4b1dedeb4b5fc6ae440a224 https://github.com/Schniz/fnm/releases/download/v1.39.0/fnm-linux.zip /fnm-amd64.zip
RUN unzip -q /fnm-$TARGETARCH.zip -d /out && chmod 0755 /out/fnm
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY host host
RUN CGO_ENABLED=0 GOARCH=$TARGETARCH go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/holon-mcp-runtime ./cmd/holon-mcp-runtime

FROM node:24.22.0-bookworm-slim@sha256:91acba91b842bec0bf7ea6f5e09c644c7ccad72c4996ed9616d861d1c7cd60d4
RUN apt-get update && apt-get install -y ca-certificates libatomic1 netbase \
 && rm -rf /var/lib/apt/lists/* /var/log/apt /var/log/dpkg.log /var/cache/ldconfig/aux-cache
COPY --from=ghcr.io/astral-sh/uv:0.13.1@sha256:4a1f3666816dafadd20ceb71eac9a7d56d7ef30279f7d64048519ab901ada336 /uv /uvx /usr/local/bin/
COPY --from=build /out/fnm /usr/local/bin/
RUN UV_PYTHON_INSTALL_DIR=/opt/uv/python UV_PYTHON_BIN_DIR=/usr/local/bin uv python install 3.13 3.15 \
 && npm install -g pnpm@10.34.9 && rm -rf /root/.npm /tmp/node-compile-cache && chmod -R a+rX /opt/uv
RUN node=/opt/holon/seed/.toolchain/node && version=node-versions/$(node --version)/installation \
 && mkdir -p $node/$version $node/aliases && ln -s /usr/local/bin $node/$version/bin && ln -s ../$version $node/aliases/default
ENV HOME=/app HOLON_RELOAD_DIR=/app/source HOLON_SOURCE_SEED=/opt/holon/seed \
    FNM_DIR=/app/source/.toolchain/node FNM_MULTISHELL_PATH=/app/source/.toolchain/node/current FNM_VERSION_FILE_STRATEGY=recursive \
    UV_PYTHON_INSTALL_DIR=/app/source/.toolchain/python UV_COMPILE_BYTECODE=1 PYTHONDONTWRITEBYTECODE=1 \
    npm_config_cache=/app/.cache/npm npm_config_audit=false npm_config_fund=false npm_config_update_notifier=false \
    PATH=/app/source/.toolchain/node/current/bin:$PATH
RUN --mount=type=tmpfs,target=/app cp -a $HOLON_SOURCE_SEED $HOLON_RELOAD_DIR && cd $HOLON_RELOAD_DIR && fnm use --install-if-missing \
 && test "$(command -v node)" = $FNM_MULTISHELL_PATH/bin/node
WORKDIR /app
COPY --from=build /out/holon-mcp-runtime /
USER 10001:10001
ENTRYPOINT ["/holon-mcp-runtime"]
