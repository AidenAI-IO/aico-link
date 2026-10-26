#!/bin/sh
# mcu_touch_test.sh - Capacitive touch-key test for the Aiden board.
#
# Follows the MCU event service log and watches for touch-key events the MCU
# reports over its UART. Each touch is a WAKEUP event whose wake source is
# TOUCH_KEY (0x03). Touches are highlighted and counted live; a summary with
# the total is printed when the capture window ends.
#
# Designed to run ON THE BOARD, as root (journalctl of a system service):
#   scp scripts/mcu_touch_test.sh aiden@192.168.42.1:/tmp/
#   ssh aiden@192.168.42.1 'sudo sh /tmp/mcu_touch_test.sh --seconds 60'
#   # then touch the capacitive key repeatedly during the window
#
# The MCU reports the touch key as a wake source, so an event is emitted on
# each touch. Wake sources: 0x01 EXTERNAL_POWER, 0x02 POWER_SWITCH,
# 0x03 TOUCH_KEY, 0x04 RTC_TIMER, 0x05 OTHER, 0x06 ASR.
set -eu

SERVICE="${MCU_SERVICE:-aiden-mcu-event.service}"
SECONDS=0          # capture window; 0 = run until interrupted (Ctrl-C)
SHOW_ALL=0         # 1 = also print non-touch MCU events for context

usage() {
    cat <<'USAGE'
Usage: mcu_touch_test.sh [options]

Options:
  --seconds N   capture window in seconds; 0 runs until Ctrl-C (default: 0)
  --all         also show other MCU events (not just TOUCH_KEY)
  --service N   systemd unit of the MCU event service
                (default: aiden-mcu-event.service)
  -h, --help    show this help

Run as root; reading a system service journal needs privileges.
Touch the capacitive key during the window: each touch prints a highlighted
[TOUCH #n] line, and the total is reported at the end.
USAGE
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --seconds) [ "$#" -ge 2 ] || { echo "--seconds needs an argument" >&2; exit 2; }; SECONDS="$2"; shift 2 ;;
        --all)     SHOW_ALL=1; shift ;;
        --service) [ "$#" -ge 2 ] || { echo "--service needs an argument" >&2; exit 2; }; SERVICE="$2"; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *)         echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
done

case "$SECONDS" in ''|*[!0-9]*) echo "--seconds must be numeric" >&2; exit 2 ;; esac

if ! command -v journalctl >/dev/null 2>&1; then
    echo "journalctl not found; this script must run on the board" >&2
    exit 1
fi

if [ "$SECONDS" -gt 0 ]; then
    echo "Watching $SERVICE for TOUCH_KEY events for ${SECONDS}s - touch the key now..."
else
    echo "Watching $SERVICE for TOUCH_KEY events (Ctrl-C to stop) - touch the key now..."
fi

# Stream the live log (since now) and, when a window is set, stop after it.
stream() {
    if [ "$SECONDS" -gt 0 ]; then
        timeout "$SECONDS" journalctl -u "$SERVICE" --since now -f -o cat 2>/dev/null
    else
        journalctl -u "$SERVICE" --since now -f -o cat 2>/dev/null
    fi
}

# Highlight + count TOUCH_KEY; optionally echo other events; summarize at end.
stream | awk -v show_all="$SHOW_ALL" '
    /wake_source_name=TOUCH_KEY/ {
        touch++
        printf "\033[32m[TOUCH #%d]\033[0m %s\n", touch, $0
        fflush()
        next
    }
    /EVENT cmd/ {
        other++
        if (show_all) { printf "  (other) %s\n", $0; fflush() }
        next
    }
    END {
        printf "\n== summary ==\n"
        printf "TOUCH_KEY touches: %d\n", touch + 0
        printf "other MCU events:  %d\n", other + 0
        if ((touch + 0) == 0) {
            printf "No touches detected. If the key is a wake source only, it may\n"
            printf "report only from standby - try touching while the board sleeps.\n"
        }
    }
'
