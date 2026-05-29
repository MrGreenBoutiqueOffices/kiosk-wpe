#!/bin/sh
set -eu

export LAUNCH_URL="${LAUNCH_URL:-about:blank}"
export KIOSK_API_PORT="${KIOSK_API_PORT:-5011}"

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
echo "========================="

# Start udev so libinput can enumerate input devices.
# io.balena.features.udev does not reliably mount /run/udev on all Balena OS versions.
if [ ! -d /run/udev ]; then
    mkdir -p /run/udev
    /lib/systemd/systemd-udevd --daemon --resolve-names=never 2>/dev/null || true
    echo "udev started"
fi

# Apply touch calibration via udev hwdb when TOUCH_DEVICE and ROTATE_DISPLAY are set.
case "${ROTATE_DISPLAY:-}" in
    inverted|180) TOUCH_MATRIX="-1 0 1 0 -1 1" ;;
    left|90)      TOUCH_MATRIX="0 1 0 -1 0 1" ;;
    right|270)    TOUCH_MATRIX="0 -1 1 1 0 0" ;;
    *)            TOUCH_MATRIX="" ;;
esac

mkdir -p /etc/udev/hwdb.d
if [ -n "${TOUCH_MATRIX}" ] && [ -n "${TOUCH_DEVICE:-}" ]; then
    printf 'evdev:name:%s:*\n LIBINPUT_CALIBRATION_MATRIX=%s\n' \
        "${TOUCH_DEVICE}" "${TOUCH_MATRIX}" \
        > /etc/udev/hwdb.d/99-kiosk-touch.hwdb
    echo "Touch calibration: ${TOUCH_MATRIX} (device: ${TOUCH_DEVICE})"
elif [ -n "${TOUCH_MATRIX}" ]; then
    echo "Touch calibration skipped: set TOUCH_DEVICE to enable"
    rm -f /etc/udev/hwdb.d/99-kiosk-touch.hwdb
else
    rm -f /etc/udev/hwdb.d/99-kiosk-touch.hwdb
fi
udevadm hwdb --update 2>/dev/null || true
udevadm trigger --type=devices --subsystem-match=input 2>/dev/null || true
udevadm settle --timeout=3 2>/dev/null || true

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
