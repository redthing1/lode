# Server operations

The Linux server keeps its database and project repositories in one private instance directory. Use a local filesystem that honors file and directory synchronization, and access repositories only through Lode.

## Start with Podman Compose

Install Podman and a Compose provider such as `podman-compose`. From the source checkout:

```sh
podman compose build
podman compose run --rm lode init --url https://lode.example /data/instance
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

Compose uses host networking and the `lode-data` named volume. `podman compose down` keeps the volume; **`down -v` deletes it**.

`GET /healthz` returns 204 at the configured hostname while the HTTP service is running. This is a liveness check, not a repository integrity check.

## Configuration

Initialization writes `config.json` inside the instance directory:

```json
{
  "url": "https://lode.example",
  "listen": "127.0.0.1:8473"
}
```

- `url` is the browser and CLI origin. Use HTTPS with a hostname; HTTP at `localhost` is allowed for local development.
- `listen` is the server's HTTP address behind the proxy. Set it during initialization with `--listen`, if needed.

Stop the server before editing this file. Unknown fields are refused.

Keep the canonical URL when restoring an instance. Changing it affects saved CLI credentials, and passkeys are tied to the hostname where they were enrolled.

## Accounts and recovery

Public registration is closed. Administrators create accounts under **Site administration → Users**; users create and share their own projects. See [sharing and access](guide.md#sharing-and-access) for roles and token scope.

Administrators can reset passwords, change administrator access, and disable accounts. These changes end the affected account's sessions and revoke its tokens. Disabling an account preserves its projects; re-enabling it requires fresh sign-in. The last enabled administrator cannot be disabled or demoted.

For account recovery without browser access, stop the server and set a new password:

```sh
podman compose stop
podman compose run --rm lode recover-user /data/instance alice
podman compose up -d
```

Without Compose, run `lode recover-user ./instance alice` while the server is stopped. The command enables the account and invalidates its sessions, tokens, keys, and recovery codes. Its role, projects, and memberships remain intact. Interactive use prompts for the password; scripts may supply one line on stdin.

## Backup and restore

`lode backup SOURCE DEST` copies and verifies a whole stopped instance. It preserves identity, accounts, credentials, permissions, and project data. Restore uses the same command.

Before starting:

- Stop the source server for the entire copy and verification.
- Finish any pending project creation or deletion.
- Choose a destination that does not exist, outside the source, with space for a complete copy.

### Without containers

```sh
lode backup ./instance ./backup
lode backup ./backup ./restored-instance
lode serve ./restored-instance
```

### With Compose

Mount a private backup directory outside the data volume:

```sh
mkdir -m 700 backups
podman compose stop
podman compose run --rm -v "$PWD/backups:/backups:Z" lode \
  backup /data/instance /backups/backup
podman compose up -d
```

Restore into a separate Compose project's new volume. Keep the original service stopped before starting the restored one at the same address:

```sh
podman compose stop
podman compose -p lode-restored run --rm -v "$PWD/backups:/backups:Z" lode \
  backup /backups/backup /data/instance
podman compose -p lode-restored up -d
```

### Verification and interrupted backups

Before publishing the destination, backup checks the copied database and project trees and reads retained files against their recorded sizes and hashes. Restore a backup periodically and check sign-in and representative downloads too.

Backups contain account credentials and the instance secret; keep them private. If a process is killed, it may leave a private `.lode-backup-*` staging directory beside the destination. Remove that incomplete directory only after confirming no backup is running.

A failed copy does not publish its destination. If an error says publication succeeded but parent synchronization failed, inspect the published destination before retrying.

## Maintenance and upgrades

Cleanup expires eligible versions and history. Collection then reclaims unused content storage. Both run explicitly through project settings or the CLI; see [storage policy](guide.md#keep-versions-and-reclaim-space).

Collection waits for readers and temporarily excludes other operations on its project. Leave free space for pack rewriting. Large merges can need substantial memory because their comparison metadata is held on the server.

Back up before replacing the executable or container image. Keep the corresponding binary or image with important backups: experimental storage formats can change, and development-format migrations are not provided.
