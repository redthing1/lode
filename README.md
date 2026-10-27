# Lode

Lode is self-hosted version control for large binary files and folders. Save versions, work on branches, download only the paths you need, and release obsolete historical contents. It is experimental and currently supports Linux.

A **project** holds shared history on a server. A **workspace** is an ordinary editable folder containing all or part of that project. Saving from a partial workspace preserves the paths you have not downloaded.

## Install

Use Go 1.27.2 or later, Git, a C compiler, and the development headers/library for [librsync 2.3.4](https://github.com/librsync/librsync/releases/tag/v2.3.4). Go must have cgo enabled. The installed command also needs the librsync shared library ([LGPL-2.1](https://github.com/librsync/librsync/blob/v2.3.4/COPYING)).

Install or update from Git using your configured Git credentials:

```sh
GOPRIVATE=github.com/redthing1/lode go install github.com/redthing1/lode/cmd/lode@main
```

Go installs to `GOBIN`, or `$(go env GOPATH)/bin` when unset. Add that directory to your shell's `PATH` once. For the default location:

```sh
export PATH="$(go env GOPATH)/bin:$PATH"
lode --version
```

From an existing source checkout, `make install` installs the local version using the same Go settings. For containerized hosting, follow [Podman Compose setup](doc/operations.md#run-with-podman-compose).

## Save your first project

Use new `store`, `work`, and `selected` directories for this example. First, create and serve the store:

```sh
lode init ./store
lode serve ./store
```

Initialization prints a private access token. The server listens on `127.0.0.1:8473`. Keep it running and open another terminal in the same directory:

```sh
export LODE_TOKEN='the token printed by init'
export LODE_AUTHOR='Your name'
mkdir -p ./work/assets
printf 'First version\n' > ./work/assets/example.bin
lode attach http://127.0.0.1:8473 ./work
cd ./work
lode status
lode save -m 'Initial version'
```

Use your own existing folder in place of `./work` to version real files. `attach` requires an empty server project. Keep application writers stopped while saving or replacing working files.

Create another workspace containing only `assets/`:

```sh
cd ..
lode clone -include assets http://127.0.0.1:8473 ./selected
cd ./selected
lode log
```

Omit `-include` to download everything. Clone destinations must be new directories. Edit files normally, inspect them with `status`, and save with `save -m MESSAGE`. If another workspace has advanced the branch, run `update` before saving again.

Workspace commands find the nearest workspace from the current directory or `-C DIR`. Workspace paths are relative to its root; attach/clone destinations are relative to the current directory. Put `-C` before the command and command options before positional arguments. Use `lode help COMMAND` for syntax.

## Documentation

- [User guide](doc/guide.md): selection, branches, conflicts, restore, cleanup, and recovery.
- [Operations](doc/operations.md): HTTPS hosting, credentials, backup, and supported limits.
- [Architecture](doc/architecture.md): component ownership, persistence, and recovery contracts.

## Development

Run `make fmt`, `make check`, and `make build` from the source checkout. Checks run locally; `make build` produces a development binary in `bin/`.
