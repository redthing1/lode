# Building Lode

Use Go 1.26 or later with current security patches, a C compiler, and librsync headers and libraries for your architecture. Keep the shared libraries installed when running Lode.

## Platform setup

| Platform | Native prerequisites |
| --- | --- |
| Fedora | `sudo dnf install gcc librsync-devel` |
| Debian / Ubuntu | `sudo apt install build-essential librsync-dev` |
| macOS | Xcode Command Line Tools (`xcode-select --install`) and `brew install librsync` |
| Windows | [MSYS2](https://www.msys2.org/), using its UCRT64 shell: `pacman -S mingw-w64-ucrt-x86_64-gcc mingw-w64-ucrt-x86_64-librsync` |

For Homebrew, expose the library paths before building:

```sh
export CGO_CPPFLAGS="-I$(brew --prefix librsync)/include"
export CGO_LDFLAGS="-L$(brew --prefix librsync)/lib"
```

On Windows, use the UCRT64 shell with Go and the UCRT64 compiler on `PATH`, then enable cgo:

```sh
export CGO_ENABLED=1
export CC=gcc
```

Keep the UCRT64 `bin` directory on `PATH` when running Lode so its DLLs can be found.

## Dependency verification

`make check` verifies module-file consistency and downloaded dependencies. `make security` runs the pinned Go vulnerability scanner. Module versions and checksums are tracked in `go.mod` and `go.sum`; container base images are pinned by digest. Update dependency and image pins when adopting security fixes. Native packages come from system repositories and are outside these Go checks.
