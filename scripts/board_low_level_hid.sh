#!/bin/sh
set +e

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/board_low_level_common.sh"

: "${SEND_HID:=0}"
: "${HID_KEY:=ESCAPE}"
: "${HID_TEXT:=}"
: "${HID_CLICK_X:=16384}"
: "${HID_CLICK_Y:=16384}"
: "${HID_CLICK_BUTTON:=left}"
: "${HID_DURATION_MS:=80}"

usage() {
    cat <<'USAGE'
Usage:
  scripts/board_low_level_hid.sh [options]

Runs board-local USB HID checks. By default it is read-only and does not emit
keyboard or pointer reports.

Options:
  --send-hid             Actually send HID keyboard/click events
  --hid-key KEY          Key for example_usb_hid keyboard tap, default ESCAPE
  --hid-text TEXT        Optional ASCII text for example_usb_hid keyboard text
  --click X,Y            Raw HID click coordinates 0..32767, default 16384,16384
  --click-button BUTTON  left|right|middle, default left
  --hid-duration-ms N    HID press/click hold duration, default 80
  -h, --help             Show this help

This script does not use Agent, Config Web, SSH, or HTTP APIs and must run on the board.
USAGE
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --send-hid)
            SEND_HID=1
            shift
            ;;
        --hid-key)
            [ "$#" -ge 2 ] || die "--hid-key requires an argument"
            HID_KEY="$2"
            shift 2
            ;;
        --hid-text)
            [ "$#" -ge 2 ] || die "--hid-text requires an argument"
            HID_TEXT="$2"
            shift 2
            ;;
        --click)
            [ "$#" -ge 2 ] || die "--click requires X,Y"
            case "$2" in
                *,*)
                    HID_CLICK_X="${2%%,*}"
                    HID_CLICK_Y="${2#*,}"
                    ;;
                *)
                    die "--click expects X,Y"
                    ;;
            esac
            shift 2
            ;;
        --click-button)
            [ "$#" -ge 2 ] || die "--click-button requires an argument"
            HID_CLICK_BUTTON="$2"
            shift 2
            ;;
        --hid-duration-ms)
            [ "$#" -ge 2 ] || die "--hid-duration-ms requires an argument"
            HID_DURATION_MS="$2"
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            die "unknown option: $1"
            ;;
    esac
done

case "$HID_CLICK_BUTTON" in
    left|right|middle) ;;
    *) die "--click-button must be left, right, or middle" ;;
esac
require_uint "--click X" "$HID_CLICK_X"
require_uint "--click Y" "$HID_CLICK_Y"
require_uint "--hid-duration-ms" "$HID_DURATION_MS"

run_privileged_hid() {
    aico_hid_name="$1"
    shift
    if [ "$(id -u)" -eq 0 ]; then
        run_check "$aico_hid_name" "$@"
        return $?
    fi
    if [ -w /dev/hidg0 ] && [ -w /dev/hidg1 ]; then
        run_check "$aico_hid_name" "$@"
        return $?
    fi
    if command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; then
        run_check "$aico_hid_name" sudo -n "$@"
        return $?
    fi
    fail "$aico_hid_name cannot run: /dev/hidg0 or /dev/hidg1 is not writable by $(id -un). Run locally as root or grant HID write permission."
    return 1
}

section "hid"
HID_BIN="$(find_bin example_usb_hid)"
[ -n "$HID_BIN" ] && pass "example_usb_hid: $HID_BIN" || fail "example_usb_hid not found"
check_exists "keyboard HID" /dev/hidg0
check_exists "pointer HID" /dev/hidg1
check_exists "consumer/control HID" /dev/hidg2

if [ -r /sys/kernel/config/usb_gadget/aico_hid/UDC ]; then
    UDC="$(cat /sys/kernel/config/usb_gadget/aico_hid/UDC 2>/dev/null)"
    if [ -n "$UDC" ]; then
        pass "USB gadget bound to UDC: $UDC"
    else
        fail "USB gadget exists but UDC is empty"
    fi
else
    warn "USB gadget UDC state not readable"
fi

if command -v ip >/dev/null 2>&1; then
    ip addr show usb0 2>/dev/null | indent
    if ip -4 addr show dev usb0 2>/dev/null | grep -q '192\.168\.42\.1'; then
        pass "usb0 has 192.168.42.1"
    else
        warn "usb0 does not show 192.168.42.1"
    fi
fi

if [ "$SEND_HID" = "1" ]; then
    if [ -n "$HID_BIN" ]; then
        run_privileged_hid "HID keyboard tap $HID_KEY" "$HID_BIN" --duration-ms "$HID_DURATION_MS" keyboard tap "$HID_KEY"
        if [ -n "$HID_TEXT" ]; then
            run_privileged_hid "HID keyboard text" "$HID_BIN" --duration-ms "$HID_DURATION_MS" keyboard text "$HID_TEXT"
        else
            skip "HID keyboard text not requested; pass --hid-text to type visible ASCII"
        fi
        run_privileged_hid "HID pointer click ${HID_CLICK_X},${HID_CLICK_Y} $HID_CLICK_BUTTON" \
            "$HID_BIN" --duration-ms "$HID_DURATION_MS" touch click "$HID_CLICK_X" "$HID_CLICK_Y" "$HID_CLICK_BUTTON"
    else
        fail "HID send requested but example_usb_hid is missing"
    fi
else
    skip "HID send disabled; pass --send-hid to write /dev/hidg0 and /dev/hidg1"
fi

print_summary
exit $?
