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

## Community

Ask questions and share what you're building on the [Holon Discord](https://holon-labs.dev/discord/).

## License

Apache License 2.0, see `LICENSE` and `NOTICE`.
