# Lode

Lode is a self-hosted version-control application for large binary files and folders. A server stores complete versions; clients work with ordinary files and can download only selected paths.

The current implementation supports connected saves, partial checkout, workspace updates, changing selection, status, history, and recovery. It is experimental and currently provides one branch, `main`. Merges, retention, collection, and backup commands are not available. All accepted versions currently retain their contents.

## Build

Use Go 1.27.2 or later on Linux. Storage must be local filesystems supporting file and directory synchronization; workspace transitions also require atomic rename without replacement.

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

Repeat `-include` to select several files or directories, and use `-exclude` for exceptions. The nearest rule wins. Omit selection rules to download the whole project; exclusions alone select everything except those paths. Options precede positional arguments; `-C` precedes the command. Commands operate on the workspace root, either the current directory or the directory specified by `-C`.

Saving from a partial workspace preserves unloaded paths. A save against an outdated branch tip fails without publishing. Use `update` to obtain the latest version while preserving independent local edits:

```sh
bin/lode -C ./selected update
bin/lode -C ./selected select -include assets -exclude assets/cache
```

Conflicting changes stop an update before files change. Resolve them with `update -ours PATH` to keep local contents or `update -theirs PATH` to take remote contents. Choices apply to whole files or subtrees and may be repeated. Local choices remain changes against the updated baseline.

`select` without options shows the current rules. With rules, it replaces the selection at the current version; `select -none` unloads selected content. Directories needed by unselected local contents remain. Unloading locally modified, deleted, or untracked selected entries fails until those changes are resolved.

## Recovery and limits

Use `lode resume` in a workspace after an interrupted operation. A save retry resolves the original request and preserves subsequent local edits. If an unaccepted save's intended files have changed, use `lode cancel` before saving again. Cancelling a save leaves local files intact; an already accepted save must be resumed. A confirmed stale rejection clears its save intent so an update can proceed.

Checkout, update and selection changes share a recovery plan in `.lode`. They preserve replaced files until completion. Resume finishes the planned transition; cancel rolls back an unfinished transition. If files acquired newer edits, recovery preserves them and reports the conflicting path. Move it aside before retrying. After a transition commits, resume completes any remaining cleanup. Cancelled initial checkout leaves an empty-selection workspace that can be populated with `select`.

Checkout creates a new directory; attach requires an existing folder and an empty server project. Stop applications writing selected files before updating or changing selection. Recovery detects many intervening changes, but does not provide a simultaneous snapshot of other processes' writes.

Lode versions file contents and directories. Symlinks and special files are unsupported; permissions and timestamps are not versioned. `.lode` is reserved for workspace state and credentials. Other selected files are included. Keep files still while saving: status hashes selected files, and upload validates changed contents again.

Clients store selected baseline metadata and files, without a native storage index or historical payloads. JSON metadata messages are limited to 16 MiB. Transfers stream. A transition needs space for preserved replaced files and one incoming file; completed staged files share their installed inode until cleanup. Initial checkout has no replaced files to preserve. Server captures currently examine the complete logical tree.

See [architecture](doc/architecture.md) for ownership and persistence contracts.
