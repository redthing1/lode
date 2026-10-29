# Lode

Lode versions large binary projects in ordinary folders. Work on a shared branch, download only the paths you need, and browse files and history in the web interface.

Lode is experimental. The client targets Linux, macOS, and Windows; the server runs on Linux.

## Install

From a source checkout, install Go 1.26 or later, a C compiler, and librsync. See [build prerequisites](doc/build.md) for platform setup.

Install the command, then add Go's binary installation directory to your `PATH` (`go env GOBIN`, or `bin` under `go env GOPATH` when unset):

```sh
go install -mod=readonly ./cmd/lode
lode --help
```

## Try it locally

On Linux, create and start a server:

```sh
lode init ./instance
lode serve ./instance
```

Open `http://localhost:8473` and use the printed setup code to create an administrator. Create a project named `example`, then a write token under **Settings → Access tokens**.

In another terminal, connect an existing folder. Replace `alice` with your username and `./work` with the folder you want to version:

```sh
lode login http://localhost:8473
lode attach http://localhost:8473/alice/example ./work
cd ./work
lode save -m 'Initial version'
```

## Documentation

- [User guide](doc/guide.md) — daily work, selected files, sharing, and recovery.
- [Server operations](doc/operations.md) — Podman Compose, HTTPS, accounts, and backups.
- [Architecture](doc/architecture.md) — responsibilities, storage, and recovery design.
- [Agent skill](skills/lode/SKILL.md) — working with Lode through the CLI.

To install the skill for Codex from a checkout:

```sh
mkdir -p "${CODEX_HOME:-$HOME/.codex}/skills"
cp -R skills/lode "${CODEX_HOME:-$HOME/.codex}/skills/"
```

## Development

Run checks locally:

```sh
make fmt
make check
make build
```
