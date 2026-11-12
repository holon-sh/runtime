# runtime

`holon-mcp-runtime` supervises a development app, so that `holon dev` reaches the app with a save in about a second
rather than a new image and pod. It is the entrypoint of `ghcr.io/holon-sh/development`, the one image every
development app runs on, and it runs nowhere else: a deployed app serves itself through its SDK.

```sh
holon-mcp-runtime COMMAND [ARG...]
```

- It serves `PORT` and proxies it to `COMMAND`, which it starts in `HOLON_RELOAD_DIR` with a loopback `HOST` and a
  `PORT` of the app's own. Until the app accepts connections, a request is answered 503 with `Retry-After`.
- `POST /.holon/reload` with a source overlay (`application/vnd.holon.source-overlay.v0.tar+zstd`, a zstd tar of the
  app's source) stops the app and its process group, writes the source over the directory, removes what the previous
  overlay wrote and this one does not, and starts `COMMAND` again. It answers 204 once the app accepts connections, or
  the error and the app's exit status; an app that does not start stays down until the next reload.
- At boot it starts the overlay the platform recorded in the file `HOLON_OVERLAY_FILE` names, after checking its
  digest. Until the registry holds it, the app waits for the first reload.

`COMMAND` installs the app's dependencies, builds its frontend and starts it, so the app accepts connections only once
everything it serves is ready; `holon dev` writes it. Only holon-api can reach the reload route: Holon's mesh admits
it to development apps alone.

## Building

```sh
scripts/build-runtime
```

cross-builds `cmd/holon-mcp-runtime` for Linux into `dist/`. Each release publishes those files as its assets
(`holon-mcp-runtime-linux-amd64`, `holon-mcp-runtime-linux-arm64` and `SHA256SUMS`) and pushes the development image,
built from `development/Dockerfile`: the runtime, Node 24, pnpm, and uv with Python 3.13 and 3.15.

## Community

Ask questions and share what you're building on the [Holon Discord](https://discord.gg/jXExyBdy).

## License

Apache License 2.0, see `LICENSE` and `NOTICE`.
