# Architecture

Lode's design uses one authoritative server and lightweight clients working in ordinary folders. History, retained contents, and local selection are separate concerns.

## Concepts

| Concept | Meaning |
| --- | --- |
| Project | A directory tree and its version history |
| Commit | An immutable complete project state with its parent commits |
| Branch | A name pointing to the current commit on a line of work |
| Workspace | Editable files, a base commit, and a selection of local paths |
| Tag | A name protecting a particular commit's contents |

A partial workspace represents a complete commit. Publishing selected changes preserves unloaded paths. Unloaded content, local deletion, and an incomplete observation have distinct meanings.

Branches share history and stored contents. Merging compares complete server trees against a single best common ancestor. Explicit path choices select whole entries or subtrees, including deletions and type changes. Merges create two-parent commits; unrelated histories and multiple best common ancestors are rejected. Restoring historical files creates local changes without rewriting accepted history.

## Responsibilities

| Component | Responsibility |
| --- | --- |
| Model | Paths, selection, entry comparison, and conflict rules independent of IO |
| Project service | History, branches, publication, merging, and retention decisions |
| Workspace | Local discovery, materialization, and recovery of interrupted operations |
| Storage adapter | Engine-specific capture, retrieval, protection, and collection |

The CLI presents application operations. The project service owns shared state; the workspace owns local state; the storage engine owns content representation. These responsibilities fit within one executable.

The current implementation uses a project-scoped HTTP API, SQLite for history and operation records, and upstream Kopia for stored contents. Clients receive selected entry metadata and requested files; native storage credentials and indexes stay on the server. File size and SHA-256 bind transferred bytes to their intended contents.

Large changed files can stream rsync deltas against the immutable base protected by their prepared publication. Clients request one bounded signature at a time and retain no patch files. The server reconstructs and verifies complete files before acceptance; transport encoding does not change commit or retry identity. Full uploads remain available for files without a usable base and servers without delta support.

## Publication and retention

Publication protects the complete candidate's contents before accepting a branch update. An expected-tip check prevents stale work from silently replacing intervening changes. Retrying an accepted operation returns the same result.

Sparse uploads seal their inputs before capture. Cancellation also reserves an unknown operation ID, so a delayed request cannot publish after its cancellation. Workspace journals precede uploads and initial materialization; accepted commits update the baseline while preserving subsequent local edits.

Update, switch, load, unload, and restore use one local transition coordinator. It compares selected base, local, and target entries before writing, then seals an action plan and checkpoints progress in a small separate record bound to that plan. Verified downloads are staged before replacement; no-replace moves preserve unexpected entries. The new workspace baseline and branch binding become current only when the transition finishes.

Merge intent seals both parent IDs and the complete choice set before composing stored file references on the server. Recovery can rebuild that candidate without a client upload. Unresolved comparisons reserve no publication operation. The client journals the merge request, then replaces it with a selected filesystem transition after acceptance. Later local edits are reconciled independently of the server choices.

Comparison metadata is stored separately from binary payloads so history survives payload expiry. Branch tips, immutable tags, policy-selected versions, a publication grace period, and active operations protect contents. Parent links alone do not imply permanent payload retention. Expiration rechecks those reasons, records irreversible release intent in SQLite, then releases the native root; restart retries unfinished releases. New protection requests cannot resurrect an expired version merely because shared bytes remain.

Materialization holds expire unless renewed by active clients. Native file streams remain protected through close or cancellation, and physical maintenance waits for their readers. Idle workspaces retain their own baseline facts without pinning server payloads.

Physical collection runs separately from expiry. The service reconciles pending publication and release work, then coordinates a bounded native compaction pass and collection cycle. It records the source before copying; retries select only live contents still in that source. Native flush and maintenance scheduling precede the durable completion acknowledgement. Once no live contents remain in the source, the next compaction batch can proceed while Kopia's normal reachability, index, age, and session checks govern eventual deletion.

Capacity admission checks volume free space before new captures and limits compaction copying while retaining an operating reserve for metadata and recovery. Already accepted retries and release work remain usable below the normal admission threshold. Measurements distinguish repository blob lengths, indexed bytes rewritten, and volume-wide free space; estimates and checks do not establish a hard storage quota.

Local project ownership spans opening, service operations, and closing the database and native storage. Offline backup holds the same ownership lock while copying the complete store into a private sibling staging directory. The staged copy is opened and closed for validation before atomic publication at a fresh path. The same operation restores a backup; its identity, credentials, metadata, and retained contents stay together.

An incomplete local materialization can be abandoned without fetching its source. Abandonment is journaled, preserves current working entries, restores verified saved files only into absent paths, and archives remaining staged data with its original path mapping. The previous workspace baseline remains current so partial changes stay visible for reconciliation.

Clients retrieve selected contents and metadata on demand and use bounded caches. Working files are user data. Server cleanup and cache eviction do not discard local edits. Workspace mutations preserve local work or stop with a clear conflict, and incomplete transitions remain identifiable for recovery.
