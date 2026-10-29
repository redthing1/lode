---
name: lode
description: Use the Lode CLI to version binary projects, manage workspaces, and share or recover changes.
---

# Lode client

Lode is a version control system for large binary projects.

- A **project** holds complete versions on the server.
- A **workspace** is a local folder on a branch, with all or selected project paths.
- A **save** publishes local changes. Paths omitted from a workspace remain in the project.
- Conflicts are resolved as whole files or directories.

Run commands in the workspace or use `lode -C <directory> <command>`. Use `lode --help` or `lode help <command>` for details. Options precede positional arguments.

## Commands

| To | Run |
| --- | --- |
| Sign in with an access token | `lode login <server-url>` |
| Download into an empty or new folder | `lode clone <project-url> <directory>` |
| Connect an existing folder to an empty project | `lode attach <project-url> <directory>` |
| Inspect local changes or selection | `lode status` or `lode select` |
| Get shared changes | `lode update` |
| Publish local changes | `lode save -m 'message'` |
| Browse versions | `lode log` |
| Restore contents as unsaved changes | `lode restore <version> [path ...]` |
| Work with branches | `lode branch`, `lode switch <branch>`, `lode merge <branch>` |

Create access tokens in server account settings. `status` works offline. Keep the workspace's `.lode/` directory intact.

## Working with files

- Review `status` before saving. If the branch advanced, update, review, and save again.
- Stop applications writing to the workspace during saves and file-changing operations.
- Clone with `--include <path>` and `--exclude <path>` to select project-relative paths. `lode select --include <path>` replaces the selection and may unload clean files.
- `.lodeignore` and `.lode/ignore` filter new local paths, not tracked changes. Lode does not read `.gitignore`.
- Merge combines complete server versions, including paths absent from the workspace, then updates selected files.

## Conflicts and recovery

For conflicts, retry with `--local <path>` or `--incoming <path>`. Ask if the intended choice is unclear.

| Operation | Local | Incoming |
| --- | --- | --- |
| Update | Workspace contents | Shared version |
| Merge | Current branch's saved contents | Source version |

- `lode resume` continues an interrupted operation. `lode cancel` settles a pending save or reverses unfinished local changes.
- A pending save may already be published; cancellation checks with the server. A published merge stays published even if its local update fails.
- Follow recovery errors and preserve unexpected paths and `.lode/staging` files.

## Storage

`lode cleanup` previews content expiry and history pruning; review the preview before applying it. `lode collect` separately reclaims storage. Keep shared changes within the user's requested scope.
