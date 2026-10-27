# Architecture

Lode is one Go module and executable. The server owns shared history and content protection. A workspace owns selected local files, its baseline, and any unfinished operation.

| Package | Responsibility |
| --- | --- |
| `model` | Pure path, selection, entry, comparison and whole-entry merge rules |
| `project` | Publication decisions, SQLite history, and branch authority |
| `content` | Kopia objects, complete snapshot roots, retrieval, and release |
| `remote` | Authenticated HTTP requests and streamed file transfers |
| `workspace` | Local observation, selected transitions, baseline, and recovery |
| `cmd/lode` | CLI and server lifecycle |

These are concrete components. The project service owns decisions; the content adapter supplies native storage operations.

## Versions and selection

A version describes a complete directory tree. A workspace's selection controls local presence, not the completeness of its saved version. Saving exact path changes preserves omitted entries. Removing a directory cannot implicitly delete unseen descendants. Unreadable selected paths fail observation rather than becoming deletions.

SQLite stores per-path changes with a complete checkpoint every 64 first-parent steps. Selected reads query indexed path ranges and replay at most 63 changesets. Native object references remain on the server. Kopia represents and protects the complete content tree; it does not decide whether a branch may advance.

Branches name versions; creating a branch copies no contents. Reference changes are serialized, and deletion checks the expected tip. The service retains every accepted version. Logical history, content availability, and workspace presence remain separate concepts.

Merges compare complete logical trees on the server and follow both parents to find common ancestors. Ambiguous best bases require an explicit common ancestor. Whole-entry choices select files or entire subtrees; selected native objects are reused from the parents. Every published merge records both parents, except a merge into an empty branch, whose sole parent is the source. A source already in the destination's ancestry produces no version.

## Publication

1. The workspace records an operation ID, expected branch tip, changes, and file size/SHA-256 identities before uploading.
2. The service durably binds the operation ID to that request's fingerprint and checks the branch tip.
3. Kopia captures changed contents, reuses omitted contents, and flushes a protected complete snapshot tagged with the operation ID.
4. One SQLite transaction records the version, advances the branch conditionally, and records acceptance. The operation ID is also the version ID.
5. The workspace updates its selected baseline without rewriting local files.

Merges use the same publication path with immutable source and destination versions and explicit choices. They upload no file contents. Acceptance changes shared history; the workspace keeps its baseline and files until a separate update. This separates publication recovery from local mutation.

Retries resolve the original outcome. Accepted, stale, unchanged, and cancelled operations keep their decisions. Reusing an operation ID for another intent fails. Cancellation also blocks a request that has not yet arrived.

Startup checks accepted snapshot protection and releases unaccepted candidates. It never promotes an unfinished capture into history. An uncertain SQLite acceptance error leaves protection intact until the database outcome is resolved.

## Local recovery

A workspace holds one unfinished save or local transition. Local process locking prevents concurrent commands from changing its state. State files are synchronized and replaced atomically.

Checkout, update, switch, selection changes and historical restore use one transition mechanism. Planning compares baseline, observed and target entries. Independent edits survive; conflicting file or structural changes require whole-entry choices. Unloading local changes is rejected. Physical containers outside the selection remain in place. Restore changes only requested selected paths and keeps the baseline; overwriting local changes requires an explicit force option.

The journal contains the target baseline, ordered before/after entry changes, a progress cursor and cancellation decision. Historical restore also records its content source because it differs from the baseline. For each change, the workspace stages and verifies required bytes, atomically preserves the previous entry, and installs the target without replacing an unexpected path. Existing descendants leave before parent type changes; new parents arrive before children. Directory removal requires emptiness, including at application time.

The cursor advances only after the file action is synchronized. Reaching the end durably decides to commit; the target baseline is then installed before recovery files are cleaned up. Cancellation before that decision reverses the applied changes while preserving unexpected edits. Partial-action progress can be inferred from the preserved and staged entries. Changed recovery contents are retained and reported.

Initial checkout starts with a valid empty-selection workspace and uses the same transition to populate it. A metadata-only transition needs only an atomic baseline update. These operations require applications to stop writing affected files; process locking excludes other Lode commands, not external writers.

Save recovery uses the original request. Later file edits remain changes against the accepted baseline.

## Storage and transport

The service serializes mutations. Reads hold shared protection while streaming. A local ownership lock excludes a second service process. SQLite uses durable transactions. The filesystem content adapter synchronizes Kopia's blob directory chain after writes and deletions; Kopia synchronizes blob file contents before rename. These contracts require Linux local filesystems that honor synchronization.

The server directory contains project identity and secrets, SQLite metadata, and the native content repository. Native connection configuration and bounded caches live there and are derived from its current location. Clients receive selected metadata and requested file bytes. HTTP requires a project token and expected project identity; plaintext client connections are limited to loopback addresses.
