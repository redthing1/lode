# Lode

Lode versions shared file projects using ordinary local folders. Download selected paths, save complete project versions, and browse files and history in its embedded web interface.

Lode is experimental. Selected workspaces support saves, updates, branches, merges, restore, and explicit whole-file conflict choices.

## Install

From a source checkout, use Go 1.27.2 or later, a C compiler, and the librsync development library. The installed command also needs the librsync shared library.

```sh
GOBIN="$HOME/.local/bin" go install ./cmd/lode
export PATH="$HOME/.local/bin:$PATH"
lode version
```

## Start a server

The server runs on Linux:

```sh
lode init ./instance
lode serve ./instance
```

Open `http://localhost:8473`, enter the printed setup code, and create the administrator account. Create a project, then create an access token under **Settings → Access tokens**.

For remote access, initialize with `-url https://lode.example` and put an HTTPS reverse proxy in front of the listen address. The canonical URL requires a hostname; local development uses `localhost`.

Administrators add accounts under **Site administration → Users**. Each user can create private projects and share them through **Project settings → Collaborators**.

Connect an existing folder to the empty project:

```sh
lode login http://localhost:8473
lode attach http://localhost:8473/alice/example ./work
cd ./work
lode save -m 'Initial version'
```

See the [user guide](doc/guide.md) for selected paths and recovery, [operations](doc/operations.md) for Podman Compose and backup, and [architecture](doc/architecture.md) for persistence and service boundaries.

## Development

Run checks locally:

```sh
make fmt
make check
make build
```
