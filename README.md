# Kiosk WPE

WPE/Cog kiosk browser block for Balena. Runs a fullscreen browser on a DRM display without an X11 or Wayland compositor, and exposes a small HTTP API for URL navigation and health checks.

## Features

- 🖥️ Fullscreen WPE/Cog browser on DRM/KMS display — no X11 or Wayland compositor required
- 🌐 HTTP API for URL navigation, reload, status, and health checks
- 🔄 Display rotation with automatic touch coordinate calibration via udev hwdb
- 🔁 Automatic crash recovery with exponential backoff (up to 30 s); instant detection via process exit channel
- 🩹 Optional upstream recovery probe that hard-restarts Cog once after an outage
- 🔄 Runtime URL control — switch pages without restarting Cog
- ⏳ Startup readiness check — waits for the target URL to be reachable before launching Cog

## Quick start

Set these fleet variables in Balena Cloud to get going:

| Variable | Example | Description |
|----------|---------|-------------|
| `LAUNCH_URL` | `https://example.com` | Page to display on startup |
| `ROTATE_DISPLAY` | `right` | Rotate screen: `left`, `right`, `inverted` |
| `TOUCH_DEVICE` | `*Waveshare*` | Glob to match your touchscreen (see [Touch calibration](#touch-calibration)) |

## Environment variables

### Display

| Variable | Default | Description |
|----------|---------|-------------|
| `LAUNCH_URL` | `about:blank` | URL loaded on startup |
| `ROTATE_DISPLAY` | _(unset)_ | Rotate display: `inverted`/`180`, `left`/`90`, `right`/`270` |
| `COG_PLATFORM_PARAMS` | _(derived from `ROTATE_DISPLAY`)_ | Override Cog DRM platform params directly (see [Display rotation](#display-rotation)) |
| `COG_PLATFORM_DRM_VIDEO_MODE` | _(unset)_ | Force a specific video mode, e.g. `1920x1080` |
| `COG_PLATFORM_DRM_MODE_MAX` | _(unset)_ | Cap available modes, e.g. `1920x1080@60` |
| `COG_PLATFORM_DRM_CURSOR` | _(unset)_ | Set to any non-empty value to show the mouse cursor |

### Touch

| Variable | Default | Description |
|----------|---------|-------------|
| `TOUCH_DEVICE` | _(unset)_ | Glob matched against the evdev device name (e.g. `*Waveshare*`, `*eGalax*`). Required for touch calibration — if unset, display rotation does not transform touch coordinates. |

### Browser

| Variable | Default | Description |
|----------|---------|-------------|
| `IGNORE_TLS_ERRORS` | _(unset)_ | Set to `1` to ignore TLS certificate errors |
| `COG_EXTRA_ARGS` | _(unset)_ | Extra CLI flags passed to Cog (see [Cog flags](#cog-flags)) |
| `KIOSK_CACHE_ROOT` | `/tmp/kiosk-wpe-cache` | Dedicated root for per-generation Cog cache directories |
| `KIOSK_RECOVERY_URL` | _(unset)_ | Optional HTTP(S) endpoint checked every 5 seconds. After an unreachable → reachable transition, Cog is hard-restarted once with a fresh cache. |

### API

| Variable | Default | Description |
|----------|---------|-------------|
| `KIOSK_API_PORT` | `5011` | Port for the control API |

## Display rotation

Set `ROTATE_DISPLAY` to rotate both the framebuffer and (when `TOUCH_DEVICE` is also set) the touch coordinate matrix:

| `ROTATE_DISPLAY` | Rotation | Touch matrix |
|------------------|----------|--------------|
| `right` / `270` | 90° CW | `0 -1 1 1 0 0` |
| `left` / `90` | 90° CCW | `0 1 0 -1 0 1` |
| `inverted` / `180` | 180° | `-1 0 1 0 -1 1` |

For advanced use, `COG_PLATFORM_PARAMS` accepts a comma-separated list of DRM parameters. The two supported keys are `renderer` (`modeset` (default) or `gles`) and `rotation` (0–3, counter-clockwise 90° steps). Rotation requires `renderer=gles`. `ROTATE_DISPLAY` sets both automatically — only use `COG_PLATFORM_PARAMS` to override.

## Touch calibration

`TOUCH_DEVICE` is a glob matched against the evdev `NAME` attribute of your touchscreen. The controller logs all detected input device names at startup — check the container logs in Balena Cloud to find the right pattern:

```
Detected input devices:
  Waveshare WS170120
  ...
```

Then set `TOUCH_DEVICE=*Waveshare*` (or whatever matches your device) as a fleet variable. Without this, touch coordinates are not corrected for rotation.

## Cog flags

Pass any of these via `COG_EXTRA_ARGS`:

| Flag | Description |
|------|-------------|
| `--scale=FACTOR` | Zoom/scaling applied to web content (e.g. `1.5`) |
| `--device-scale=FACTOR` | Device pixel ratio for high-DPI displays (e.g. `2.0`) |
| `--bg-color=COLOR` | Background color as CSS name or `#RRGGBBAA` hex |
| `--webprocess-failure=ACTION` | On WebProcess crash: `error-page` (default), `exit`, `exit-ok`, `restart` |
| `--proxy=PROXY` | HTTP proxy URL |
| `--ignore-host=HOSTS` | Proxy bypass hosts |
| `--content-filter=PATH` | Path to a content filter JSON rule set |
| `--automation` | Enable WebDriver automation mode |

For WebKit-level settings (fonts, JavaScript, media, etc.) run `cog --help-websettings` on the device.

## HTTP API

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/url` | Current URL as plain text |
| `POST` | `/url` | `{"url": "https://..."}` — navigate via D-Bus without restarting Cog (async, returns 200 immediately). Falls back to a hard restart if D-Bus is unavailable. Only `http://`, `https://`, and `about:` schemes accepted. |
| `POST` | `/refresh` | Re-navigate to the current URL via D-Bus without restarting Cog (async, returns 200 immediately). Falls back to a hard restart if D-Bus is unavailable. |
| `POST` | `/restart` | Fully restart the Cog process group with a fresh cache and re-apply touch calibration (async, returns 200 immediately). Use when Cog is in a bad state. |
| `GET` | `/status` | JSON with `url`, `running`, `crash_count`, `ready`, `started_at`, `uptime_seconds`, `cog_started_at`, `last_crash_at`, `cog_version` |
| `GET` | `/health` | 200 only while Cog is running, ready and outside a crash loop; otherwise 503 |

```sh
# Current URL
curl http://<device>:5011/url

# Navigate (no Cog restart)
curl -X POST http://<device>:5011/url \
  -H 'content-type: application/json' \
  -d '{"url":"https://example.com"}'

# Soft reload (no Cog restart)
curl -X POST http://<device>:5011/refresh

# Hard restart (re-applies touch calibration)
curl -X POST http://<device>:5011/restart

# Diagnostics (includes uptime, cog version, last crash time, readiness)
curl http://<device>:5011/status
```

## Development

How to setup the development environment.

### Prerequisites

You need the following tools to get started:

- [uv] - A Python virtual environment/package manager (for dev tooling)
- [Go] (1.23+) - The programming language
- [golangci-lint] - Go linter

### Installation

1. Clone the repository
2. Install the dev tooling dependencies

```bash
uv sync --dev
```

3. Setup the pre-commit hooks

```bash
uv run pre-commit install
```

4. Build the controller binary

```bash
go build -o kiosk_controller .
```

### Run pre-commit checks

As this repository uses the [pre-commit][pre-commit] framework, all changes
are linted with each commit. You can run all checks manually using the following command:

```bash
uv run pre-commit run --all-files
```

To run only on staged files:

```bash
uv run pre-commit run
```

## Runtime notes

- The controller is a static Go binary — no runtime dependencies in the image.
- The arm64 runtime pins Cog, WPE WebKit and its FDO backend from Debian Trixie. Renovate tracks
  updates that are actually published for Trixie as one non-automerge `deploy-pr`, so each
  browser-engine bump goes through the normal block release and hardware validation flow. Testing
  or unstable packages are deliberately not mixed into this production image.
- Startup, Cog and D-Bus diagnostics log configured URLs verbatim. This is intentional so operators
  can inspect the exact navigation target; deployments that place credentials or sensitive
  identifiers in URLs accept that those values can appear in the container logs.
- URL navigation and page reloads use D-Bus (`org.gtk.Application.Open` on `com.igalia.Cog`) so Cog never needs to restart for a URL change. A D-Bus session daemon is started by `start.sh` and its address is exported as `DBUS_SESSION_BUS_ADDRESS`. If D-Bus is unavailable, all navigation falls back to a hard restart.
- After a D-Bus navigation, `udevadm trigger --action=change` is fired after 500 ms so libinput re-reads the hwdb calibration matrix for any input device opened by the new WPEWebProcess.
- Cog and all its WPE subprocesses run in their own process group; on a hard restart the entire group is signalled so DRM/GL resources are fully released before the new instance starts.
- Every hard restart uses a new cache generation below `KIOSK_CACHE_ROOT` and removes the previous
  generation. Persistent browser data such as LocalStorage and IndexedDB is not removed.
- Cog defaults to `--webprocess-failure=exit`, making a crashed web process visible to the
  supervisor and `/health` instead of leaving a falsely healthy error surface. An explicit
  `COG_EXTRA_ARGS` value can override that policy.
- A 500 ms settle delay after stopping Cog prevents "Cannot set mode (Permission denied)" DRM errors when using the `gles` renderer.
- Cog crashes are detected instantly (via process exit channel) and restarted with exponential backoff (max 30 s). The crash counter resets only after 30 s of stable uptime.
- `/health` returns 503 while Cog is stopped, not ready or above five crashes; it does not claim
  that the loaded page itself rendered successfully.
- `LAUNCH_URL` is authoritative when the container starts. URL changes made through the control API apply to the current container runtime; after a container restart the latest `LAUNCH_URL` is loaded again.
- When `KIOSK_RECOVERY_URL` is configured, the controller records endpoint availability every five
  seconds. An initially healthy endpoint does not restart Cog. If the endpoint is unavailable at
  startup, or becomes unavailable later, the next successful check triggers one hard restart;
  further successful checks do not restart again until another outage is observed.
  Leaving the variable unset preserves the existing runtime behavior.
- udev is started in-container; `io.balena.features.udev` does not reliably mount `/run/udev` on all Balena OS versions. A warning is logged to stderr if udev fails to start.
- Setting `ROTATE_DISPLAY` without `TOUCH_DEVICE` logs a warning — touch coordinates will not be corrected for rotation.

## License

Distributed under the **MIT** License - see [LICENSE](LICENSE) for details.

<!-- LINK -->
[uv]: https://docs.astral.sh/uv/
[Go]: https://go.dev/
[golangci-lint]: https://golangci-lint.run/
[pre-commit]: https://pre-commit.com
