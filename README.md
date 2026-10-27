# Lode

Lode is a self-hosted version-control application for large binary files and folders. A server stores complete versions; clients work with ordinary files and can download only selected paths.

The current implementation supports connected saves, branches, whole-file merges, partial checkout, workspace updates, historical restore, retention tags, cleanup, backup, and recovery. It is experimental.

## Build

Use Go 1.27.2 or later on Linux, with cgo enabled, a C compiler, and the development headers/library for [librsync 2.3.4](https://github.com/librsync/librsync/releases/tag/v2.3.4). The executable links to the system librsync shared library, which must also be installed on machines running Lode. Librsync is distributed under [LGPL-2.1](https://github.com/librsync/librsync/blob/v2.3.4/COPYING).

Storage must be local filesystems supporting file and directory synchronization; workspace transitions also require atomic rename without replacement.

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

Repeat `-include` to select several files or directories, and use `-exclude` for exceptions. The nearest rule wins. Omit selection rules to download the whole project; exclusions alone select everything except those paths. Options precede positional arguments; `-C` precedes the command. Commands operate on the workspace root, either the current directory or the directory specified by `-C`. Use `lode help COMMAND` for arguments and options; `status` shows the branch, base and selection before local changes.

Saving from a partial workspace preserves unloaded paths. A save against an outdated branch tip fails without publishing. Use `update` to obtain the latest version while preserving independent local edits:

```sh
bin/lode -C ./selected update
bin/lode -C ./selected select -include assets -exclude assets/cache
```

Conflicting changes stop an update before files change. Resolve them with `update -ours PATH` to keep local contents or `update -theirs PATH` to take remote contents. Choices apply to whole files or subtrees and may be repeated. Local choices remain changes against the updated baseline.

`select` without options shows the current rules. With rules, it replaces the selection at the current version; `select -none` unloads selected content. Rules that match no remote or local entries cannot empty a populated workspace; check the paths or use `-none` explicitly. Directories needed by unselected local contents remain. Unloading locally modified, deleted, or untracked selected entries fails until those changes are resolved.

## Branches and restore

Create a branch at the workspace's current version, then switch to it:

```sh
bin/lode -C ./work branch experiment
bin/lode -C ./work switch experiment
# Edit files, then save.
bin/lode -C ./work save -m 'Try a different layout'
bin/lode -C ./work switch main
bin/lode -C ./work merge experiment
bin/lode -C ./work update
```

`switch` preserves independent local edits and accepts the same whole-entry choices as `update`. `branch` lists branches; `branch NAME COMMIT` chooses a starting version. `branch -delete NAME` deletes a reference, retaining its versions. The current workspace branch and `main` cannot be deleted through this command.

`merge` combines saved versions on the server, including unloaded paths. It leaves local files and the workspace baseline unchanged; run `update` to materialize the result. The destination branch must still be at the workspace base. Unsaved local edits are excluded from the merge and remain available for the subsequent update.

Conflicts require `merge -ours PATH BRANCH` or `merge -theirs PATH BRANCH`, choosing the destination or source version respectively. A directory choice replaces that entire subtree, including independent changes within it; use individual file paths for narrower choices. Merges record both parents. An already merged source creates no version. If history has multiple best common ancestors, choose one of the reported versions with `-base COMMIT`. `log` follows first-parent history.

`restore COMMIT [PATH...]` places historical contents into currently selected paths as local changes, keeping the branch and baseline. Omit paths to restore the whole selection. An explicit path must exist in the historical version, local files or current baseline; a misspelling fails before any files change. It refuses to overwrite local changes unless `-force` is supplied. Save the restored changes to publish a new version.

## Storage lifetime

Tags protect important versions from content expiry. `tag NAME [COMMIT]` uses the workspace base when no version is supplied; `tag` lists tags and `tag -delete NAME` removes one. A tag cannot silently move to a different version.

Preview cleanup before applying it:

```sh
bin/lode -C ./work tag release-1
bin/lode -C ./work cleanup -keep 20
bin/lode -C ./work cleanup -keep 20 -apply PREVIEW_TOKEN
```

`-keep N` protects the latest N first-parent versions of every branch, counting each tip. Branch tips and tagged versions are always protected, including with `-keep 0`. The preview lists version IDs, timestamps, availability and protection counts; `branch` and `tag` show the named references. It streams the complete list and prints an apply token only after the preview finishes successfully. Apply rejects changes to history or references since that preview. Repeating an applied token is safe while history and references still match.

Expiry removes the promise of downloadable contents; history and comparison metadata remain, and `log` marks expired versions. Increasing `-keep` later cannot restore expired contents. Expired versions cannot become new branch or tag targets. Local files and baseline facts remain usable after their server base expires, and `update` can preserve local work while advancing to a retained tip.

Applying cleanup records expiry, then requests native collection. `collect` requests collection again without changing retention. Collection reports physical repository blob bytes before and after, excluding caches. Native safety delays can require several runs over hours or days; packed storage overhead can remain. Allow free space for native caches and temporary rewritten packs when saving or collecting. Collection runs explicitly and temporarily blocks other project operations. Logical history metadata is not pruned.

An active file download stays protected until its stream finishes. A version can expire between downloads in a local transition; resume then reports unavailable contents, and cancel restores the preserved local files. Idle workspaces and unfinished local operations do not indefinitely retain server contents.

## Back up and restore a project

Stop the server, then copy the complete project into a new directory:

```sh
bin/lode backup ./store ./project-backup
```

The copy contains history, branches, tags, retention decisions, operation outcomes and retained contents. It also preserves project identity and credentials; keep it private. It needs space for the complete database and native repository, including obsolete bytes not yet collected. Native caches and connection configuration are rebuilt when opened.

To restore, make a working copy and serve it:

```sh
bin/lode backup ./project-backup ./restored-store
bin/lode serve ./restored-store
```

The source must be stopped and writable, and the destination must not exist or lie inside the source. Backup holds the project ownership lock and closes its database and content writers before copying. It checks the copied database and retained snapshot roots before publishing the destination name; this is not a full payload integrity scan. Copies are independent stores, not synchronized replicas.

An abrupt interruption may leave an unfinished sibling directory named `.DEST.partial`. The final destination appears only after copying and verification; remove the unfinished directory before retrying. An existing destination is never replaced.

## Recovery and limits

Long commands report elapsed activity on an interactive terminal. Ctrl-C interrupts scanning and transfers; an interrupted command exits with status 130. Unfinished operations remain recoverable, and interruption does not automatically roll back local changes.

Use `lode resume` in a workspace after an interrupted operation. A save retry resolves the original request and preserves subsequent local edits. If an unaccepted save's intended files have changed, use `lode cancel` before saving again. Cancelling a save leaves local files intact; an already accepted save must be resumed. A confirmed stale rejection clears its save intent so an update can proceed.

Checkout, update, switch, selection changes and restore share a recovery plan in `.lode`. They preserve replaced files until completion. Resume finishes the planned transition; cancel rolls back an unfinished transition. If files acquired newer edits, recovery preserves them and reports the conflicting path. Move it aside before retrying. After a transition commits, resume completes any remaining cleanup. Cancelled initial checkout leaves an empty-selection workspace that can be populated with `select`. An interrupted merge resumes its original saved source and destination versions; update remains a separate local operation.

Checkout creates a new directory; attach requires an existing folder and an empty server project. Stop applications writing affected files before any local transition. Recovery detects many intervening changes, but does not provide a simultaneous snapshot of other processes' writes.

Lode versions file contents and directories. Paths must be UTF-8 without control characters. Symlinks and special files are unsupported; permissions and timestamps are not versioned. `.lode` is reserved for workspace state and credentials. Other selected files are included. Keep files still while saving: status hashes selected files, and upload validates changed contents again.

Clients store selected baseline metadata and files, without a native storage index or historical payloads. JSON requests and individual metadata records are limited to 16 MiB; metadata lists stream without that total-response limit. File transfers stream. Changed files of at least 1 MiB can upload as librsync deltas against their retained base. Updates, switches and restores can similarly reuse an existing local file during download, even if its server version has expired. Files without a suitable basis transfer in full, including initial checkout and newly selected paths with no local file.

A basis above about 28 GiB uses full transfer to bound client memory. Upload signatures use at most 64 MiB of temporary client disk space per save; remaining files upload in full. A download holds at most one 1 MiB signature at a time. Delta transfer still reads complete source files, and full rewrites may save no bandwidth. Updates can reread local files several times for observation, matching and recovery checks. Downloaded files are reconstructed into temporary files and verified before installation.

A transition needs space for preserved replaced files and one incoming file; completed staged files share their installed inode until cleanup. Initial checkout has no replaced files to preserve. Server captures currently examine the complete logical tree.

See [architecture](doc/architecture.md) for ownership and persistence contracts.
