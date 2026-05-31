# Kiosk WPE

WPE/Cog kiosk browser block for Balena. Runs a fullscreen browser on a DRM display without an X11 or Wayland compositor, and exposes a small HTTP API for URL navigation and health checks.

## Features

- 🖥️ Fullscreen WPE/Cog browser on DRM/KMS display — no X11 or Wayland compositor required
- 🌐 HTTP API for URL navigation, reload, status, health, and screenshot
- 🔄 Display rotation with automatic touch coordinate calibration via udev hwdb
- 🔁 Automatic crash recovery with exponential backoff (up to 30 s)
- 💾 URL persistence across Cog restarts
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
| `POST` | `/url` | `{"url": "https://..."}` — navigate and restart Cog |
| `POST` | `/refresh` | Restart Cog with the current URL |
| `GET` | `/status` | JSON: `url`, `running`, `crash_count` |
| `GET` | `/health` | Always 200 OK while the controller process is alive |
| `GET` | `/screenshot` | Current screen as PNG (requires `/dev/fb0`; see [Screenshot](#screenshot)) |

```sh
# Current URL
curl http://<device>:5011/url

# Navigate
curl -X POST http://<device>:5011/url \
  -H 'content-type: application/json' \
  -d '{"url":"https://example.com"}'

# Reload
curl -X POST http://<device>:5011/refresh

# Diagnostics
curl http://<device>:5011/status

# Save screenshot
curl http://<device>:5011/screenshot -o screen.png
```

## Screenshot

`GET /screenshot` captures the current display and returns a PNG. It reads
`/dev/fb0` (Linux legacy framebuffer) which requires the DRM driver to expose
one. On Raspberry Pi 4 (vc4 driver) this works out of the box. The endpoint
returns `503` when `/dev/fb0` is not available rather than failing the
container.

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
- Cog crashes are automatically restarted with exponential backoff (max 30 s).
- udev is started in-container; `io.balena.features.udev` does not reliably mount `/run/udev` on all Balena OS versions.
- The active URL is persisted across Cog restarts. `LAUNCH_URL` is only used when no persisted URL exists yet.

## License

Distributed under the **MIT** License - see [LICENSE](LICENSE) for details.

<!-- LINK -->
[uv]: https://docs.astral.sh/uv/
[Go]: https://go.dev/
[golangci-lint]: https://golangci-lint.run/
[pre-commit]: https://pre-commit.com
