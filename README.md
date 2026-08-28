# rclone-manager

`rclone-manager` is a small Go supervisor for long-running `rclone mount`
containers. It removes stale rclone FUSE attachments before every start,
installs the selected official rclone release at runtime, forwards signals, and
restarts rclone with capped exponential backoff when the process exits or a
previously active mount disappears.

rclone is deliberately not bundled in the image. The selected executable lives
under `/app/bin`; its validation manifest and the rclone configuration live in
the persistent `data` volume under `/app/data`.

## Why a manager?

A FUSE mount can outlive the userspace process that served it or become unusable
after a process, container, kernel, storage, or network failure. Accessing the
mounted path may then return `Transport endpoint is not connected`, block, or
prevent a replacement mount from starting. Heavy file activity makes unclean
shutdowns more likely because more requests and open handles are in flight.

The manager never probes the mounted filesystem with file I/O. It reads
`/proc/self/mountinfo`, identifies only an exact `fuse.rclone` attachment at the
configured mountpoint, and escalates through normal, forced, and lazy Linux
`umount2` operations. An unrelated filesystem at that path is rejected rather
than unmounted.

Ubuntu is the runtime base because the separately downloaded `rclone mount`
binary still needs the FUSE userspace helper (`fusermount3`). The image also
provides CA certificates for release downloads and timezone data. The manager
itself uses direct Linux syscalls for cleanup; embedding a Go FUSE library would
not change how the separate rclone process creates its mount.

## Quick start

The host must be Linux with FUSE available. Docker needs access to `/dev/fuse`,
`CAP_SYS_ADMIN`, and shared mount propagation so the mount is visible outside
the container.

```console
cp .env.example .env
$EDITOR .env
docker compose up -d
docker compose exec rclone-manager rclone config
docker compose restart rclone-manager
docker compose logs -f rclone-manager
```

The interactive `rclone config` command writes
`/app/data/config/rclone.conf` directly to the `data` named volume. The manager
may retry the configured mount until that first-time setup is complete.

The included Compose deployment mounts host `/mnt` into the container with
`rshared` propagation. The corresponding host mount must itself be shared. You
can inspect it with:

```console
findmnt -o TARGET,PROPAGATION /mnt
```

If it is not shared, configure the host mount persistently or make it shared
before starting the container:

```console
sudo mount --make-rshared /mnt
```

The Compose file uses `apparmor:unconfined`, which is commonly required on
AppArmor-enabled hosts. Treat the container as privileged infrastructure: both
the manager and rclone intentionally run as root so the manager can repair
mounts. Use native rclone `RCLONE_UID` and `RCLONE_GID` settings to control the
ownership rclone exposes through the mounted filesystem.

## Manager configuration

Every manager setting is present and documented in `.env.example`.

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `RCLONE_REMOTE` | Yes | none | Remote and optional path passed to `rclone mount`, such as `infinidysk:` or `s3:media`. |
| `RCLONE_VERSION` | No | `latest` | Exact stable version such as `1.74.2` (an optional leading `v` is accepted), or `latest`. |
| `RCLONE_MOUNTPOINT` | No | `/mnt/rclone` | Absolute mountpoint owned by the manager. `/` is rejected. |
| `RCLONE_MANAGER_SHUTDOWN_TIMEOUT` | No | `30s` | Positive Go duration to wait after `SIGINT` or `SIGTERM` before forced unmount and `SIGKILL`. |

Manager-owned settings are consumed before rclone starts. All other native
`RCLONE_*` environment variables are forwarded unchanged except
`RCLONE_DAEMON`. Daemon mode is ignored because the manager must own a foreground
rclone process. The image supplies these native defaults:

| Native rclone variable | Image default |
| --- | --- |
| `RCLONE_CONFIG` | `/app/data/config/rclone.conf` |
| `RCLONE_CACHE_DIR` | `/app/cache` |

Add any native rclone environment option directly to `.env`. For example, the
shell command in the motivating deployment maps to:

```dotenv
RCLONE_BUFFER_SIZE=0M
RCLONE_DIR_CACHE_TIME=365d
RCLONE_VFS_CACHE_MODE=full
RCLONE_VFS_CACHE_MAX_SIZE=8G
RCLONE_VFS_CACHE_MAX_AGE=24h
RCLONE_VFS_READ_AHEAD=512M
RCLONE_LINKS=true
RCLONE_USE_COOKIES=true
RCLONE_ALLOW_OTHER=true
RCLONE_UID=1000
RCLONE_GID=1000
RCLONE_RC=true
RCLONE_RC_ADDR=0.0.0.0:5572
RCLONE_RC_USER=rclone
RCLONE_RC_PASS=replace-me
```

If the RC endpoint should be reachable from the host, explicitly add its port
mapping to `compose.yaml`. It is not published by default.

## rclone installation and offline behavior

For an exact `RCLONE_VERSION`, an already installed matching binary is reused
without a network request. Otherwise the manager resolves the release through
the GitHub API, selects the official Linux archive for `amd64` or `arm64`,
verifies the SHA-256 digest published in the release metadata, extracts only the
expected rclone executable, validates `rclone version`, and atomically replaces
the managed files.

`latest` is resolved on each clean manager start so it can advance. If GitHub is
temporarily unavailable, a previously installed binary is reused only when its
version and SHA-256 still match the manager's validation manifest. Transient
installation failures retry forever with a delay capped at one minute. Invalid
configuration or release metadata fails immediately.

## Process and mount lifecycle

1. Repair an old `fuse.rclone` attachment without traversing the mountpoint.
2. Install or reuse the requested rclone version.
3. Start `rclone mount` in the foreground and in its own process group.
4. Forward `SIGHUP`; on `SIGINT` or `SIGTERM`, wait for graceful exit, then force
   cleanup and kill the process group if the timeout expires.
5. Clean up and restart after any child exit or after an observed mount vanishes.

The manager intentionally does not perform active reads or writes as a health
check. Such probes can themselves hang behind a broken FUSE request. Liveness is
based on the rclone process and the kernel mount table.

## Development

The project requires Go 1.27 or newer. Run the local quality gates with:

```console
go test ./...
go test -race ./...
go vet ./...
golangci-lint run
docker compose config --quiet
docker build -t rclone-manager:dev .
```

Pushes to `main` publish the multi-architecture `edge` image. Bare Semantic
Version tags such as `1.0.0` publish version aliases to GHCR and create a GitHub
Release from the matching changelog section.

## License

[MIT](LICENSE)
