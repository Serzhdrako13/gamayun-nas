# Building from source

[Русский](build.md) · [README](../README.en.md)

## Requirements
- Linux amd64, `git`, `bash`, `binutils` (`ar`), `curl`, `sha256sum`;
- **Go 1.24** or newer;
- **Node.js 22** or newer and `npm` — only if you change the web interface.

## Layout
| Directory | Contents |
|---|---|
| `cmd/gamayund` | management daemon: API, web UI, tasks, events |
| `cmd/gamayun-helper` | root executor of operations (disks, Samba, SSH) |
| `cmd/gamayun-catalog` | app catalog signing tool |
| `cmd/gamayun-disks` | diagnostic disk snapshot (JSON) |
| `internal/` | logic: storage, NAS, apps, notifications, etc. |
| `web/` | Svelte 5 interface; the build goes to `internal/web/dist` and is embedded into the binary |
| `packaging/` | systemd units, default configuration, package scripts |
| `api/openapi.yaml` | HTTP API description |

Code comments and server messages are in Russian; the web interface and its help are available in English.

## Tests
```sh
go vet ./... && go test ./...
cd web && npm ci && npm test
```

## Web interface
The built interface is already in `internal/web/dist`. After changing `web/`:
```sh
cd web && npm ci && npm run build
```

## .deb packages
```sh
scripts/build-deb.sh                 # gamayun-nas_<version>_amd64.deb in dist/
scripts/fetch-apps-vendor.sh         # pinned Docker, Compose, Caddy (sha256-verified)
scripts/build-deb-apps.sh            # gamayun-nas-apps_<version>_amd64.deb in dist/
```

The version comes from git: `0.1.0~alpha.<commit count>+g<commit>`; `.dirty` is appended when there
are uncommitted changes. Set `OUT=…` to change the output directory.

Docker stack versions are pinned in `packaging/apps/versions.lock` (URL and sha256 of every archive).

## App catalog
```sh
go build -o gamayun-catalog ./cmd/gamayun-catalog
./gamayun-catalog -h                  # commands: keygen, build, sign, verify, pack
```
Put the catalog public key into the directory set by `catalog.keys_dir` in the `gamayund` configuration
and the archive URL into `catalog.url` (`/etc/gamayun-nas/gamayund.json`).
