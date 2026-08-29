<div align="center">

# Rclone Manager

[![Release](https://img.shields.io/github/v/release/neurekadev/rclone-manager?style=flat-square&label=Release&color=F43F5E&logo=github&logoColor=F43F5E)](https://github.com/neurekadev/rclone-manager/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/neurekadev/rclone-manager/CI.yml?branch=main&style=flat-square&label=CI&color=8B5CF6&logo=githubactions&logoColor=8B5CF6)](https://github.com/neurekadev/rclone-manager/actions/workflows/CI.yml)
[![License](https://img.shields.io/github/license/neurekadev/rclone-manager?style=flat-square&label=License&color=14B8A6&logo=opensourceinitiative&logoColor=14B8A6)](./LICENSE)
[![AI](https://img.shields.io/badge/AI-assisted-5786FE?style=flat-square&logo=deepseek&logoColor=5786FE)](https://github.com/neurekadev/rclone-manager)
[![Stars](https://img.shields.io/github/stars/neurekadev/rclone-manager?style=flat-square&label=Stars&color=EAB308&logo=googlegemini&logoColor=EAB308)](https://github.com/neurekadev/rclone-manager)

Keep long-running rclone FUSE mounts healthy with verified runtime installation, safe stale-mount repair, automatic restarts, and graceful shutdown handling in one Docker container.

</div>

> [!IMPORTANT]
> Rclone Manager requires a Linux host with FUSE, `/dev/fuse`, `CAP_SYS_ADMIN`, and shared mount propagation. The included Compose deployment also disables AppArmor confinement. Treat the container as privileged infrastructure.

## Quickstart

Download [`compose.yaml`](./compose.yaml) and [`.env.example`](./.env.example).

## Usage

Create your environment file and set `RCLONE_REMOTE` to the remote and optional path you want to mount, such as `s3:media`.

```bash
cp .env.example .env
$EDITOR .env
```

Start the container, configure the remote, and restart the manager so it can mount the configured path.

```bash
docker compose up -d
docker compose exec rclone-manager rclone config
docker compose restart rclone-manager
docker compose logs -f rclone-manager
```

The rclone configuration is stored in the persistent `data` volume. Initial mount retries are expected until the remote is configured.

The included deployment exposes `/mnt` from the container to the host. Confirm that the host mount uses shared propagation:

```bash
findmnt -o TARGET,PROPAGATION /mnt
sudo mount --make-rshared /mnt
```

Make the propagation setting persistent using the configuration appropriate for your host.

## Features

- **Self-healing mounts** — Repairs stale `fuse.rclone` attachments and restarts rclone when its process exits or an active mount disappears.
- **Verified installation** — Downloads official rclone releases, verifies their published SHA-256 digest and reported version, and installs them atomically.
- **Offline fallback** — Reuses a validated cached version when `latest` cannot be resolved because GitHub is temporarily unavailable.
- **Safe cleanup** — Refuses to unmount unrelated filesystems and escalates through normal, forced, and lazy unmounts only for the configured rclone mountpoint.
- **Controlled shutdown** — Forwards `SIGHUP`, allows graceful `SIGINT` and `SIGTERM` handling, then cleans up and stops the rclone process group when necessary.
- **Multi-platform images** — Publishes Linux container images for `amd64` and `arm64` with persistent data and cache volumes.

## Configuration

### Manager settings

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `RCLONE_REMOTE` | Yes | — | Remote and optional path passed to `rclone mount`, such as `s3:media`. |
| `RCLONE_VERSION` | No | `latest` | Latest stable rclone release or an exact stable version such as `1.74.2`; an optional leading `v` is accepted. |
| `RCLONE_MOUNTPOINT` | No | `/mnt/rclone` | Absolute mountpoint owned and supervised by Rclone Manager. The filesystem root is rejected. |
| `RCLONE_MANAGER_SHUTDOWN_TIMEOUT` | No | `30s` | Positive Go duration allowed for graceful rclone shutdown before forced cleanup. |

All native `RCLONE_*` environment variables may be added to `.env` and are forwarded to rclone, except `RCLONE_DAEMON`. Daemon mode is ignored because Rclone Manager must supervise rclone in the foreground.

### Image defaults

| Variable | Default | Purpose |
| --- | --- | --- |
| `RCLONE_CONFIG` | `/app/data/config/rclone.conf` | Stores remote configuration in the persistent `data` volume. |
| `RCLONE_CACHE_DIR` | `/app/cache` | Stores rclone cache data in the persistent `cache` volume. |

## Mount Lifecycle

1. Remove stale rclone-owned attachments from the configured mountpoint without traversing the mounted filesystem.
2. Install or reuse the selected rclone version and validate it before use.
3. Start `rclone mount` in the foreground and monitor both its process and kernel mount state.
4. Clean up and restart with capped exponential backoff after a process or mount failure.
5. Forward shutdown signals, wait for graceful exit, then force cleanup if the configured timeout expires.

## Troubleshooting

| Symptom | What to check |
| --- | --- |
| The mount is not visible on the host | Confirm `/mnt` is a shared host mount and the Compose bind uses `rshared` propagation. |
| Cleanup reports a permissions error | Confirm `/dev/fuse` is available and the container has `CAP_SYS_ADMIN`. |
| Cleanup refuses the configured mountpoint | Another filesystem occupies that exact path. Remove it or select a different `RCLONE_MOUNTPOINT`; Rclone Manager will not unmount it. |
| rclone exits repeatedly during first-time setup | Run `docker compose exec rclone-manager rclone config`, verify `RCLONE_REMOTE`, then restart the service. |
| rclone installation keeps retrying | Check access to GitHub and confirm that `RCLONE_VERSION` is `latest` or an exact stable version. |

## Why Use Rclone Manager?

Rclone Manager turns an rclone FUSE mount into an unattended service. It recovers from stale attachments and child-process failures, verifies downloaded binaries before execution, protects unrelated mounts, and keeps runtime configuration and cache data persistent across container restarts.

## Telemetry

Rclone Manager automatically reports unexpected application errors and anonymous lifecycle events. Automatic error reporting helps fix bugs faster without requiring any action from you. Telemetry is stored on official Neureka.Dev servers and is never provided to third parties.

To disable error reporting and anonymous lifecycle analytics, add `TELEMETRY=false` to your `.env` file and restart Rclone Manager.
