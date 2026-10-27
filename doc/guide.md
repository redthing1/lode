# User guide

Follow the [quickstart](../README.md#save-your-first-project) to create a workspace. Examples start at its root. Workspace commands also work from subdirectories; use `lode -C DIR` to work from elsewhere. Workspace paths are relative to the root; attach/clone destinations are relative to the current directory.

## Save and update

A saved version, or **commit**, describes the complete project. Your workspace remembers a **base** version for comparing local changes. A **branch** names a line of saved work; new projects start on `main`.

```sh
lode status
lode save -m 'Update the assets'
lode log
lode update
```

`status` shows the branch, base, selection, and added/modified/deleted paths. `save` publishes selected changes and prints the new version ID. A save with no changes reports that there is nothing to save. `log` follows first-parent history—the destination branch's line through merges; `log -n 50` shows more versions.

Saving requires a connection. Local editing and `status` work offline. If another workspace advances the branch, a stale save is rejected; `update` brings in the current version while preserving independent local edits, then you can save again.

Conflicts stop an update before files change. For example, keep your local file:

```sh
lode update -ours assets/example.bin
```

Use one choice for each conflicting path; options are repeatable. `-ours` keeps your local contents as changes against the updated base. To take the server version instead, use `update -theirs PATH`. A directory choice chooses the entire subtree, including independent changes within it.

## Choose what is local

Selection controls local presence. Unloaded paths remain part of the shared project and survive partial saves.

```sh
lode clone -include assets -exclude assets/cache https://lode.example.com ../selected
lode -C ../selected select
lode -C ../selected select -include assets -include references
```

Set `LODE_TOKEN` before attaching or cloning. Both commands accept repeatable include/exclude options; the most specific path rule wins. Without rules, they select everything. Exclusions alone select everything except those paths. Use `select -include .` to select the whole project.

`select` without options shows the rules. With options, it replaces them at the current base. Clean files leaving the selection are removed locally; modified, deleted, or untracked selected entries must be resolved first. Unselected local contents and directories needed to contain them remain.

A rule set matching no remote or local entries cannot empty a populated workspace. Check the paths, or use `select -none` to unload all selected content explicitly. The workspace remains attached and can load paths again later.

## Branch and merge

Create a branch at your workspace base, work on it, then merge its saved versions into `main`:

```sh
lode branch experiment
lode switch experiment
# Edit files, then save.
lode save -m 'Try a different layout'
lode switch main
lode merge experiment
lode update
```

`branch` lists branches; `branch NAME COMMIT` chooses a starting version. `branch -delete NAME` removes the name while retaining its history. The CLI refuses deletion of `main` or the current workspace branch. `switch` preserves independent local edits and accepts the same choices as `update`.

`merge` compares complete saved trees on the server, including unloaded paths. It excludes unsaved local edits and leaves workspace files and the base unchanged; run `update` to bring in the result. The destination branch must still match the workspace base.

For a conflict, use `merge -ours PATH BRANCH` to choose the destination version or `merge -theirs PATH BRANCH` to choose the source. Choices select whole files or subtrees. An already merged source creates no version. If several best common ancestors exist, choose a reported ID with `-base COMMIT`.

## Restore and protect a version

`COMMIT` means the full version ID printed by `save`, `log`, or `tag`, not a branch or tag name.

```sh
lode restore COMMIT assets/example.bin
lode status
lode save -m 'Restore the earlier file'
```

Restore puts historical contents into selected paths as local changes. It keeps the branch and base. Omit paths to restore the whole selection. Every explicit path must be known to the historical version, local files, or current baseline; a typo fails before any files change. A file absent from the historical version is removed locally.

Restore refuses to discard local changes unless you pass `-force`. Only retained versions have downloadable contents.

A **tag** protects a saved version from expiry. `tag release-1` protects the workspace base; `tag release-1 COMMIT` names another retained version. `tag` lists names and IDs, and `tag -delete release-1` removes protection. An existing tag cannot silently move to another version.

## Release old contents

```sh
lode tag release-1
lode cleanup -keep 20
# Inspect the preview, then copy its token:
lode cleanup -keep 20 -apply PREVIEW_TOKEN
```

`-keep N` protects the latest N first-parent versions of every branch, counting its tip. Tips and tagged versions are always protected, including with `-keep 0`. The preview explains which versions are kept, eligible, or already expired. Its apply token appears only after the complete preview succeeds.

Apply rechecks history and references; if they changed, preview again. Repeating the same applied token is safe while those facts still match.

Expiry removes downloadable contents while preserving history and comparison metadata. `log` marks expired versions. Increasing `-keep` cannot bring their bytes back; expired versions cannot become new branch/tag targets. Local files survive expiry of their server base and can update toward a retained version.

Apply also requests physical collection. `collect` requests it again without expiring more versions. Native safety delays can require later runs over hours or days, and packing overhead can remain. See [storage limits](operations.md#storage-and-transfer-limits).

## Recover interrupted work

Long workspace commands report elapsed activity on a terminal. Ctrl-C interrupts them with exit status 130; it does not automatically roll back changes.

- `resume` finishes an interrupted save or file transition. A save retry resolves the original outcome and preserves subsequent local edits.
- `cancel` cancels an unaccepted save without deleting local files, or rolls back an unfinished file transition. An accepted save must be resumed.
- If an unaccepted save's intended files changed, cancel it before saving again. After a confirmed stale rejection, update and retry.
- If recovery finds unexpected edits, it preserves them and reports the path. Move those contents aside before retrying. After a transition commits, resume finishes cleanup.

A cancelled initial checkout leaves an attached empty-selection workspace; populate it with `select`. A historical version can expire between downloads; the transition then stops and can be cancelled. An interrupted merge resumes its original saved versions; update is still a separate step.

For more command options, use `lode help COMMAND`. Hosting and complete-store recovery are covered in [operations](operations.md).
