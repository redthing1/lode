# Architecture

Lode is one Go module and executable. The server owns shared history and content protection. A workspace owns selected local files, its baseline, and any unfinished operation.

| Package | Responsibility |
| --- | --- |
| `model` | Pure path, selection, entry, comparison and whole-entry merge rules |
| `project` | Publication, SQLite history, references, and retention authority |
| `content` | Kopia objects, snapshot roots, retrieval, release, and collection |
| `remote` | Authenticated HTTP requests and streamed file transfers |
| `workspace` | Local observation, selected transitions, baseline, and recovery |
| `cmd/lode` | CLI and server lifecycle |

These are concrete components. The project service owns decisions; the content adapter supplies native storage operations.

## Versions and selection

A version describes a complete directory tree. A workspace's selection controls local presence, not the completeness of its saved version. Saving exact path changes preserves omitted entries. Removing a directory cannot implicitly delete unseen descendants. Unreadable selected paths fail observation rather than becoming deletions.

SQLite stores per-path changes with a complete checkpoint every 64 first-parent steps. Selected reads query indexed path ranges and replay at most 63 changesets. Native object references remain on the server. Kopia represents and protects the complete content tree; it does not decide whether a branch may advance.

Branches name versions; creating a branch copies no contents. Tags protect named versions. Reference changes are serialized, deletion checks the expected target, and new targets require retained contents. Logical history, content availability, and workspace presence remain separate concepts.

Merges compare complete logical trees on the server and follow both parents to find common ancestors. Ambiguous best bases require an explicit common ancestor. Whole-entry choices select files or entire subtrees; selected native objects are reused from the parents. Every published merge records both parents, except a merge into an empty branch, whose sole parent is the source. A source already in the destination's ancestry produces no version.

## Publication

1. The workspace records an operation ID, expected branch tip, changes, and file size/SHA-256 identities before uploading.
2. The service durably binds the operation ID to that request's fingerprint and checks the branch tip.
3. Kopia captures changed contents, reuses omitted contents, and flushes a protected complete snapshot tagged with the operation ID.
4. One SQLite transaction records the version, advances the branch conditionally, and records acceptance. The operation ID is also the version ID.
5. The workspace updates its selected baseline without rewriting local files.

Merges use the same publication path with immutable source and destination versions and explicit choices. They upload no file contents. Acceptance changes shared history; the workspace keeps its baseline and files until a separate update. This separates publication recovery from local mutation.

Retries resolve the original outcome. Accepted, stale, unchanged, and cancelled operations keep their decisions. Reusing an operation ID for another intent fails. Cancellation also blocks a request that has not yet arrived.

Startup checks retained snapshot protection and releases unaccepted or expired roots. It never promotes an unfinished capture into history. An uncertain SQLite acceptance error leaves protection intact until the database outcome is resolved.

## Retention and collection

Cleanup protects branch tips, tags and a requested number of first-parent versions per branch. Preview explains those reasons. Its token binds the count, immutable version identities and current references. Apply recomputes that decision under the exclusive project gate and rejects a changed token. Availability is excluded from the token, so repeating the same expiry decision is idempotent.

One SQLite transaction clears the native root reference of each eligible version. An empty root is the durable payload-availability fact; logical entries, parents and publication outcomes remain. Native manifests are released by collection or startup reconciliation only after that decision. Interrupted cleanup can leave extra protection temporarily, but cannot release contents still promised by SQLite.

Reads check availability and hold the shared gate for their full stream. Publication, reference changes, expiry and collection use its exclusive side. Merges require retained parent payloads while common-ancestor comparison uses logical metadata even after expiry. An idle workspace base has no retention lease; its local baseline supports comparison and update. A multi-file transition can encounter expiry between reads and remains cancellable.

The content adapter invokes Kopia's ordinary full snapshot garbage collection and maintenance with its standard safety delays. Kopia owns reachability, pack rewriting, deletion and maintenance state. Lode reports physical blob totals before and after the request; a successful request does not promise immediate reclamation. There is no separate pack ledger or collection scheduler.

## Local recovery

A workspace holds one unfinished save or local transition. Local process locking prevents concurrent commands from changing its state. State files are synchronized and replaced atomically.

Checkout, update, switch, selection changes and historical restore use one transition mechanism. Planning compares baseline, observed and target entries. Independent edits survive; conflicting file or structural changes require whole-entry choices. Unloading local changes is rejected. Physical containers outside the selection remain in place. Restore changes only requested selected paths and keeps the baseline; overwriting local changes requires an explicit force option.

The immutable plan contains the target baseline and ordered before/after entry changes. A separate small progress record holds the cursor and cancellation decision, avoiding rewriting the full plan after each file. Historical restore also records its content source because it differs from the baseline. For each change, the workspace stages and verifies required bytes, atomically preserves the previous entry, and installs the target without replacing an unexpected path. Existing descendants leave before parent type changes; new parents arrive before children. Directory removal requires emptiness, including at application time.

Initial progress is durable before the plan appears. The cursor advances only after the file action is synchronized. Reaching the end durably decides to commit; the target baseline is then installed before recovery files are cleaned up. Cleanup removes the plan before its progress record, so a present plan never loses its commit decision. Orphan progress is overwritten before the next plan. Cancellation before commitment reverses applied changes while preserving unexpected edits. Partial-action progress can be inferred from preserved and staged entries. Changed recovery contents are retained and reported.

Initial checkout starts with a valid empty-selection workspace and uses the same transition to populate it. A metadata-only transition needs only an atomic baseline update. These operations require applications to stop writing affected files; process locking excludes other Lode commands, not external writers.

Save recovery uses the original request. Later file edits remain changes against the accepted baseline.

## Storage and transport

The service serializes mutations. Reads hold shared protection while streaming. A local ownership lock excludes a second service process. SQLite uses durable transactions. The filesystem content adapter synchronizes Kopia's blob directory chain after writes and deletions; Kopia synchronizes blob file contents before rename. These contracts require Linux local filesystems that honor synchronization.

The server directory contains project identity and secrets, SQLite metadata, and the native content repository. Native connection configuration and bounded caches live there and are derived from its current location. Clients receive selected metadata and requested file bytes. HTTP requires a project token and expected project identity; plaintext client connections are limited to loopback addresses.

Backup uses the same ownership lock as the service. It opens and reconciles the source, closes SQLite and Kopia while retaining that lock, then copies the cold database and native repository into a private staging directory. It preserves file modification times used by native maintenance and excludes derived configuration/caches. Reopening the independent copy verifies its retained roots and SQLite consistency before a synchronized no-replace rename publishes it. Restoration is the same operation applied to a backup; no separate archive format or backup metadata ledger exists.
