FROM node:24.22.0-bookworm-slim@sha256:91acba91b842bec0bf7ea6f5e09c644c7ccad72c4996ed9616d861d1c7cd60d4
COPY --from=ghcr.io/astral-sh/uv:0.13.1@sha256:4a1f3666816dafadd20ceb71eac9a7d56d7ef30279f7d64048519ab901ada336 /uv /uvx /usr/local/bin/
RUN UV_PYTHON_INSTALL_DIR=/opt/uv/python UV_PYTHON_BIN_DIR=/usr/local/bin uv python install 3.13 3.15 \
 && npm install -g pnpm@10.34.9 && npm cache clean --force && chmod -R a+rX /opt/uv
ARG TARGETARCH
COPY dist/holon-mcp-runtime-linux-${TARGETARCH} /holon-mcp-runtime
ENV HOME=/app XDG_CACHE_HOME=/app/.cache npm_config_cache=/app/.cache/npm UV_CACHE_DIR=/app/.cache/uv \
    UV_PYTHON_INSTALL_DIR=/app/.cache/uv-python HOLON_RELOAD_DIR=/app/source \
    npm_config_audit=false npm_config_fund=false npm_config_update_notifier=false
WORKDIR /app
USER 10001:10001
STOPSIGNAL SIGTERM
ENTRYPOINT ["/holon-mcp-runtime"]
