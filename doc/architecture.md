# Architecture

Lode's design combines complete project checkpoints, ordinary local workspaces, and a single authoritative server.

## Core concepts

| Concept | Meaning |
| --- | --- |
| Project | A directory tree published as a coherent unit |
| Checkpoint | An immutable accepted project state |
| Workspace | Editable files, one base checkpoint, and a selection of local paths |
| Milestone | A name that protects a checkpoint's contents from cleanup |

Every checkpoint describes the complete project, including files omitted from a partial workspace. Local selection controls which files are present for editing.

## State and storage

The server owns accepted history and retention decisions. A storage engine represents file contents and directory trees, sharing repeated content across checkpoints. Workspace metadata records the base, selection, and progress of local operations.

Saving applies captured changes to the stored base tree and reuses unchanged entries. Publication checks the expected current checkpoint and accepts the result only after its contents are durable and protected.

Updating advances the workspace base while preserving compatible local edits. Restoring historical files creates local changes against the existing base. Conflicting binary files require a whole-file choice.

Retention determines which historical contents remain retrievable. Current checkpoints and milestones stay protected. Releasing older contents does not change retained checkpoints or remove local working files.
