#!/bin/sh
set -eu

export LAUNCH_URL="${LAUNCH_URL:-about:blank}"
export KIOSK_API_PORT="${KIOSK_API_PORT:-5011}"
export NO_AT_BRIDGE=1

if [ -z "${COG_PLATFORM_PARAMS:-}" ]; then
    case "${ROTATE_DISPLAY:-}" in
        inverted|180) export COG_PLATFORM_PARAMS="renderer=gles,rotation=2" ;;
        left|90)      export COG_PLATFORM_PARAMS="renderer=gles,rotation=1" ;;
        right|270)    export COG_PLATFORM_PARAMS="renderer=gles,rotation=3" ;;
    esac
fi

echo "=== kiosk-wpe ==="
echo "  LAUNCH_URL         = ${LAUNCH_URL}"
echo "  ROTATE_DISPLAY     = ${ROTATE_DISPLAY:-<unset>}"
echo "  COG_PLATFORM_PARAMS= ${COG_PLATFORM_PARAMS:-<unset>}"
echo "  COG_EXTRA_ARGS     = ${COG_EXTRA_ARGS:-<unset>}"
echo "  IGNORE_TLS_ERRORS  = ${IGNORE_TLS_ERRORS:-<unset>}"
echo "  TOUCH_DEVICE       = ${TOUCH_DEVICE:-<unset>}"
echo "  API PORT           = ${KIOSK_API_PORT}"
echo "  DBUS_SESSION_BUS_ADDRESS = ${DBUS_SESSION_BUS_ADDRESS:-<unset>}"
echo "========================="

# Start a D-Bus session daemon so kiosk_controller can send navigation commands
# to Cog via org.gtk.Application.Open without restarting the process.
if [ -z "${DBUS_SESSION_BUS_ADDRESS:-}" ]; then
    _dbus_addr=$(dbus-daemon --session --print-address --fork 2>/dev/null) || true
    if [ -n "${_dbus_addr:-}" ]; then
        export DBUS_SESSION_BUS_ADDRESS="${_dbus_addr}"
        echo "D-Bus session bus started: ${DBUS_SESSION_BUS_ADDRESS}"
    else
        echo "WARNING: dbus-daemon failed to start — URL changes will fall back to Cog restart" >&2
    fi
fi

# Start udev so libinput can enumerate input devices.
# io.balena.features.udev does not reliably mount /run/udev on all Balena OS versions.
if [ ! -d /run/udev ]; then
    mkdir -p /run/udev
    if /lib/systemd/systemd-udevd --daemon --resolve-names=never 2>/dev/null; then
        echo "udev started"
        # Wait for the control socket so udevadm trigger reaches the daemon.
        _wait=0
        while [ ! -S /run/udev/control ] && [ "${_wait}" -lt 5 ]; do
            sleep 1
            _wait=$(( _wait + 1 ))
        done
    else
        echo "WARNING: udev failed to start — touch input may be unavailable" >&2
    fi
fi

# Enumerate input devices so the udev runtime database is populated before
# we attempt to inject the calibration property.
udevadm trigger --type=devices --subsystem-match=input 2>/dev/null || true
udevadm settle --timeout=5 2>/dev/null || true

# Determine the calibration matrix for the configured rotation.
case "${ROTATE_DISPLAY:-}" in
    inverted|180) TOUCH_MATRIX="-1 0 1 0 -1 1" ;;
    left|90)      TOUCH_MATRIX="0 1 0 -1 0 1" ;;
    right|270)    TOUCH_MATRIX="0 -1 1 1 0 0" ;;
    *)            TOUCH_MATRIX="" ;;
esac

# Inject touch calibration directly into the udev runtime database so libinput
# picks it up regardless of whether the host or container udev is active.
# The hwdb approach does not work when the host udev socket is mounted
# (privileged containers on Balena always get the host /run/udev).
if [ -n "${TOUCH_MATRIX}" ] && [ -n "${TOUCH_DEVICE:-}" ]; then
    _calibrated=0
    for _event_dir in /sys/class/input/event*; do
        _dev_name=$(cat "${_event_dir}/device/name" 2>/dev/null) || continue
        case "${_dev_name}" in
            ${TOUCH_DEVICE})
                _dev_num=$(cat "${_event_dir}/dev" 2>/dev/null) || continue
                _db_file="/run/udev/data/c${_dev_num}"
                grep -v "^E:LIBINPUT_CALIBRATION_MATRIX=" "${_db_file}" 2>/dev/null > "${_db_file}.tmp" || true
                mv "${_db_file}.tmp" "${_db_file}"
                printf 'E:LIBINPUT_CALIBRATION_MATRIX=%s\n' "${TOUCH_MATRIX}" >> "${_db_file}"
                echo "Touch calibration injected: ${TOUCH_MATRIX} (device: ${_dev_name}, db: ${_db_file})"
                _calibrated=1
                ;;
        esac
    done
    if [ "${_calibrated}" -eq 0 ]; then
        echo "WARNING: No device matching '${TOUCH_DEVICE}' found — touch coordinates will not be corrected" >&2
    fi
elif [ -n "${TOUCH_MATRIX}" ]; then
    echo "WARNING: ROTATE_DISPLAY=${ROTATE_DISPLAY:-} is set but TOUCH_DEVICE is not — touch coordinates will not be corrected for rotation" >&2
fi

# Log detected input device names to help configure TOUCH_DEVICE.
echo "Detected input devices:"
grep '^N: Name=' /proc/bus/input/devices 2>/dev/null \
    | sed 's/N: Name=/  /' \
    || echo "  (none found)"

# Wait for the URL to be reachable before launching Cog; without this, Cog
# may show a blank error page if the target service is still starting.
# Skipped for non-http(s) URLs (about:blank, file://, etc.).
case "${LAUNCH_URL}" in
    http://*|https://*)
        _retries=0
        until wget -q --spider --timeout=2 "${LAUNCH_URL}" 2>/dev/null; do
            _retries=$(( _retries + 1 ))
            if [ "${_retries}" -ge 30 ]; then
                echo "URL not ready after 60s, starting anyway"
                break
            fi
            echo "Waiting for ${LAUNCH_URL}... (${_retries}/30)"
            sleep 2
        done
        ;;
esac

exec /usr/src/app/kiosk_controller
