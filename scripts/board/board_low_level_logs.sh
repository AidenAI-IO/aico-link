#!/bin/sh
set +e

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/board_low_level_common.sh"

LINES=30

usage() {
    cat <<'USAGE'
Usage:
  scripts/board/board_low_level_logs.sh [options]

Prints recent board-local low-level service logs.

Options:
  --lines N    Lines per log file, default 30
  -h, --help   Show this help

This script does not use Agent, Config Web, SSH, or HTTP APIs and must run on the board.
USAGE
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --lines)
            [ "$#" -ge 2 ] || die "--lines requires an argument"
            LINES="$2"
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
require_uint "--lines" "$LINES"

section "recent logs"
FOUND=0
for log_path in /var/log/frame_service/frame_service.log /var/log/audio_service/audio_service.log; do
    if [ -r "$log_path" ]; then
        FOUND=1
        printf '%s\n' "-- $log_path"
        tail -n "$LINES" "$log_path" | indent
    fi
done

if [ "$FOUND" -eq 0 ]; then
    warn "no readable low-level service logs found"
fi

print_summary
exit $?
