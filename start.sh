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

# Determine the calibration matrix for the configured rotation.
case "${ROTATE_DISPLAY:-}" in
    inverted|180) TOUCH_MATRIX="-1 0 1 0 -1 1" ;;
    left|90)      TOUCH_MATRIX="0 1 0 -1 0 1" ;;
    right|270)    TOUCH_MATRIX="0 -1 1 1 0 0" ;;
    *)            TOUCH_MATRIX="" ;;
esac

# Write a udev rules file so the container's own udevd sets
# LIBINPUT_CALIBRATION_MATRIX via ENV{} — this is stored in the udev runtime
# database and picked up by libinput when Cog opens the input device.
mkdir -p /etc/udev/rules.d
if [ -n "${TOUCH_MATRIX}" ] && [ -n "${TOUCH_DEVICE:-}" ]; then
    printf 'SUBSYSTEM=="input", KERNEL=="event*", ATTRS{name}=="%s", ENV{LIBINPUT_CALIBRATION_MATRIX}="%s"\n' \
        "${TOUCH_DEVICE}" "${TOUCH_MATRIX}" \
        > /etc/udev/rules.d/99-kiosk-touch.rules
    echo "Touch calibration rule: ${TOUCH_MATRIX} (device: ${TOUCH_DEVICE})"
elif [ -n "${TOUCH_MATRIX}" ]; then
    rm -f /etc/udev/rules.d/99-kiosk-touch.rules
    echo "WARNING: ROTATE_DISPLAY=${ROTATE_DISPLAY:-} is set but TOUCH_DEVICE is not — touch coordinates will not be corrected for rotation" >&2
else
    rm -f /etc/udev/rules.d/99-kiosk-touch.rules
fi

# Reload rules and re-enumerate input devices so the calibration property is
# written to the database before Cog opens the device.
udevadm control --reload 2>/dev/null || true
udevadm trigger --type=devices --subsystem-match=input 2>/dev/null || true
udevadm settle --timeout=5 2>/dev/null || true

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
