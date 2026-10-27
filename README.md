# Lode

Lode is a self-hosted version-control application for large binary files and folders. A server stores complete versions; clients work with ordinary files and can download only selected paths.

The current implementation supports connected saves, partial checkout, local status, history, and recovery of interrupted saves and checkouts. It is experimental and currently provides one branch, `main`. Updating an existing workspace, merges, retention, collection, and backup commands are not available. All accepted versions currently retain their contents.

## Build

Use Go 1.27.2 or later on Linux. Server and workspace storage must be local filesystems supporting file and directory synchronization.

```sh
make fmt
make check
make build
```

The executable is `bin/lode`. Checks run locally.

## Start a project

```sh
bin/lode init ./store
bin/lode serve ./store
```

Initialization prints the project's access token. Keep that token private. The server listens on `127.0.0.1:8473`; remote access requires a TLS reverse proxy. A project permits one server process at a time.

In another terminal, attach an existing folder to the empty project and save it:

```sh
export LODE_TOKEN='the token printed by init'
export LODE_AUTHOR='Your name'
bin/lode attach http://127.0.0.1:8473 ./work
bin/lode -C ./work status
bin/lode -C ./work save -m 'Initial version'
```

Create a second workspace containing only one directory:

```sh
bin/lode clone -include assets http://127.0.0.1:8473 ./selected
bin/lode -C ./selected log
```

Repeat `-include` to select several files or directories. Omit it to download the whole project. Options precede positional arguments; `-C` precedes the command. Commands operate on the workspace root, either the current directory or the directory specified by `-C`.

Saving from a partial workspace preserves unloaded paths. A save against an outdated branch tip fails. Until workspace updates are available, use a fresh clone to obtain the latest version.

## Recovery and limits

Use `lode resume` in a workspace after an interrupted save or checkout. A save retry resolves the original request and preserves subsequent local edits. If an unaccepted save's intended files have changed, use `lode cancel` before saving again. Cancel leaves local files intact; an already accepted save must be resumed.

An interrupted checkout keeps its plan in `.lode`. Resume verifies files already installed and refuses to overwrite unexpected contents. Move a conflicting path aside before resuming. Checkout creates a new directory; attach requires an existing folder and an empty server project.

Lode versions file contents and directories. Symlinks and special files are unsupported; permissions and timestamps are not versioned. `.lode` is reserved for workspace state and credentials. Other selected files are included. Keep files still while saving: status hashes selected files, and upload validates changed contents again.

Clients store selected baseline metadata and files, without a native storage index or historical payloads. JSON metadata messages are limited to 16 MiB. File transfers stream; initial checkout stages at most one additional file. Server captures currently examine the complete logical tree.

See [architecture](doc/architecture.md) for ownership and persistence contracts.
