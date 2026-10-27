# Architecture

This document describes the intended design. The current checkout has no implementation.

## Product model

A project is a versioned directory tree. A commit records one complete logical state and its parents. A branch names a line of work; a tag protects a chosen version. A workspace is an editable folder associated with a base commit and a selection of paths.

Three facts remain separate:

- **History:** the versions and their relationships.
- **Availability:** which versions still have recoverable contents.
- **Local presence:** which paths a workspace currently holds.

A partial workspace represents a complete project version. Publishing selected changes preserves unloaded paths. An unreadable path, an unloaded path, and an intentional deletion have different meanings.

Commits publish to an authoritative server. Branch updates check the expected tip. Merges compare complete project states and use explicit whole-file choices for conflicting opaque contents, including conflicts outside the client's selection.

## Ownership

| Responsibility | Owns |
| --- | --- |
| Model | Path, selection, entry, and comparison rules independent of IO |
| Project service | History, branches, publication, merge decisions, and retention policy |
| Workspace | Local observation, selection, file replacement, and recovery |
| Content adapter | Capture, retrieval, protection, release, and native maintenance |

The CLI presents operations. HTTP carries project requests. Persistence records application state. These are supporting boundaries within one Go executable; packages are introduced for implemented responsibilities.

SQLite is the preferred application store and Kopia the preferred content engine to evaluate. Their representations and APIs must fit the product's lifetime and resource requirements. The design does not require interchangeable storage backends.

## Operation contracts

Publication must protect complete contents before accepting a branch update. Retrying an uncertain request must resolve its original outcome. Workspace replacement must preserve local edits or report a conflict, and interruption must leave enough state to recover.

Retention protects branch tips, named keeps, policy-selected history, and active operations. Ancestry alone does not permanently retain every payload. Expiry releases protection; native collection later reclaims bytes that are no longer needed. History must distinguish expired contents from recoverable versions.

Clients retrieve selected metadata and contents on demand without acquiring repository-wide storage indexes or historical payloads. Transfers stream large files. Metadata growth, repeated local reads, temporary space, and collection overhead are part of the efficiency requirements.

The CLI workflow, storage contract, and recovery model must remain understandable together. Wire formats, schemas, retention defaults, and optimization mechanisms are implementation decisions to be settled against those contracts.
