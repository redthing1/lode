# Lode

Lode is a lightweight, self-hosted version control system for large binary projects. Work in ordinary folders, save deliberate checkpoints, and choose which historical contents deserve the disk space.

Lode is in early development. The executable currently provides help and version information.

See [the architecture overview](doc/architecture.md) for the core concepts.

## Build

Requires Go as declared in [go.mod](go.mod), or a newer compatible toolchain, and Make.

```sh
make build
./bin/lode --help
./bin/lode version
```

The executable is written to `bin/lode`. Set `VERSION` to label a build:

```sh
make build VERSION=0.1.0-dev
```

## Development

```sh
make check
make fmt
```

`make check` verifies formatting and runs `go vet` and `go test`. `make fmt` formats the Go sources.
