# Architecture

Lode has one executable: a client for local folders and a Linux server for shared projects. The server stores accounts and project metadata in SQLite, with one Kopia content repository per project. Within an instance, `state/` holds configuration and SQLite, while `projects/` holds bulk storage and an instance identity marker. These directories may live on different filesystems.

A **version** describes a complete project tree. A **workspace** holds only the files and baseline metadata selected by its user. Saving a partial workspace preserves the rest of the project on the server.

## Responsibilities

| Package | Owns |
| --- | --- |
| `model` | File identities, changes, and selection rules; no IO |
| `workspace` | Local observation, selected baselines, and recoverable file changes |
| `remote` | Authenticated HTTP requests and verified transfers |
| `transfer` | Streaming librsync signatures, deltas, and reconstruction |
| `progress` | Transient activity reports; terminal rendering belongs to the CLI |
| `web` | Browser pages, sessions, previews, and the client API |
| `instance` | Accounts, project access, and repository lifetime |
| `project` | Versions, branches, publication, and retention policy |
| `content` | Native trees, verified writes, content protection, and collection |
| `cmd/lode` | Commands, credentials, and server lifecycle |

The client does not depend on SQLite or Kopia. The project service decides which versions exist and which contents remain available; the content layer represents, protects, and collects those contents.

## Saving and sharing versions

Directory metadata is immutable and shares unchanged name groups between versions. Reading an exact path loads one group per component. A partial save rewrites affected directories and reuses omitted subtrees.

Publication follows three steps:

1. The workspace records its request before contacting the server.
2. The server flushes and protects the new content tree.
3. One SQLite transaction records the version and advances the branch, provided its expected tip still matches.

Retries bind the original request to its authenticated actor. An accepted retry returns the same version. Cancellation either returns that accepted result or prevents the request from publishing later. Startup reconciliation releases orphan content protection only after checking accepted versions.

Branches point to immutable versions. Tags protect individual versions from expiry. Merge compares complete trees on the server, reuses existing file objects, and records both parents. Clients receive only their selected results. Ambiguous or insufficient ancestry stops automatic merging.

## Changing local files

Update compares three states: the saved baseline, local files, and the incoming version. Independent changes combine; conflicts require explicit whole-file or whole-directory choices. Unloading selected paths refuses to discard changed files or unselected children.

Workspace ignore rules filter new local paths during observation. Baseline and incoming paths stay visible so ignores cannot hide tracked changes or overwrite local data. Selection remains independent of ignores.

Clone, update, selection changes, switch, and restore share a durable plan and progress record. Downloads are verified before installation. Original files and replacements remain in staging until commitment; unexpected local paths stop the operation.

Before commitment, cancellation restores preserved originals without overwriting newer edits. After commitment, recovery finishes the new baseline and staging cleanup. External applications must stop writing during these operations; workspace locks only exclude other Lode commands.

Publishing and changing local files are separate steps. Recovering an accepted save updates the baseline without rewriting working files. Recovering an accepted merge uses ordinary update to reconcile its published result with local work. Restore uses the same file machinery but leaves restored contents as unsaved changes.

## Retention and collection

Retention keeps branch tips, recent first-parent versions, and tagged versions. Content availability comes from each version's native protection reference.

Cleanup binds a preview to the current policy and project state. Its transaction clears expired protection references, prunes eligible metadata, and advances an operation cutoff so forgotten requests cannot publish later. Surviving versions and recent retry outcomes retain the metadata they need. Native protection is released afterward; startup reconciliation completes interrupted releases.

Collection is a separate operation. Under the project lock, it checks retained protections and uses Kopia's garbage collection, index compaction, and pack rewriting. Interrupted collection is safe to retry. Lode keeps no separate garbage collector or persistent pack ledger.

Repository reads use the operating system's cache; Kopia's persistent caches are disabled. SQLite reclaims free pages incrementally. Storage reports measure content-repository files, separately from the database and caches.

## Concurrency and instance lifetime

Project mutations serialize while published versions remain readable. Cleanup and collection also wait for active readers before removing data. The instance holds an exclusive filesystem lock and bounds open repository handles; repository IO does not hold the shared handle-registry lock.

Project deletion first marks the project unavailable and revokes its scoped tokens. It then closes and removes native storage before forgetting metadata. Interrupted removal can finish on retry or restart. Access and membership changes check that the project is still available.

Initialization and backup hold the destination's root lock and require empty storage. They stage metadata on the state filesystem and synchronize project contents before atomically publishing `state/`. The bulk mount stays in place. Startup checks its identity against SQLite.

Backup also locks the stopped source. Only the copy is opened for recovery and verification: database integrity, project trees, and retained files' sizes and hashes. Interrupted copies without published state remain unusable and are never adopted by a retry. Restore uses the same operation and preserves instance identity.

The repository filesystem must be local, strongly consistent, and honor file and directory synchronization. Only Lode should access its repositories.

## Authentication and previews

Browser sessions and CLI bearer tokens share current account and project authorization. Tokens are capped by role and project scope and do not inherit browser administrator authority. Requests already authorized may finish after access changes.

An optional private origin exposes only the token API and health check, using an exact host and port match. The public browser origin, secure cookies, and WebAuthn configuration remain independent. Client HTTP consent lives with origin-specific user credentials. Changing a workspace's remote URL verifies its existing instance and project identities before updating local metadata.

Sessions carry the account generation captured during authentication. Security changes invalidate old generations and tokens atomically; delayed requests cannot revive them. WebAuthn challenges are bound to the browser and purpose, expire after three minutes, and are consumed once. Both passwordless key flows require user verification. Password sign-in with an enrolled key also requires key verification or a recovery code.

Browser forms have same-origin protection. Text previews are escaped; Markdown omits embedded HTML and external images. Local links and media identify immutable versions and use the same authorized content reader as downloads. Inline HTML and SVG are refused.

Sign-in attempts are throttled by source address in a bounded table. Forwarded addresses are accepted only through configured trusted proxies. Browser forms have size and time limits; streamed transfers refresh an idle deadline. Unfinished sign-in sessions expire after five minutes. Username-first security-key login exposes credential descriptors to the requesting browser; account names and key enrollment are not confidential.

## Resource costs

Changed large files use librsync deltas when a suitable basis exists; other files transfer whole. Small downloads use bounded batches. Every reconstructed file is checked against its complete size and hash before installation.

Workspaces avoid downloading omitted trees and contents, but observation still hashes local files. Merge currently holds complete comparison metadata in server memory. Collection needs temporary space to rewrite packs; backup needs space for a complete copy.

See the [user guide](guide.md) for command behavior and [server operations](operations.md) for deployment and maintenance.
