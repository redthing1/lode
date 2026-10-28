# Operations

Lode hosts multiple accounts and projects in one instance. The server and supported CLI build run on Linux. Use a case-sensitive local filesystem that supports file and directory synchronization and atomic rename without replacement. Build and runtime requirements are in the [README](../README.md#install).

## Host an instance

Initialize an instance with its public HTTPS origin, then start the loopback-only server:

```sh
lode init -url https://lode.example.com ./instance
lode serve -listen 127.0.0.1:8473 ./instance
```

`init` prints a one-time setup code. Open the displayed `/setup` URL, create the first administrator, then create projects and accounts in the web interface. For local evaluation, `http://localhost:8473` is accepted without TLS. Use HTTPS for remote access; browsers allow WebAuthn on localhost for local evaluation.

Put a TLS reverse proxy on the host. For example, a [Caddy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy) site can use:

```caddyfile
lode.example.com {
    reverse_proxy 127.0.0.1:8473
}
```

Point DNS at the host and allow ports 80 and 443 for automatic certificate management. Proxy the origin without rewriting paths and preserve the request `Host` and `Authorization` headers. The configured hostname must match the hostname users open in their browsers. Keep the proxy's certificate data persistent.

The server only listens on loopback. Do not expose its listener directly to a network. Browser sessions use same-origin cookies; the interface rejects cross-origin form requests. Keep the instance directory private and outside project workspaces.

## Accounts and recovery

The first account is an instance administrator. Administrators create more accounts; there is no public registration. Each new account receives a temporary password that must be changed at first sign-in. Administrators can disable accounts and change their administrator status. Account password recovery is a local operator action described below.

Users can register up to ten FIDO2 credentials for passwordless sign-in, with or without entering a username. Password sign-in uses a registered key as a second factor; single-use recovery codes provide a fallback. Removing the final key disables the second factor. Recovery codes are shown only when generated.

For local operator recovery, stop the server and run:

```sh
lode recover ./instance USERNAME
```

The command prints a generated temporary password, invalidates browser sessions and personal tokens, removes registered security keys and recovery codes, and requires a password change at next sign-in. It does not change the account's role or enabled state. Protect the terminal output as a credential.

## Run with Podman Compose

Install Podman and a Compose provider such as [podman-compose](https://docs.podman.io/en/latest/markdown/podman-compose.1.html). Build the image and initialize the named data volume. Use your public HTTPS origin when hosting remotely:

```sh
podman compose build
podman compose run --rm lode init -url https://lode.example.com /data/instance
podman compose up -d
```

For local-only evaluation, use `-url http://localhost:8473`. Open the setup URL printed by `init` and create the first administrator. The service stores the complete instance under `/data/instance`, uses host networking, and binds `127.0.0.1:8473`; it publishes no network port. Host networking also lets the container reach host loopback services. Put the host's TLS proxy in front for remote access.

`podman compose restart` restarts the service, `podman compose logs` shows its output, and `podman compose down` removes the service while retaining the named volume. For a cold backup, stop the server and copy the backup to a host directory:

```sh
podman compose stop
mkdir -p ./backups
podman compose run --rm -v "$PWD/backups:/backups:Z" lode backup /data/instance /backups/instance-backup
podman compose down
```

Restore into a separate Compose project so the original volume remains available:

```sh
podman compose -p lode-restored run --rm -v "$PWD/backups:/backups:Z" lode backup /backups/instance-backup /data/instance
podman compose -p lode-restored up -d
```

Stop the original service before starting the restored one because both use the same loopback address. The restored instance has the same identity, accounts, tokens, projects, and configured URL; do not run both copies at once.

## Back up, restore, and import

Stop the server before backing up. The source must be a stopped instance; the destination must be new and outside the source:

```sh
lode backup ./instance ./instance-backup
```

The backup contains the instance identity, accounts, memberships, personal tokens, security keys, recovery codes, browser sessions, and every registered project. Treat it as sensitive as the live instance. To restore, copy it into another new directory and serve that copy:

```sh
lode backup ./instance-backup ./restored-instance
lode serve ./restored-instance
```

Run only one copy at a time. Existing destinations are never replaced. Backup verifies the database and projects before publishing the copy. It does not hash every content payload. A failure before publication removes its owned staging directory; the final destination appears only after verification.

To bring a legacy single-project store into a new instance, stop both servers and import the project under a new project slug:

```sh
lode import ./instance example ./old-project-store
```

Import preserves the project's immutable ID and history. The old shared project token is not accepted by the instance; create accounts, memberships, and new personal tokens in the web interface. Clone the imported project into a new workspace. To carry unsaved edits forward, copy them into that workspace, review `lode status`, then save them. Resolve or cancel pending operations in the old workspace before moving the project.

## Storage and transfer limits

- Allow space for native caches and temporary rewritten packs during saves and collection. Collection blocks other project operations and reports physical repository blob bytes before and after, excluding caches. Native safety delays and packing overhead prevent an immediate-reclamation guarantee.
- Instance backup needs room for a complete physical copy, including obsolete bytes awaiting collection. A local file transition needs room for preserved replaced files plus one incoming file; completed staged files share their installed inode.
- Logical history and operation outcomes remain unpruned. Captures examine the complete logical tree; browser directory pages return at most 100 entries and history pages at most 50 versions. The server keeps at most eight native project stores open and closes idle stores when needed.
- Clients keep selected baseline metadata and requested files, without a native storage index or historical payloads. JSON requests and individual metadata records are limited to 16 MiB; lists stream without an aggregate response limit.
- Eligible changed files of at least 1 MiB can transfer as librsync deltas. Initial checkout and files without a suitable basis transfer in full. Bases above about 28 GiB use full transfer. Upload signatures use at most 64 MiB of temporary client disk space per save; a download holds at most one 1 MiB signature.
- Delta transfer still reads complete source files; unrelated rewrites may save no bandwidth. Updates can reread local files for observation, matching, and recovery verification. Incoming contents are verified before installation.

An active download protects its source until the stream finishes. Idle workspaces do not pin server contents indefinitely, and a multi-file transition can encounter expiry between downloads. Use the workspace's normal [recovery commands](guide.md#recover-interrupted-work) if it stops.
