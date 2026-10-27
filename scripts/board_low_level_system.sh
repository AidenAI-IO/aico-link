#!/bin/sh
set +e

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/board_low_level_common.sh"

usage() {
    cat <<'USAGE'
Usage:
  scripts/board_low_level_system.sh

Runs board-local system readiness checks:
  - board identity, date, kernel, OS
  - USB HID, frame, and audio service status
  - required low-level binaries

This script does not use Agent, Config Web, SSH, or HTTP APIs and must run on the board.
USAGE
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        -h|--help)
            usage
            exit 0
            ;;
        *)
            die "unknown option: $1"
            ;;
    esac
done

section "board"
hostname 2>/dev/null | indent
date -Is 2>/dev/null | indent
id 2>/dev/null | indent
uname -a 2>/dev/null | indent
[ -r /etc/os-release ] && sed -n '1,8p' /etc/os-release | indent

section "services"
if [ -x /usr/lib/aico/aico-usb-gadget ]; then
    pass "USB gadget helper present: /usr/lib/aico/aico-usb-gadget"
else
    warn "USB gadget helper missing: /usr/lib/aico/aico-usb-gadget"
fi
check_exists "USB HID keyboard" /dev/hidg0
check_exists "USB HID pointer" /dev/hidg1
check_exists "USB HID consumer/control" /dev/hidg2
if [ -r /sys/kernel/config/usb_gadget/aico_hid/UDC ]; then
    UDC="$(cat /sys/kernel/config/usb_gadget/aico_hid/UDC 2>/dev/null)"
    if [ -n "$UDC" ]; then
        pass "USB HID gadget bound to UDC: $UDC"
    else
        fail "USB HID gadget exists but UDC is empty"
    fi
else
    warn "USB HID gadget UDC state not readable"
fi
service_check "HDMI frame service" "aico-frame.service aico_frame.service frame_service.service aico-frame-service.service" "/usr/lib/aico/aico-frame-control"
service_check "audio service" "aico-audio.service aico_audio.service audio_service.service aico-audio-service.service" ""

section "binaries"
HID_BIN="$(find_bin example_usb_hid)"
FRAME_CLI="$(find_bin frame_service_cli)"
AUDIO_CLI="$(find_bin audio_service_cli)"
[ -n "$HID_BIN" ] && pass "example_usb_hid: $HID_BIN" || fail "example_usb_hid not found"
[ -n "$FRAME_CLI" ] && pass "frame_service_cli: $FRAME_CLI" || fail "frame_service_cli not found"
[ -n "$AUDIO_CLI" ] && pass "audio_service_cli: $AUDIO_CLI" || fail "audio_service_cli not found"

print_summary
exit $?
