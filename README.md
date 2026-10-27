# Lode

Lode is a self-hosted version control system for large binary projects. Its design combines branches, partial workspaces, and explicit control over retained history.

Lode is in early development. The CLI supports local project hosting, branches, selected workspaces, whole-file commits and merges, history, restoration, tags, explicit retention, and storage collection.

See [the architecture overview](doc/architecture.md) for the core concepts.

## Build

Requires Go as declared in [go.mod](go.mod), or a newer compatible toolchain, and Make. The current server and workspace implementation requires Linux and a local filesystem that supports file and directory synchronization.

```sh
make build
./bin/lode --help
./bin/lode version
```

The executable is written to `bin/lode`. Set `VERSION` to label a build:

```sh
make build VERSION=0.1.0-dev
```

## Try it locally

Initialize a project store and start its server:

```sh
./bin/lode init project-store
./bin/lode serve project-store
```

In another terminal, attach a working folder to the empty project and record its files:

```sh
mkdir work
printf 'first version\n' > work/example.bin
./bin/lode attach --token-file project-store/token http://127.0.0.1:8021 work
./bin/lode status -C work
./bin/lode commit -C work --author Example -m 'Initial version'
```

Create a workspace containing only a selected path:

```sh
./bin/lode clone --token-file project-store/token --select example.bin \
  http://127.0.0.1:8021 selected-work
./bin/lode log -C selected-work
```

Repeat `--select` to include additional paths; omitting it selects the whole project. Commits preserve unselected server contents and reject a stale branch base. Working folders store selected files and local metadata, without a repository-wide content cache.

Update a workspace, manage branches, or change its selection:

```sh
./bin/lode update -C selected-work
./bin/lode branch create -C selected-work experiment
./bin/lode switch -C selected-work experiment
./bin/lode unload -C selected-work example.bin
./bin/lode load -C selected-work example.bin
```

Update and switch preserve unrelated local edits and stop on conflicting changes. Unload removes only clean controlled copies. `lode restore -C FOLDER --from COMMIT PATH` restores selected historical paths as local changes against the current base. Use `.` to address the selected project tree in load, unload, and restore. Close applications writing affected files before changing their materialization.

After recording different versions on the branches, merge from a clean, current workspace:

```sh
./bin/lode merge -C selected-work --preview main
./bin/lode merge -C selected-work --author Example -m 'Merge main' main
```

The server compares the complete project, including unloaded paths. Resolve conflicts with repeated `--ours PATH` or `--theirs PATH` options; each chooses that side's entire entry or subtree, and a more specific path overrides an ancestor choice. For a merge, these sides are the current branch and the source branch. Only selected result files are downloaded. Use `--from COMMIT` instead of a branch name to pin a source explicitly.

Update, switch, and restore also accept these choices: `--ours` keeps local contents and `--theirs` takes the target contents. Their choices affect selected paths; restore further restricts them to its requested paths. `.` chooses the applicable tree. Each merge records two parents; identical tips, unrelated histories, and histories with multiple best common ancestors are rejected.

Use `lode resume -C FOLDER` after an interrupted operation. For an uncertain commit or merge, `lode abort -C FOLDER` resolves the server outcome before clearing local operation state. An accepted operation is completed locally. If newer local edits conflict with an accepted merge before materialization starts, resume stops; use `resume --ours PATH` or `resume --theirs PATH` to resolve those local choices separately.

Protect important versions and inspect retention:

```sh
./bin/lode tag create -C selected-work release-1
./bin/lode retention show -C selected-work
./bin/lode retention preview -C selected-work
./bin/lode retention apply -C selected-work
./bin/lode storage status -C selected-work
./bin/lode storage collect -C selected-work
```

The default policy keeps the latest 10 first-parent commits per branch and commits created in the last 7 days. Branch tips, tags, active operations, and a 24-hour publication grace period also protect contents. Change the history rules with `retention set --last N --days N`; zero disables that rule. `retention why COMMIT` explains a version's current protection.

`retention apply` rechecks current protections and expires up to 256 eligible versions (`--limit` accepts 1–1000). Expiry preserves history and comparison metadata, releases storage protection, and prevents new content reads or tags for those versions. Shared bytes may remain until physical collection. `log` distinguishes retained and expired versions; storage status reports repository blob lengths separately from shared volume free space.

`storage collect` runs a collection cycle and resumes interrupted compaction. It preserves protected contents and reports measured storage before and after the run. Native safety delays can require repeated runs over several days; the reported next-run time is a suggestion. Each call can compact one source pack, with a default budget of 64 MiB of indexed contents (`--max-rewrite-bytes`, maximum 1 GiB). Pack inspection and index maintenance are outside that copy budget; inspection can read an entire pack. New candidates require obsolete contents totaling at least one third of their live contents and exceeding the pack's original format overhead.

`storage capacity` shows the free-space reserve, initially 1 GiB; set it with `--reserve-bytes N`. New captures require the reserve plus a conservative estimate of twice the changed-file bytes, checked before each file. Collection limits copying to available headroom and can continue native cleanup without copying. These checks provide operating headroom; they do not impose a hard project quota or reserve space against other applications.

Active materialization renews a bounded read hold. Idle workspaces do not keep their old server contents indefinitely: local files remain editable, and update uses the stored baseline to reconcile with a retained target. To load more paths from an expired workspace base, update to a retained version first.

Use `abort` to stop an incomplete checkout or workspace transition, including when its source has expired. It keeps current files and the previous baseline, restores saved old files only into empty paths, and preserves remaining recovery data under the printed `.lode/recovery-...` directory. Completed file changes are not undone; review `status` before continuing. Recovery directories are user data and are not automatically removed.

Stop the server and any other process using a store before making a complete backup:

```sh
./bin/lode backup project-store project-backup
./bin/lode backup project-backup restored-store
```

Each destination must be a new directory. The copy includes history, retained contents, and credentials, and is validated before publication. Restoring preserves the project identity and access token. An interrupted copy can leave a private `.lode-backup-*` staging directory beside the destination; remove that incomplete copy only after its process has stopped.

The server listens on loopback HTTP. Remote access can use an SSH tunnel or an HTTPS reverse proxy. The project token grants access to the entire project; keep it private. The `.lode` directory in each workspace contains its credentials and recovery state.

This implementation tracks regular-file bytes and directories. Symlinks and special files are unsupported; permissions and timestamps are not versioned. Commit a coherent application save: Lode does not coordinate writes by other applications. Place options before positional arguments; use `lode COMMAND --help` for command options.

## Development

```sh
make fmt
make check
```

`make check` verifies formatting and runs `go vet` and `go test`. `make fmt` formats the Go sources.
