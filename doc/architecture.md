# Architecture

Lode is one Go module and executable. The project service owns shared history and retention. A workspace owns selected local files, its baseline, and unfinished work. SQLite stores application facts; Kopia represents and collects content; librsync transforms transfer bytes.

| Package | Responsibility |
| --- | --- |
| `model` | Pure path, selection, entry, comparison, and whole-entry merge rules |
| `project` | Publication, SQLite history, references, and retention authority |
| `content` | Kopia objects, protected snapshot roots, retrieval, release, and collection |
| `transfer` | Bounded streaming signature, delta, and patch calls to librsync |
| `remote` | Authenticated HTTP operations and streamed file transfers |
| `workspace` | Local observation, selected transitions, baseline, and recovery |
| `cmd/lode` | CLI presentation and server lifecycle |

## Versions and presence

Every version describes a complete directory tree. Workspace selection controls local presence, while retention controls downloadable contents. Saving exact path changes preserves omitted entries. Directory deletion cannot implicitly erase unseen descendants; failed observation cannot become an inferred deletion.

Five SQLite tables hold commits, path changes/checkpoints, branches, tags, and operation outcomes. Path changes are relative to the first parent, with a full checkpoint every 64 steps. Selected reads use indexed exact-path and descendant-range queries, replay at most 63 changesets, deduplicate overlapping prefixes, and apply exclusions. Logical metadata survives payload expiry.

Branches name versions without copying contents. Tags protect versions. Reference changes are serialized; deletion checks the expected target, and new targets require retained contents.

Merges compare complete logical trees, follow both parents for ancestry, and require an explicit choice among ambiguous best bases. Whole-file/subtree choices reuse native objects from the selected parents. Published merges record both parents; merging into an empty branch records only the source parent. Already-merged sources create no version.

## Publication

1. The workspace durably records an operation ID, expected branch tip, changes, and file size/SHA-256 identities.
2. The service binds the operation ID to the request fingerprint and checks the branch tip.
3. Kopia captures changed bytes, reuses omitted contents, and flushes a protected complete snapshot labelled with the operation ID.
4. One SQLite transaction records the version, conditionally advances the branch, and records acceptance. The operation ID is also the version ID.
5. The workspace advances its selected baseline without rewriting local files.

Merge uses this same publication path with immutable parents and explicit choices; it uploads no contents. The workspace remains unchanged until a separate update.

Retries resolve the original outcome. Accepted, stale, unchanged, and cancelled decisions remain stable; an ID cannot be reused for another intent. Cancellation also prevents a request that has not arrived yet. Startup checks retained protection and releases unaccepted or expired roots. It never promotes an unfinished capture into history. Uncertain SQLite acceptance leaves protection intact until the outcome is resolved.

## Local transitions

Clone, update, switch, selection changes, and restore share one planner/executor/canceller. Comparison uses baseline, observed, and target entries. Independent edits survive; conflicts require whole-entry choices. Dirty unloading and unmatched explicit restore paths fail before mutation. Nonempty selection changes cannot empty a populated workspace without matching any remote or local entries.

The immutable plan stores the target baseline, ordered before/after changes, and a historical content source when it differs from the baseline. A separate small progress record stores the cursor and cancellation direction. Local process locking excludes concurrent workspace commands.

Each action verifies staged bytes, preserves the previous entry, and installs without replacing an unexpected path. Children leave before parent type changes; new parents arrive before children. Physical unselected contents remain protected, and directory removal requires emptiness.

Progress is durable before the plan appears. The cursor advances only after the action is synchronized. Reaching the end durably commits the transition; the target baseline is installed before recovery contents are cleaned up. Cleanup removes the plan before progress. Orphan progress is replaced before a new plan.

Before commitment, cancellation reverses actions and restores originals without replacing newer edits. Preserved/staged entries identify interrupted actions. Unexpected recovery contents remain available and are reported. After commitment, resume finishes cleanup. Initial clone starts from a valid empty-selection workspace; metadata-only changes need only an atomic baseline update.

Hashing observes cancellation between reads; mutation boundaries preserve synchronization ordering. External writers must stop during these operations. Save recovery resolves its original request, leaving later file edits visible against the accepted baseline.

## Retention and collection

Cleanup protects branch tips, tags, and the requested recent first-parent history. Its preview token binds the count, version identities, and references. Apply recomputes the decision under the exclusive project gate. Availability is excluded from the token, making repeated expiry safe while history and references match.

One SQLite transaction clears eligible commit root references. An empty root is the payload-expiry fact; parents, comparison metadata, and operation outcomes remain. Reconciliation releases native manifests only after that decision. Interruption may leave extra protection, never permission to remove promised contents.

Reads check availability and hold shared protection for their entire stream. Publication, references, expiry, and collection take the exclusive gate. Merges need retained parent contents, but can compare an expired ancestor's metadata. Idle clients hold no retention lease.

Kopia's ordinary full maintenance owns reachability, rewriting, deletion, and maintenance state, with standard safety delays. Lode measures repository bytes before and after collection; it maintains no parallel pack ledger or scheduler.

## IO boundaries

A project ownership lock excludes a second service. SQLite transactions and synchronized workspace state files record application decisions. Kopia synchronizes blob contents; the filesystem adapter also synchronizes affected directory chains. These contracts require local filesystems that honor synchronization.

HTTP requires a bearer token and expected project identity. Counted newline-delimited metadata records let clients detect truncated lists while bounding each record. Cleanup exposes its apply token only after complete validation. Selected inventories become workspace baseline metadata; native indexes stay on the server.

Upload signatures are fetched for exact retained base paths before publication and spooled to a bounded anonymous file. Under the publication gate, librsync reconstructs into the same size/hash-verified capture path as full uploads. Downloads optionally use a local basis and reconstruct into the transition's existing staging file, then verify before installation. Retries regenerate signatures; neither direction adds durable transfer state.

Backup holds the project ownership lock, closes database/content writers, copies identity/SQLite/native files while preserving maintenance timestamps, and excludes derived configuration/caches. It reopens the copy to check SQLite and retained roots before a synchronized no-replace rename publishes the destination. Restore uses the same copy operation.

See the [user guide](guide.md) for command behavior and [operations](operations.md) for deployment and resource limits.
