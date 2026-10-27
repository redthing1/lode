# Architecture

Lode is one Go module and executable. The server owns shared history and content protection. A workspace owns selected local files, its baseline, and any unfinished operation.

| Package | Responsibility |
| --- | --- |
| `model` | Pure path, selection, entry, and tree-change rules |
| `project` | Publication decisions, SQLite history, and branch authority |
| `content` | Kopia objects, complete snapshot roots, retrieval, and release |
| `remote` | Authenticated HTTP requests and streamed file transfers |
| `workspace` | Local observation, selected checkout, baseline, and recovery |
| `cmd/lode` | CLI and server lifecycle |

These are concrete components. The project service owns decisions; the content adapter supplies native storage operations.

## Versions and selection

A version describes a complete directory tree. A workspace's selection controls local presence, not the completeness of its saved version. Saving exact path changes preserves omitted entries. Removing a directory cannot implicitly delete unseen descendants. Unreadable selected paths fail observation rather than becoming deletions.

SQLite stores per-path changes with a complete checkpoint every 64 first-parent steps. Selected reads query indexed path ranges and replay at most 63 changesets. Native object references remain on the server. Kopia represents and protects the complete content tree; it does not decide whether a branch may advance.

The current service has one branch and retains every accepted version. Logical history, content availability, and workspace presence remain separate concepts.

## Publication

1. The workspace records an operation ID, expected branch tip, changes, and file size/SHA-256 identities before uploading.
2. The service durably binds the operation ID to that request's fingerprint and checks the branch tip.
3. Kopia captures changed contents, reuses omitted contents, and flushes a protected complete snapshot tagged with the operation ID.
4. One SQLite transaction records the version, advances the branch conditionally, and records acceptance. The operation ID is also the version ID.
5. The workspace updates its selected baseline without rewriting local files.

Retries resolve the original outcome. Accepted, stale, and cancelled operations keep their decisions. Reusing an operation ID for another intent fails. Cancellation also blocks a request that has not yet arrived.

Startup checks accepted snapshot protection and releases unaccepted candidates. It never promotes an unfinished capture into history. An uncertain SQLite acceptance error leaves protection intact until the database outcome is resolved.

## Local recovery

A workspace holds one unfinished save or checkout. Local process locking prevents concurrent commands from changing its state. State files are synchronized and replaced atomically.

Initial checkout records a selected target before installing files. Each file is streamed to staging, checked against its size and hash, synchronized, and installed without replacing an existing path. Resume infers progress from installed contents. The baseline becomes current only after installation completes. Unexpected local files are preserved and reported as conflicts.

Save recovery uses the original request. Later file edits remain changes against the accepted baseline.

## Storage and transport

The service serializes mutations. Reads hold shared protection while streaming. A local ownership lock excludes a second service process. SQLite uses durable transactions. The filesystem content adapter synchronizes Kopia's blob directory chain after writes and deletions; Kopia synchronizes blob file contents before rename. These contracts require Linux local filesystems that honor synchronization.

The server directory contains project identity and secrets, SQLite metadata, and the native content repository. Native connection configuration and bounded caches live there and are derived from its current location. Clients receive selected metadata and requested file bytes. HTTP requires a project token and expected project identity; plaintext client connections are limited to loopback addresses.
