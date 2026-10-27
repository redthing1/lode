# Lode

Lode is self-hosted version control for large binary files and folders. Save versions, work on branches, download only the paths you need, and release obsolete historical contents. It is experimental and currently supports Linux.

A **project** holds shared history on a server. A **workspace** is an ordinary editable folder containing all or part of that project. Saving from a partial workspace preserves the paths you have not downloaded.

## Build

For a containerized server, follow [Podman Compose setup](doc/operations.md#run-with-podman-compose).

Use Go 1.27.2 or later, cgo, a C compiler, and the development headers/library for [librsync 2.3.4](https://github.com/librsync/librsync/releases/tag/v2.3.4). Every machine running Lode also needs the librsync shared library ([LGPL-2.1](https://github.com/librsync/librsync/blob/v2.3.4/COPYING)).

```sh
make check
make build
```

The executable is `bin/lode`; examples below run from the source checkout. Run `make fmt` after Go edits. Checks run locally.

## Save your first project

Use new `store`, `work`, and `selected` directories for this example. First, create and serve the store:

```sh
bin/lode init ./store
bin/lode serve ./store
```

Initialization prints a private access token. The server listens on `127.0.0.1:8473`. Keep it running and open another terminal in the source checkout:

```sh
export LODE_TOKEN='the token printed by init'
export LODE_AUTHOR='Your name'
mkdir -p ./work/assets
printf 'First version\n' > ./work/assets/example.bin
bin/lode attach http://127.0.0.1:8473 ./work
bin/lode -C ./work status
bin/lode -C ./work save -m 'Initial version'
```

Use your own existing folder in place of `./work` to version real files. `attach` requires an empty server project. Keep application writers stopped while saving or replacing working files.

Create another workspace containing only `assets/`:

```sh
bin/lode clone -include assets http://127.0.0.1:8473 ./selected
bin/lode -C ./selected log
```

Omit `-include` to download everything. Clone destinations must be new directories. Edit files normally, inspect them with `status`, and save with `save -m MESSAGE`. If another workspace has advanced the branch, run `update` before saving again.

Workspace commands use the current directory or `-C DIR` as the workspace root. Put `-C` before the command and command options before positional arguments. Use `bin/lode help COMMAND` for syntax.

## Documentation

- [User guide](doc/guide.md): selection, branches, conflicts, restore, cleanup, and recovery.
- [Operations](doc/operations.md): HTTPS hosting, credentials, backup, and supported limits.
- [Architecture](doc/architecture.md): component ownership, persistence, and recovery contracts.
