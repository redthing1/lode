# Server operations

The Linux server keeps configuration and SQLite state separate from bulk project contents. Compose uses ordinary host directories, so the deployment is easy to inspect, back up, and move.

## Start with Podman Compose

Install Podman and a Compose provider such as `podman-compose`. From the source checkout, the defaults store state in `./data` and project contents in `./storage`:

```sh
podman compose build
podman compose run --rm lode init --url https://lode.example /data
podman compose up -d
```

To finish setup:

1. Keep the setup code printed by `init`.
2. Put an HTTPS reverse proxy in front of `127.0.0.1:8473`. Preserve the Host header and request path; allow your largest uploads and enough time for transfers and collection.
3. Open your configured URL and enter the setup code to create the first administrator.

For local evaluation, omit `--url` and open `http://localhost:8473`. To run without containers, follow the [local setup](../README.md#try-it-locally), or initialize with `lode init --url https://lode.example ./instance` for HTTPS hosting.

### Start, stop, and inspect

```sh
podman compose logs -f lode
podman compose stop
podman compose up -d
```

Compose uses host networking. `podman compose down` removes containers and leaves the host directories intact.

`GET /healthz` returns 204 at the configured hostname while the HTTP service is running. This is a liveness check, not a repository integrity check.

## Data locations

| Host path (defaults) | Contents |
| --- | --- |
| `data/state/config.json` | Server URL and listen address |
| `data/state/instance.sqlite` | Accounts, permissions, version metadata, and instance identity |
| `storage/` | Bulk project contents and their indexes |

Keep `data/` on a fast local disk. SQLite grows with project history, but `storage/` holds the large binary data. To put it on an HDD or RAID mount, copy `.env.example` to `.env` and set:

```dotenv
LODE_DATA_DIR=./data
LODE_STORAGE_DIR=/mnt/bulk/lode
```

Choose the paths before initialization, with the bulk filesystem mounted and writable by the account running Podman. Use a local filesystem that honors file and directory synchronization. Mount a dedicated directory; only Lode should access its contents.

Inside the container, the instance is `/data`: `state/` contains metadata and the storage directory is mounted at `projects/`. Lode checks the storage identity at startup and refuses a missing or mismatched volume. Keep its hidden `.instance` marker intact.

To move an existing deployment, stop the server and move both directories together, or use backup and restore below. Changing a path in `.env` does not move its contents.

## Configuration

Edit `data/state/config.json` (under your configured data directory):

```json
{
  "url": "https://lode.example",
  "listen": "127.0.0.1:8473"
}
```

- `url` is the browser and CLI origin. Use HTTPS with a hostname; HTTP at `localhost` is allowed for local development.
- `listen` is the server's HTTP address behind the proxy. Set it during initialization with `--listen`, if needed.
- `trusted_proxies` is an optional list of IP prefixes, such as `["127.0.0.1/32", "::1/128"]`. Only listed peers may supply `X-Forwarded-For` for per-client sign-in limits. List only your proxy addresses; the default trusts none and limits by the direct peer.

Stop the server before editing this file. Unknown fields are refused.

Keep the canonical URL when restoring an instance. Changing it affects saved CLI credentials, and passkeys are tied to the hostname where they were enrolled.

## Accounts and recovery

Public registration is closed. Administrators create accounts under **Site administration → Users**; users create and share their own projects. See [sharing and access](guide.md#sharing-and-access) for roles and token scope.

All projects are private. Anonymous requests can access sign-in, static assets, health, and initial setup with its one-use code. Project pages, file contents, the client API, and administration require authentication and current permissions. Personal tokens do not inherit administrator authority.

Administrators can reset passwords, change administrator access, and disable accounts. These changes end the affected account's sessions and revoke its tokens. Disabling an account preserves its projects; re-enabling it requires fresh sign-in. The last enabled administrator cannot be disabled or demoted.

For account recovery without browser access, stop the server and set a new password:

```sh
podman compose stop
podman compose run --rm lode recover-user /data alice
podman compose up -d
```

Without Compose, run `lode recover-user ./instance alice` while the server is stopped. The command enables the account and invalidates its sessions, tokens, keys, and recovery codes. Its role, projects, and memberships remain intact. Interactive use prompts for the password; scripts may supply one line on stdin.

## Backup and restore

`lode backup SOURCE DEST` copies and verifies a whole stopped instance. It preserves identity, accounts, credentials, permissions, and project data. Restore uses the same command.

Before starting:

- Stop the source server for the entire copy and verification.
- Finish any pending project creation or deletion.
- Choose a new or empty destination outside the source, with space for a complete copy. An existing empty `projects/` mount is allowed.

### Without containers

```sh
lode backup ./instance ./backup
lode backup ./backup ./restored-instance
lode serve ./restored-instance
```

### With Compose

Mount a private backup directory outside both data directories:

```sh
mkdir -m 700 backups
podman compose stop
podman compose run --rm -v "$PWD/backups:/backups:Z" lode \
  backup /data /backups/backup
podman compose up -d
```

To restore, stop the service and set `LODE_DATA_DIR` and `LODE_STORAGE_DIR` in `.env` to new, empty directories. Then restore and start using those paths:

```sh
podman compose stop
podman compose run --rm -v "$PWD/backups:/backups:Z" lode \
  backup /backups/backup /data
podman compose up -d
```

### Verification and interrupted copies

Backup checks the copied database and project trees and reads retained files against their recorded sizes and hashes. It publishes `state/` only after verification. Restore a backup periodically and check sign-in and representative downloads too.

Backups contain account credentials and the instance secret; keep them private. An interrupted copy can leave partial project contents and a `.lode-backup-*` metadata staging directory inside the destination. Lode refuses to initialize or restore over those contents. Use fresh empty directories to retry; remove partial copies only after confirming no backup is running.

Interrupted initialization can also leave partial files. If `state/` has not been published, use empty data and storage directories for a new attempt.

If an error says the instance was published but directory synchronization failed, inspect that destination before retrying.

## Maintenance and upgrades

Cleanup expires eligible versions and history. Collection then reclaims unused content storage. Both run explicitly through project settings or the CLI; see [storage policy](guide.md#keep-versions-and-reclaim-space).

Collection waits for readers and temporarily excludes other operations on its project. Leave free space for pack rewriting. Large merges can need substantial memory because their comparison metadata is held on the server.

Back up before replacing the executable or container image. Keep the corresponding binary or image with important backups: experimental storage formats can change, and development-format migrations are not provided.
