# Operations

Lode currently supports one private project per server process on Linux. Use case-sensitive local filesystems that support file and directory synchronization and atomic rename without replacement. Build and runtime requirements are in the [README](../README.md#install).

## Host a project

Initialize a new store and keep its printed access token private:

```sh
lode init ./store
lode serve -listen 127.0.0.1:8473 ./store
```

Only one process can own a store at a time. Run the server under your normal process supervisor for persistent hosting. Keep the store outside working folders and serve it over HTTPS for remote clients; Lode's listener accepts loopback addresses only.

For example, with [Caddy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy) on the same host, a `Caddyfile` can contain:

```caddyfile
lode.example.com {
    reverse_proxy 127.0.0.1:8473
}
```

Replace the hostname with your own domain. Point its DNS records at the host, make ports 80 and 443 reachable, and keep Caddy's data directory persistent and writable for [automatic HTTPS](https://caddyserver.com/docs/automatic-https). Run Caddy through its service installation or `caddy run --config ./Caddyfile`.

Proxy the root URL without rewriting paths. Preserve request headers, including `Authorization` and `Lode-Project-ID`. Allow large, long-running uploads and downloads; avoid request buffering or limits below your expected transfer sizes. The example uses Caddy's normal header forwarding and adds no request-body size cap.

Clients connect using the HTTPS origin, without a path prefix:

```sh
export LODE_TOKEN='the project access token'
lode clone https://lode.example.com ./work
```

## Run with Podman Compose

The repository includes a Linux container build and Compose service. Install Podman and a Compose provider such as [podman-compose](https://docs.podman.io/en/latest/markdown/podman-compose.1.html), then build the image and initialize the project volume once:

```sh
podman compose build
podman compose run --rm lode init /data/project
```

Save the printed access token privately. Start the server with:

```sh
podman compose up -d
```

The service keeps the project in the named `lode-data` volume and binds `127.0.0.1:8473` in the host network namespace. It publishes no ports. Use a TLS reverse proxy on the host for remote clients, as described above. Host networking is required because Lode accepts loopback listeners only; the container can reach services bound to host loopback. `podman compose` uses an external Compose provider, which must be installed separately.

Restart with `podman compose restart`; view server output with `podman compose logs`. `podman compose down` stops and removes the service while preserving its named volume. For a cold backup, stop the service and copy the verified backup out to a host directory:

```sh
podman compose stop
mkdir -p ./backups
podman compose run --rm -v "$PWD/backups:/backups:Z" lode backup /data/project /backups/project-backup
podman compose down
```

To restore that copy into a fresh volume while keeping the original volume available, use a new Compose project name:

```sh
podman compose -p lode-restored run --rm -v "$PWD/backups:/backups:Z" lode backup /backups/project-backup /data/project
podman compose -p lode-restored up -d
```

The restored service binds the same loopback address, so stop the original service before starting it. Use the restored Compose project name for later commands.

## Credentials and files

The project has one shared access token; fine-grained users and permissions are not implemented. `attach` and `clone` read `LODE_TOKEN` and store credentials in the workspace's `.lode` directory. Protect both the store and workspace state. Backups include project identity and credentials. Client connections require valid TLS certificates; plaintext HTTP is restricted to loopback.

Lode versions ordinary file contents and directories. Paths must be UTF-8 without control characters. Symlinks, special files, permissions, and timestamps are not versioned. `.lode` is reserved; other selected files are included.

Stop applications writing affected files while saving or changing local presence. Lode locks out concurrent commands in a workspace, but cannot take a simultaneous snapshot of another application's writes.

## Back up and restore

Stop the server with Ctrl-C in its terminal or through its process supervisor, then copy the complete store into a new directory:

```sh
lode backup ./store ./project-backup
```

The source must be stopped and writable. The destination must not exist or lie inside the source. The copy contains history, references, retention decisions, operation outcomes, credentials, and retained contents. Derived native caches and connection configuration are rebuilt when opened.

To restore, make a working copy of the backup and serve it:

```sh
lode backup ./project-backup ./restored-store
lode serve ./restored-store
```

Copies are independent stores, not synchronized replicas. Point existing clients at the restored service's original address to keep using their stored connection settings.

Backup checks SQLite consistency and retained snapshot roots before publishing the destination. It does not hash every payload. An abrupt interruption can leave a sibling `.DEST.partial` directory; the final destination appears only after copying and verification. Remove the unfinished staging directory before retrying. Existing destinations are never replaced.

## Storage and transfer limits

- Allow space for native caches and temporary rewritten packs during saves and collection. Collection blocks other project operations and reports physical repository blob bytes before/after, excluding caches. Native safety delays and packing overhead prevent an immediate-reclamation guarantee.
- Backup needs room for a complete physical copy, including obsolete bytes awaiting collection. A local file transition needs room for preserved replaced files plus one incoming file; completed staged files share their installed inode.
- Logical history and operation outcomes remain unpruned. Captures examine the complete logical tree; cleanup enumerates history on the server.
- Clients keep selected baseline metadata and requested files, without a native storage index or historical payloads. JSON requests and individual metadata records are limited to 16 MiB; lists stream without an aggregate response limit.
- Eligible changed files of at least 1 MiB can transfer as librsync deltas. Initial checkout and files without a suitable basis transfer in full. Bases above about 28 GiB use full transfer. Upload signatures use at most 64 MiB of temporary client disk space per save; a download holds at most one 1 MiB signature.
- Delta transfer still reads complete source files; unrelated rewrites may save no bandwidth. Updates can reread local files for observation, matching, and recovery verification. Incoming contents are verified before installation.

An active download protects its source until the stream finishes. Idle workspaces do not pin server contents indefinitely, and a multi-file transition can encounter expiry between streams. Use the workspace's normal [recovery commands](guide.md#recover-interrupted-work) if it stops.
