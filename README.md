# runtime

## Building

Run `go test ./...` to test the supervisor. To build the release image for Linux amd64 and arm64, install Docker Buildx, ORAS, and Python. Run the command from the repository root.

```sh
python3 development/package.py out/development
```

The command writes an OCI layout at `out/development/release` with the tag `release`. It contains the source worker and the production bases the CLI selects when assembling an app.

## Community

Ask questions and share what you're building on the [Holon Discord](https://discord.gg/jXExyBdy).

## License

Apache License 2.0, see `LICENSE` and `NOTICE`.
