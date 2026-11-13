# runtime

The development image keeps a stable HTTP endpoint while an app reloads from source. Production apps serve directly
through their SDK and do not run this supervisor.

For each source overlay the supervisor runs `.holon/prepare` to install dependencies, then `.holon/start` and an
optional `.holon/build` concurrently. Only builds independent of server startup belong in `.holon/build`; server
prerequisites remain in `.holon/prepare`. It forwards traffic and acknowledges the reload only after the app listens and
the build succeeds. Preparation and build output go to the reload reply; app output goes to its log. A failed reload
leaves the supervisor reachable for the next correction. Every command's process group is retired when its leader
exits or the command is stopped, and dependency files survive ordinary source reloads.

The supervisor sets `HOLON_CONCURRENT_BUILD=1` for preparation. New prepare scripts run `.holon/build` synchronously
when that variable is absent, so they also work with older development images. Existing overlays without a separate
build script continue to work.

## Building

```sh
docker buildx build --platform linux/amd64,linux/arm64 -f development/Dockerfile .
```

The Dockerfile compiles the supervisor and includes Node, pnpm, uv, and Python. Releases publish the development image.

## Community

Ask questions and share what you're building on the [Holon Discord](https://discord.gg/jXExyBdy).

## License

Apache License 2.0, see `LICENSE` and `NOTICE`.
