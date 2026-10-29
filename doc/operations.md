# Running Lode

The server runs on Linux and stores its database and project repositories in one private instance directory. Use a local filesystem that honors file and directory synchronization. Access repositories only through Lode.

## Podman Compose

Install Podman and a Compose provider such as `podman-compose`. From the source checkout:

```sh
podman compose build
podman compose run --rm lode init --url https://lode.example /data/instance
podman compose up -d
```

Keep the setup code printed by `init`. Put an HTTPS reverse proxy in front of `127.0.0.1:8473`, preserving the original Host header and request path. Allow uploads as large as your project files and sufficient request time for transfers and collection. Open the configured URL, enter the setup code, and create the first administrator.

The Compose service uses host networking and the `lode-data` named volume. For local evaluation, omit `--url` and open `http://localhost:8473`. The initialization command's `--listen` flag selects another address if needed.

```sh
podman compose logs -f lode
podman compose stop
podman compose up -d
```

`/healthz` returns 204 at the canonical hostname when the HTTP service is running. It does not verify repository contents. `podman compose down` keeps the named volume; `down -v` deletes it.

Without containers, use `lode init --url https://lode.example ./instance` and `lode serve ./instance`.

## Configuration and accounts

Initialization writes `config.json` inside the instance:

```json
{
  "url": "https://lode.example",
  "listen": "127.0.0.1:8473"
}
```

Stop the server before editing it. `url` is the browser and CLI origin; HTTPS is required except for local development at `localhost`. `listen` is the HTTP IP address and port behind the proxy. Unknown configuration fields are refused.

Changing the hostname affects saved CLI credentials and passkeys, which are tied to the original server identity and hostname. Keep the same canonical URL when restoring an instance.

Public registration is closed. Administrators create accounts in **Site administration → Users**; users create and share their own projects. Account security, token scope, and recovery are described in the [user guide](guide.md#account-security).

For local account recovery, stop the service and run:

```sh
podman compose stop
podman compose run --rm lode recover-user /data/instance alice
podman compose up -d
```

## Backup and restore

`lode backup SOURCE DEST` copies a stopped whole instance into a new directory. It preserves identity, accounts, credentials, permissions, history and native repositories. Verification checks the staged database and project trees and reads retained file contents against their recorded size and hash. Path-specific connection files are rebuilt when needed.

The destination must not exist and must be outside the source. Allow space for a complete repository copy. The source must remain stopped for the entire copy and verification. Unfinished project creation or deletion must be resolved before backup.

```sh
lode backup ./instance ./backup-2026-10-28
lode backup ./backup-2026-10-28 ./restored-instance
lode serve ./restored-instance
```

With Compose, mount a private backup directory outside the data volume:

```sh
mkdir -m 700 backups
podman compose stop
podman compose run --rm -v "$PWD/backups:/backups:Z" lode \
  backup /data/instance /backups/backup-2026-10-28
podman compose up -d
```

Restore into a separate Compose project's new volume. Stop the original service before starting the restored one at the same address:

```sh
podman compose stop
podman compose -p lode-restored run --rm -v "$PWD/backups:/backups:Z" lode \
  backup /backups/backup-2026-10-28 /data/instance
podman compose -p lode-restored up -d
```

Keep backups private: they contain the instance secret and account credentials. A failed copy does not publish its destination. Killing the process can leave a private `.lode-backup-*` staging directory beside the destination; remove that incomplete directory after confirming no backup is running. If the command reports that publication succeeded but parent synchronization failed, inspect the published destination before retrying.

Restore a backup periodically and check sign-in and representative file downloads. Verification cannot guarantee the backup's future storage health.

## Storage and maintenance

Project cleanup expires contents and prunes eligible history; collection subsequently reclaims native storage. Both are explicit operations available in project settings and the CLI. Tags and branch tips retain their complete contents. See [retention](guide.md#protect-versions-and-clean-up) for the policy and its effect on older merge ancestry.

Collection waits for readers and excludes other operations on that project. Pack rewriting needs temporary free space; reported repository allocation excludes SQLite and caches. Merge currently holds complete comparison metadata in memory. Changed large files use delta transfers when a basis is available; hashing and reconstruction still read local contents. Unrelated rewrites can transfer approximately the whole file plus a bounded signature.

Back up before replacing the executable or container image. This experimental release does not provide development-format migrations.
