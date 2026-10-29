# Using Lode

A project contains saved versions on a shared branch. A workspace is an ordinary folder containing selected project files and local changes. Saves publish online; editing and status work offline.

## Connect

Create a project in the browser. In account settings, create an access token with write access to that project, then sign in:

```sh
lode login https://lode.example
```

The token prompt is hidden in a terminal. Credentials are stored privately in your OS user configuration directory, outside the workspace. `LODE_TOKEN` can supply or override the token. HTTP connections are limited to loopback; remote access requires HTTPS.

Use `attach` for an existing folder and an empty project. Use `clone` to obtain an existing version:

```sh
lode attach https://lode.example/alice/example ./work
lode clone https://lode.example/alice/example ./another-workspace
```

Clone requires an empty or nonexistent destination. Workspace metadata lives in `.lode/`.

## Share a project

An administrator creates accounts under **Site administration → Users**. Public registration is closed. Each account can create private projects under its username.

Owners add collaborators in **Project settings → Collaborators**:

| Role | Access |
| --- | --- |
| Reader | Browse and download |
| Writer | Also save changes and create branches/tags |
| Maintainer | Also manage collaborators, retention, and branch/tag removal |

Owners retain maintainer access. Administrators can manage all projects through the browser. Personal tokens apply to one project or all accessible projects, with a maximum reader, writer, or maintainer role. They never grant more access than the account currently has and do not inherit site-administrator authority. Create and revoke them under **Settings → Access tokens**.

Role changes affect subsequent requests, including existing tokens. Requests already authorized may finish. Removing access leaves local files intact.

## Browse and manage projects

The browser shows files, saved versions, text and Markdown previews. Images, audio and video use the browser's supported formats; Markdown can embed images from the same saved version. Download retrieves the original file. Expired contents cannot be previewed or downloaded.

Owners and site administrators can remove a project under **Project settings → Delete project** by confirming its full name. Deletion permanently removes its files, history and project-scoped tokens; local workspaces remain intact. Active requests must finish first. Interrupted deletion leaves the project unavailable: retry from its deletion page or restart the server to finish. Administrators can find pending deletions under **Site administration → Projects**. Collaborator maintainer access does not permit project deletion.

## Account security

Change your password under **Settings → Security**, using your current password. A password change signs you out on every device and revokes your access tokens. Create a new token after signing in, then run `lode login` again. Local workspaces and files are preserved.

Administrators can reset passwords, enable or disable accounts, and change administrator access under **Site administration → Users**. These changes also end the affected account's sessions and revoke its tokens. Disabling an account preserves its projects and memberships; enabling it again requires a fresh sign-in. The last enabled administrator cannot be disabled or demoted.

Add a passkey or security key under **Settings → Security**. A passkey supports sign-in without a username. Choose **Security key — enter a username** for a key you intend to select by account name. Both passwordless paths require the key's PIN, biometric verification, or equivalent user verification.

Once a key is enrolled, password sign-in also requires a key or a recovery code. Save the ten recovery codes shown after first enrollment; each works once, with your password. You can replace the codes in Security. Adding or removing a key and replacing codes require your current password, sign out other sessions, and revoke access tokens. Your current session remains signed in. Removing the last key returns to password-only sign-in and removes recovery codes.

Changing your own password preserves enrolled keys. An administrator password reset removes keys and recovery codes. A local operator can recover an account while the server is stopped:

```sh
lode recover-user ./instance alice
```

The command prompts for a new password, enables the account, and invalidates its sessions, tokens, keys, and recovery codes. Its role, projects, and memberships are preserved. Scripts may supply the new password as one line on stdin.

## Select files

Include or exclude project-relative paths. Flags may be repeated; the nearest ancestor rule wins.

```sh
lode clone -include assets -exclude assets/cache \
  https://lode.example/alice/example ./assets-work
```

Only selected files and baseline metadata are downloaded. Saving preserves omitted project contents. Removing a partially selected directory removes its selected contents, preserving unseen descendants.

Inspect or replace an existing workspace's selection:

```sh
lode select
lode select -include assets -include references
lode select -include assets
```

Selection changes use the workspace's current version. Clean files outside the new selection are unloaded; changed files refuse unloading. Unselected local files remain untouched. Use `lode select -none` to unload all clean selected paths. Loading over different local contents requires the same conflict choices as update.

## Save and inspect

```sh
lode status
lode save -m 'Update assets'
lode log
```

Commands find the workspace from the current directory or a parent. Use `lode -C ./work status` to work elsewhere.

Lode handles ordinary files and directories. Stop applications writing those files during saves and workspace operations; Lode does not take application-consistent snapshots of running programs. Selected symlinks and special files are refused.

Each save requires the branch to match the workspace's base version. A stale save leaves local files intact. Update to reconcile the latest shared version, then save again:

```sh
lode update
lode save -m 'Combine changes'
```

Independent edits survive. Conflicting file edits, deletions, and file/directory replacements stop before changing working files. Choose each conflicting path explicitly:

```sh
lode update -local assets/model.bin -incoming references/source.bin
```

`-local` keeps local contents; `-incoming` takes the shared version. A directory conflict chooses the whole subtree. For selection conflicts, repeat the selection rules with these flags. Update settles any pending save first, recovering its accepted baseline or preventing that request from publishing later.

## Branches and restore

Projects start on `main`. Create an experiment at your workspace's saved version, then switch to it:

```sh
lode branch
lode branch create experiment
lode switch experiment
```

Writers can create branches. Switching preserves the selection and leaves unselected local files alone. Unsaved changes can follow a branch at the same version; save them before switching to a different version. Use `lode branch create -from main experiment` to start at another branch or version.

Clone directly from a branch with `lode clone -branch experiment <project-url>`. The browser's branch picker shows its files; **All branches** lists branch tips. Maintainers can delete a non-default branch using `lode branch delete experiment` after switching elsewhere. Deletion refuses a tip that changed during the request and leaves saved versions intact.

Integrate another branch or saved version into the current branch:

```sh
lode switch main
lode update
lode merge experiment
```

Merge requires clean selected files and an up-to-date target branch. It combines complete project versions on the server, including excluded paths, then updates your selected files. It uploads no file contents. Conflicts stop publication; retry with explicit choices:

```sh
lode merge -local assets/model.bin -incoming references/source.bin experiment
```

Here `-local` keeps the current branch's saved contents; `-incoming` takes the source version. Directory choices cover the whole subtree. The saved merge records both parents, including when choices leave the target files unchanged. Already integrated sources create no new version. Ambiguous or missing common ancestry prevents automatic integration; retained contents can still be restored explicitly.

Restore earlier contents as local changes:

```sh
lode log
lode restore <version> assets/model.bin
lode status
lode save -m 'Restore model'
```

Use a branch or tag name, a full version ID, or the short ID shown by history. Ambiguous references are refused; qualify them with `branch:<name>`, `tag:<name>`, or `version:<full-id>`. `lode log -full` shows complete IDs. With no paths, restore covers the workspace's selection. It never loads excluded paths, refuses to overwrite changed target files, and preserves unrelated local work. The branch and saved baseline remain unchanged until you save.

## Protect versions and clean up

Tags name and protect complete saved versions:

```sh
lode tag create release-one
lode tag
lode restore release-one assets/model.bin
```

Creation uses the workspace's saved version; `-from <reference>` selects another. Tags cannot move. Writers can create them; maintainers can remove protection with `lode tag delete release-one`. The browser lists tags in the branch menu.

Maintainers manage retention in **Project settings → Storage** or through the CLI:

```sh
lode retention -keep 20 -history-days 90
lode cleanup
```

Changing policy removes nothing. Cleanup previews its consequences and prints an apply command valid for one hour. Application refuses if relevant project state changed; preview again. Branch tips, recent first-parent versions per branch, and tagged versions keep their contents. Defaults are 20 versions per branch and 90 days of unprotected history metadata.

Expired contents cannot be downloaded or restored. Their history remains until eligible for removal; recent save outcomes remain for at least the seven-day retry window. Tags protect their chosen version, not every ancestor. Removing history can prevent later automatic merges, and history identifies removed boundaries. Increasing retention cannot recover expired contents or removed history. Cleanup leaves local files unchanged.

Cleanup releases content protection and prunes obsolete metadata. Reclaim unreferenced content storage afterward:

```sh
lode collect
```

Collection is also available in **Project settings → Storage**. It reports allocated content-repository space before and after; these figures exclude the database and cache. Retained versions and shared contents stay available after collection. Small unused fragments may remain in otherwise efficient packs.

Collection waits for active reads and temporarily blocks other project operations. Keep free disk space for rewriting packs. If interrupted, collection may have partly completed; run it again. No additional history is expired by collecting.

## Interrupted operations

Run `lode resume` to continue an interrupted operation. Downloads are verified before installation; an unexpected local path stops recovery without being replaced. Move it aside before resuming.

Save recovery checks the original server outcome before reading local files. If the save was accepted, recovery updates the baseline while preserving subsequent edits.

For a pending save, `lode cancel` records an already accepted version locally or prevents that request from publishing later. It requires server access and leaves local files untouched.

Readers can still settle their own pending saves after losing write permission. Cancellation cannot affect another account's request.

Merge recovery resolves the original source version and choices before updating local files. An accepted merge stays published if the local update fails or conflicts with later edits. Use `lode update` to reconcile those edits, or `lode resume` for an unfinished local transition. Cancelling an accepted merge leaves the workspace baseline and files unchanged; update when ready. If the process stops between resolving publication and starting the local update, the unchanged baseline remains sufficient for `lode update`.

For an unfinished clone, update, selection change, switch, or restore, `lode cancel` restores preserved originals without contacting the server. Newer local edits stop cancellation instead of being overwritten. Recovery errors identify contents that need moving aside, including preserved files under `.lode/staging`. Once all actions have durably completed, resume or cancel finishes the committed baseline and cleanup instead of reversing the operation.

Previously unaccepted save requests expire after seven days. If an old save outcome has been removed, `lode resume` and `lode cancel` direct you to `lode update`. Update reconciles from the existing baseline while preserving local edits; it does not assume the expired save was unpublished.
