# kiosk-wpe - Agent Guide

Use this as the canonical instruction file for AI agents working in this repo.

## What this repo is

A Balena block that runs a WPE/Cog kiosk browser on a DRM display (no X11 or Wayland), with a small HTTP control API. Intended as a generic, open-source Balena block — no project-specific defaults or hardcoding.

## Architecture

- **`kiosk_controller.go`** — the main process. Static Go binary, runs as PID 1 via `start.sh`. Manages the Cog subprocess (launch, supervise, restart with exponential backoff) and serves the HTTP control API on port 5011.
- **`start.sh`** — container entrypoint. Sets defaults, derives `COG_PLATFORM_PARAMS` from `ROTATE_DISPLAY`, writes udev hwdb for touch calibration, logs detected input devices, waits for the URL to be reachable, then execs `kiosk_controller`.
- **`Dockerfile.template`** — multi-stage build: Go binary compiled in `golang:alpine`, runtime image is `debian:trixie-slim` + `cog` + its DRM/GL dependencies.

## Key constraints

- **No Docker healthcheck in `docker-compose.yml`**: Balena supervisor restarts containers it marks unhealthy. The `/health` API endpoint is for external checks only.
- **`COG_PLATFORM_NAME` is hardcoded to `drm`** in `buildArgs()` — do not re-expose it as an env var.
- **Touch calibration requires `TOUCH_DEVICE`**: the hwdb `evdev:name:*:*` wildcard does not match; users must set a specific glob (e.g. `*Waveshare*`).
- **Static binary, no CGO**: `CGO_ENABLED=0`, so no libc dependency in the Go binary itself.
- **Single `package main`**, no Go modules beyond stdlib — keep it that way unless there is a strong reason.

## File map

| File | Role |
|------|------|
| `kiosk_controller.go` | Go binary: process management + HTTP API |
| `start.sh` | Entrypoint: env defaults, udev, touch calibration, URL readiness wait |
| `Dockerfile.template` | Multi-stage Docker build |
| `balena.yml` | Balena block metadata and supported device types |
| `examples/docker-compose.yml` | Example Balena compose for this block |
| `README.md` | User-facing documentation |

## Development

Build and run the Go binary locally:

```bash
go build -o kiosk_controller .
```

Run linting:

```bash
golangci-lint run
gofmt -l .
```

Run pre-commit checks:

```bash
uv run pre-commit run --all-files
```

## Releasing

Push a tag in the form `v<major>.<minor>.<patch>` to trigger the deploy workflow. The workflow updates `version` in `balena.yml` and deploys to the Balena fleet set in `vars.BALENA_SLUG`.

## What to avoid

- Do not add project-specific defaults, URLs, or naming.
- Do not add a Docker healthcheck to compose files — it causes Balena to restart the container.
- Do not introduce CGO or external Go dependencies without strong justification.
- Do not remove the `//nolint:gosec` comments on `exec.Command` and the state file path — these are intentional and reviewed.
