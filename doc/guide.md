# User guide

Follow the [quickstart](../README.md#start-an-instance) to create a project and connect a workspace. Examples start at its root. Workspace commands also work from subdirectories; use `lode -C DIR` to work from elsewhere. Workspace paths are relative to the root; attach/clone destinations are relative to the current directory.

## Accounts and project access

Create a private project from **Projects → New project**. Its URL is `/<username>/<project>`, such as `https://lode.example.com/alice/assets`. Usernames use 3–32 lowercase letters, digits, underscores or hyphens; passwords require at least 12 characters. Administrator-created accounts must change their temporary password before using projects. Public registration is closed by default.

Under **Project settings → Members**, add existing accounts as readers, writers or maintainers. Readers browse and download. Writers also save versions and manage branches. Maintainers also manage members and retention. Owners have maintainer access automatically; instance administrators can access every project. Tokens cannot exceed their owner's current project permissions or administer the instance.

Owners and instance administrators can delete a project under its settings by confirming the full `username/project` name. Deletion removes all versions and contents and is refused while the project has active requests.

Open **Settings → Access tokens** from the account menu. Choose one project or all projects you can access, a maximum role, and an expiry of 30, 90, or 365 days. Copy the token when it is created; it is shown only once. Revoke it from the same page when it is no longer needed.

Log in to a project URL to save a project-specific credential:

```sh
lode login https://lode.example.com/alice/example
lode attach https://lode.example.com/alice/example ./work
```

The prompt hides the token in a terminal. Lode verifies the account and project before saving it in your OS user configuration, with file permissions restricted to your account. Credentials are not stored in workspace state. A project-specific credential takes precedence; a credential saved for the instance origin is used as a fallback for its projects. `LODE_TOKEN` overrides either saved credential for the current command.

`lode logout URL` removes the saved local credential for that URL. It does not revoke the server token; revoke tokens in the web interface. An administrator can reset another account from `/-/admin/users`; the temporary password is shown once, sessions and tokens are invalidated, security keys and recovery codes are removed, and the account must change its password at next sign-in. The reset preserves the account role, enabled state, and project data. A local operator can do the same with `lode recover INSTANCE USERNAME` while the server is stopped.

## Browser sign-in and files

Add passkeys under **Settings → Security**. The default mode lets you sign in without a username. Choose **Enter a username** for a security key that should avoid storing a discoverable credential. At sign-in, use **Sign in with a passkey**; enter your username first for a non-discoverable key. Both passwordless modes require the authenticator's PIN or biometric verification. With a key registered, password sign-in also requires a key or a single-use recovery code. Existing keys remain available.

Directory pages render their README. Click a filename to preview Markdown, text, supported images, audio or video. Text previews are limited to 1 MiB; other files remain downloadable. Markdown links and local images stay pinned to the selected version. The interface defaults to dark; choose a theme under **Settings → Appearance** in the account menu. The choice is saved in your browser.

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
lode clone -include assets -exclude assets/cache https://lode.example.com/alice/example ../selected
lode -C ../selected select
lode -C ../selected select -include assets -include references
```

Log in first or set `LODE_TOKEN` before attaching or cloning. Both commands accept repeatable include/exclude options; the most specific path rule wins. Without rules, they select everything. Exclusions alone select everything except those paths. Use `select -include .` to select the whole project.

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
