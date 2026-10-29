# Using Lode

A **project** holds complete saved versions on the server. A **workspace** is your local folder, with all or part of a project. Most work happens on the shared `main` branch; create another branch when you need one.

Editing and `status` work offline. Saving and downloading require the server.

[Connect](#connect) · [Daily work](#daily-work) · [Ignore files](#ignore-files) · [Selected files](#selected-files) · [Branches](#branches-and-versions) · [Storage](#keep-versions-and-reclaim-space) · [Sharing](#sharing-and-access) · [Security](#account-security) · [Recovery](#interrupted-work)

## Connect

Create a project in the browser, or ask its owner to add you. Under **Settings → Access tokens**, create a token with the access you need, then sign in:

```sh
lode login https://lode.example
```

The token prompt is hidden. Credentials live in your OS user configuration directory, outside the workspace. Scripts can use `LODE_TOKEN` instead. Remote connections use HTTPS unless you explicitly trust a private HTTP origin.

Connect an existing folder to an empty project with `attach`. The local folder must already exist:

```sh
lode attach https://lode.example/alice/example ./work
```

To download a saved project into an empty or nonexistent folder, use `clone`:

```sh
lode clone https://lode.example/alice/example ./work
```

The browser's **Clone** button copies a command for the selected branch. Workspace metadata lives in `.lode/`; leave that directory in place.

Run commands inside the workspace, or use `lode -C ./work status`. For command help, use `lode help clone` or `lode clone --help`. Put options before positional arguments.

### Change the server address

Each workspace has one remote project URL. Show it with `lode remote`. To use a configured [private endpoint](operations.md#private-cli-access), sign in there and change the URL:

```sh
lode login --allow-http http://lode.internal:8473
lode remote set-url http://lode.internal:8473/alice/example
```

`--allow-http` trusts only that exact origin and is remembered with your credentials. Use it only over a protected network such as Tailscale; ordinary HTTP sends tokens and data without TLS. Omit the option for HTTPS. Scripts using `LODE_TOKEN` still need this saved HTTP consent.

The new URL must identify the same instance and project. Branch, selected files, history, and unsaved work stay intact. Finish pending operations with `resume` or `cancel` before changing the URL. Switch back with `lode remote set-url https://lode.example/alice/example`; no download is needed.

## Daily work

After editing your files:

```sh
lode status
lode save
lode log
```

In a terminal, `save` prompts for an optional message; press Enter to leave it blank. Use `lode save -m 'Update assets'` to skip the prompt. Scripts do not prompt; supply `-m` when you want a message.

Stop applications writing to the project during saves and workspace changes. Lode versions ordinary files and directories; it does not capture a consistent snapshot of a running application. Selected symlinks and special files are refused.

Longer commands show one updating progress line in an interactive terminal: the current phase, file, bytes processed, and elapsed time. Byte counts cover the current file, including local processing during delta transfers. Progress goes to stderr and disappears when the command finishes. Redirect stderr or use `lode --no-progress save` to disable it.

### Get others' changes

```sh
lode update
```

Update combines independent edits with your local work. If someone saves first, your save is refused without changing local files: update, review the result, and save again.

Conflicts stop before working files change. Choose which contents to keep at each conflicting path:

```sh
lode update --local assets/model.bin --incoming references/source.bin
```

- `--local` keeps your local contents.
- `--incoming` takes the shared version.
- A directory choice covers its whole subtree.

Repeat these options for additional conflicts. Lode treats binary files as whole files; it does not merge their internal formats.

## Ignore files

Lode ignores new `.DS_Store`, `Thumbs.db`, and `desktop.ini` files at any depth. Other dotfiles and application files are included normally.

Put project rules in `.lodeignore` at the workspace root:

```gitignore
# Local generated files
cache/
*.tmp
!reference.tmp
```

Rules use familiar ignore syntax: `*`, `?`, character classes, and `**` for directory levels. A trailing `/` matches directories; a leading `/` anchors a rule at the root. Patterns containing a slash are root-relative; other patterns match names anywhere. `#` starts a comment and `!` includes a match again. Later rules win, but an excluded parent directory must be included before its children can be included.

For rules specific to this workspace, use `.lode/ignore`. These override `.lodeignore`, which overrides the defaults. For example, `!.DS_Store` includes that name again. Lode reads these two files only; it does not read `.gitignore` or nested ignore files.

Ignores affect new local files. Already saved files remain tracked, including their edits and deletions. To remove an accidentally saved file, delete it locally and save that deletion. Ignored local files are preserved during workspace changes; a collision with an incoming saved file still requires resolution.

`.lodeignore` is an ordinary versioned file. Include it in your selection to share its rules. Selection controls which saved paths you download; ignores control which new local files you save.

## Selected files

Download only the paths you need:

```sh
lode clone --include assets --exclude assets/cache \
  https://lode.example/alice/example ./assets-work
```

Rules use project-relative paths and can be repeated. The closest ancestor rule wins. Only selected files and baseline metadata are downloaded; saves preserve omitted project contents.

Inside a workspace, inspect or replace the selection:

```sh
lode select
lode select --include assets --include references
lode select --include assets
```

Changing selection uses the workspace's current version. Clean files outside the new selection are unloaded. Lode refuses to unload changed files and leaves unselected local files alone. Use `lode select --none` to unload all clean selected paths.

Loading over different local contents requires `--local` or `--incoming`, alongside the selection options. Deleting a partially selected directory removes its selected contents while preserving unseen descendants.

## Branches and versions

### Branches

Create a branch at your workspace's saved version and switch to it:

```sh
lode branch
lode branch create experiment
lode switch experiment
```

Switching preserves your selection. Save local changes before switching to a different version. To start elsewhere, use `lode branch create --from main experiment`; to clone a branch, use `lode clone --branch experiment <project-url>`.

Writers can create branches. Maintainers can delete a branch other than `main` with `lode branch delete experiment`, after switching away from it. Deleting a branch leaves its saved versions intact.

### Merge

With clean selected files, integrate another branch into `main`:

```sh
lode switch main
lode update
lode merge experiment
```

Merge combines complete project versions on the server, including paths you have not downloaded, then updates your selected files. If it finds conflicts, retry with whole-file choices:

```sh
lode merge --local assets/model.bin --incoming references/source.bin experiment
```

For merge, `--local` means the current branch's saved contents; `--incoming` means the source version. Directory choices cover the whole subtree. Insufficient or ambiguous history prevents automatic merging; retained contents can still be restored explicitly.

### Restore

Restore saved contents as local changes, then review and save them:

```sh
lode log
lode restore <version> assets/model.bin
lode status
lode save -m 'Restore model'
```

A reference can be a branch, tag, full version ID, or the short ID shown by `log`. Use `lode log --full` for complete IDs. If a name is ambiguous, qualify it as `branch:<name>`, `tag:<name>`, or `version:<id>`.

Without paths, restore covers your current selection. It refuses to overwrite changed target files, preserves unrelated work, and never loads excluded paths. Your branch and saved baseline stay unchanged until you save.

## Keep versions and reclaim space

**Account settings → Usage** lists storage for projects you own. Project sizes include history and uncollected data; shared server metadata is excluded.

Tags name and protect complete versions:

```sh
lode tag create release-one
lode tag
```

A tag uses your workspace's saved version, or another reference supplied with `--from`. Tags cannot move. Writers can create them; maintainers can remove them with `lode tag delete release-one`.

Maintainers control storage under **Project settings → Storage**, or through three commands:

| Command | Effect |
| --- | --- |
| `retention` | Show or change the policy; delete nothing |
| `cleanup` | Preview expiry and history removal; print a command to apply it |
| `collect` | Reclaim unused content storage after cleanup |

```sh
lode retention --keep 20 --history-days 90
lode cleanup
# Review the preview, then run its printed apply command.
lode collect
```

The defaults keep 20 versions per branch and 90 days of unprotected history metadata. Branch tips and tagged versions remain protected. A cleanup approval lasts one hour and is refused if relevant project state changes; preview again in that case.

Before applying cleanup:

- Expired contents cannot be downloaded or restored. Increasing retention cannot recover them.
- Tags protect their chosen version, not every ancestor. Removing older history can prevent future automatic merges.
- Local files are unchanged. Recent save outcomes remain available for at least the seven-day retry window.

Collection waits for readers and temporarily blocks other operations on that project. It needs free space to rewrite packs and is safe to retry after interruption. Its before/after figures cover content repositories, excluding the database and caches. Small unused pack fragments may remain.

## Sharing and access

An administrator creates accounts under **Site administration → Users**. Public registration is closed. Each user can create private projects and add collaborators under **Project settings → Collaborators**.

| Role | Access |
| --- | --- |
| Reader | Browse and download |
| Writer | Also save and create branches or tags |
| Maintainer | Also manage collaborators, retention, and branch or tag removal |

Owners have maintainer access. Tokens cover one project or all accessible projects and cap access at their chosen role. Create or revoke them under **Settings → Access tokens**.

**If the CLI says “access denied”:** check project membership, token scope, and token role. Administrator access to other users' projects is browser-only; a personal token still needs membership.

Permission changes affect subsequent requests; already authorized operations may finish. Removing access leaves local files intact.

### Browse and delete

The browser previews text, Markdown, supported images, audio, and video. **Download** retrieves the original file. Expired contents are unavailable.

Owners and site administrators can delete a project under **Project settings → Delete project** by confirming its full name. This permanently removes its server files, history, and scoped tokens. Local workspaces remain intact. A maintainer collaborator cannot delete the project.

If deletion is interrupted, retry from its settings page. Administrators can find pending deletions under **Site administration → Projects**.

## Account security

Manage your password and keys under **Settings → Security**.

### Passkeys and security keys

- **Passkey:** sign in without entering a username.
- **Security key — enter a username:** select the account before using the key.

Both passwordless flows require the key's PIN, biometric check, or equivalent verification. Once a key is enrolled, password sign-in also requires a key or a recovery code. Save the ten recovery codes shown after enrollment; each works once, with your password.

Adding or removing a key, or replacing recovery codes, requires your password. It signs out other sessions and revokes access tokens while keeping your current session signed in. Removing the last key restores password-only sign-in and removes recovery codes.

### Password changes and recovery

Changing your password signs out every session and revokes access tokens, but preserves enrolled keys. Sign in again, create a new token, and run `lode login` to reconnect the CLI. Local files are unaffected.

An administrator can reset your password; this also removes keys and recovery codes. Operators who cannot sign in can use [server account recovery](operations.md#accounts-and-recovery).

## Interrupted work

Start with `lode resume`. If recovery reports an unexpected local path, move that path aside before retrying; it will not be overwritten.

| Situation | What to do |
| --- | --- |
| Interrupted clone, update, selection change, switch, or restore | `lode resume` continues; `lode cancel` reverses unfinished changes |
| Save with an unknown server outcome | Resume or cancel checks the original outcome before proceeding |
| Merge published, but local update failed | Use `lode update`; use `resume` if a local transition is still unfinished |
| Old save outcome no longer available | Follow the instruction to run `lode update` |

Save cancellation requires the server. It records an already accepted version locally or prevents that request from publishing later, leaving working files untouched. Losing write access does not prevent a reader from settling their own pending save.

Cancelling unfinished local file changes works offline and restores preserved originals. Newer edits stop cancellation instead of being overwritten. Recovery errors identify any files to move aside, including preserved files under `.lode/staging`. Once an operation has committed, resume or cancel finishes its baseline and cleanup rather than reversing it.

An accepted merge remains published even if its local update fails or is cancelled. Previously unaccepted save requests expire after seven days; update reconciles from the existing baseline without assuming an expired request was never published.
