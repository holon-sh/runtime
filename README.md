# runtime

The development image keeps a stable HTTP endpoint while an app reloads from source. Production apps serve directly
through their SDK and do not run this supervisor.

For each source overlay the supervisor runs `.holon/prepare` to install dependencies and build the app, then
`.holon/start` to serve it. It forwards traffic and acknowledges the reload after the app listens. Preparation output
goes to the reload reply; app output goes to its log. A failed reload leaves the supervisor reachable for the next
correction. The app's process group is retired when its leader exits, and dependency files survive ordinary source
reloads.

## Building

```sh
docker buildx build --platform linux/amd64,linux/arm64 -f development/Dockerfile .
```

The Dockerfile compiles the supervisor and includes Node, pnpm, uv, and Python. Releases publish the development image.

## Community

Ask questions and share what you're building on the [Holon Discord](https://discord.gg/jXExyBdy).

## License

Apache License 2.0, see `LICENSE` and `NOTICE`.
