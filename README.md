# Lode

Lode is self-hosted version control for large binary projects. Its embedded web interface manages accounts, projects, permissions, history, and storage. The CLI works with ordinary editable workspaces and can load only selected paths. Saves preserve project paths that are not present locally.

Lode is experimental. The server and currently supported CLI build run on Linux.

## Install

Use Go 1.27.2 or later, Git, a C compiler, and the development headers/library for [librsync 2.3.4](https://github.com/librsync/librsync/releases/tag/v2.3.4). Go must have cgo enabled. The installed command also needs the librsync shared library ([LGPL-2.1](https://github.com/librsync/librsync/blob/v2.3.4/COPYING)).

Install or update from Git using your configured Git credentials:

```sh
GOBIN="$HOME/.local/bin" GOPRIVATE=github.com/redthing1/lode \
  go install github.com/redthing1/lode/cmd/lode@main
```

Add `$HOME/.local/bin` to `PATH` in your shell configuration once:

```sh
export PATH="$HOME/.local/bin:$PATH"
lode --version
```

From a source checkout, `GOBIN="$HOME/.local/bin" make install` installs the local version there. For container hosting, see [Podman Compose](doc/operations.md#run-with-podman-compose).

## Start an instance

Create a private instance directory and its first administrator setup code:

```sh
lode init -url http://localhost:8473 ./instance
lode serve ./instance
```

Open the setup URL and enter the code printed by `init`. Choose an administrator username and a password of at least 12 characters. The administrator creates projects and accounts in the web interface; each user can create personal access tokens in account settings. Use an HTTPS origin with `-url` when serving beyond loopback; see [operations](doc/operations.md#host-an-instance).

## Connect a workspace

Create a project named `example` in the web interface, then open **Settings → Access tokens** from the account menu to create a token. From the CLI, log in to the project URL and connect a folder:

```sh
lode login http://localhost:8473/p/example
mkdir -p ./work
lode attach http://localhost:8473/p/example ./work
cd ./work
printf 'First version\n' > example.bin
lode save -m 'Initial version'
```

The token prompt is hidden in a terminal. Credentials are saved in your OS user configuration with private file permissions, not in the workspace. `LODE_TOKEN` overrides saved credentials. See the [user guide](doc/guide.md) for project roles, selected paths, branches, recovery, and credential handling.

## Documentation

- [User guide](doc/guide.md): workspaces, access, branches, history, and recovery.
- [Operations](doc/operations.md): hosting, accounts, backup, import, and storage limits.
- [Architecture](doc/architecture.md): service boundaries and persistence contracts.

## Development

Run `make fmt`, `make check`, and `make build` from the source checkout. Checks run locally; `make build` produces a development binary in `bin/`.
