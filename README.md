# runtime

## Building

```sh
scripts/build-runtime
```

cross-builds `cmd/holon-mcp-runtime` for Linux into `dist/`. Each release publishes the same files as its assets:
`holon-mcp-runtime-linux-amd64`, `holon-mcp-runtime-linux-arm64` and `SHA256SUMS`.

The JavaScript adapter is bundled from `host/javascript/adapter.ts` and committed; `npm ci --ignore-scripts && npm run build`
there regenerates `adapter.bundle.mjs` byte for byte. `npm test` there builds the runtime and runs the host contract under
Node; set `HOLON_TEST_RUNTIME` to `bun`, `deno` or a Python interpreter with `host/python`'s dependencies (`uv sync --frozen`)
to run it under another runtime.

## Reloading from a source overlay

With `HOLON_RELOAD_SECRET` set, `--transport=hosted-http` serves `POST /.holon/reload` on the public listener, ahead of platform
admission, so that `holon dev` can restart the application in place. The request has `Authorization: Bearer <secret>` and the
body `{"registry", "repository", "token", "digest", "mediaType"}`, all required: `registry` is the registry's `http` or `https`
URL and `mediaType` is `application/vnd.holon.source-overlay.v0.tar+zstd`. The runtime fetches
`<registry>/v2/<repository>/blobs/<digest>`, unpacks the verified zstd tar into a fresh directory under `HOLON_RELOAD_DIR`
(default `/tmp/holon-source`), restarts the child there and answers `{"restartedAt", "readyAt", "digest"}` once it has posted
ready. Requests meanwhile get 503. The overlay is not durable: a restarted container boots from its image.

| Status | Meaning |
|---|---|
| 400 | malformed request |
| 401 | wrong secret |
| 409 | a reload is in progress |
| 500 | could not unpack, or the new child failed (`exit` holds its status) and the previous source is running again |
| 502 | the registry refused or failed, or the blob does not match its digest |
| 504 | the new child was not ready 45 s after the request and the previous source is running again |

Top-level entries of the application directory that the overlay lacks, such as `node_modules` or a built `dist`, are linked into
the new directory. Without the secret the path behaves like any other.

## License

Apache License 2.0, see `LICENSE` and `NOTICE`.
