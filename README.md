# runtime

## Building

Run `go test -race ./...` to test the supervisor. Build the development image with Docker Buildx.

```sh
docker buildx build --platform linux/amd64,linux/arm64 -f development/Dockerfile .
```

## Community

Ask questions and share what you're building on the [Holon Discord](https://discord.gg/jXExyBdy).

## License

Apache License 2.0, see `LICENSE` and `NOTICE`.
