#!/bin/sh
set +e

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/board_low_level_common.sh"

: "${FRAME_SOCKET:=/run/frame_service/frame_service.sock}"
: "${RUN_V4L2:=0}"

usage() {
    cat <<'USAGE'
Usage:
  scripts/board_low_level_hdmi.sh [options]

Runs board-local HDMI/frame_service checks:
  - video device and frame socket
  - frame_service_cli health
  - screenshot BMP artifact
  - latest raw frame artifact

Options:
  --socket PATH       frame_service Unix socket, default /run/frame_service/frame_service.sock
  --run-v4l2          Also run optional v4l2-ctl probes; may block on some TC358743 firmware
  --artifact-dir DIR  Artifact directory, default /tmp
  --run-id ID         Artifact run id, default UTC timestamp
  -h, --help          Show this help

This script does not use Agent, Config Web, SSH, or HTTP APIs and must run on the board.
USAGE
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --socket)
            [ "$#" -ge 2 ] || die "--socket requires an argument"
            FRAME_SOCKET="$2"
            shift 2
            ;;
        --run-v4l2)
            RUN_V4L2=1
            shift
            ;;
        --artifact-dir)
            [ "$#" -ge 2 ] || die "--artifact-dir requires an argument"
            AICO_LOW_LEVEL_ARTIFACT_DIR="$2"
            shift 2
            ;;
        --run-id)
            [ "$#" -ge 2 ] || die "--run-id requires an argument"
            RUN_ID="$2"
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

SCREENSHOT_OUT="$(artifact_path screenshot.bmp)"
FRAME_OUT="$(artifact_path frame.raw)"

section "hdmi"
FRAME_CLI="$(find_bin frame_service_cli)"
[ -n "$FRAME_CLI" ] && pass "frame_service_cli: $FRAME_CLI" || fail "frame_service_cli not found"
check_exists "video device" /dev/video0
[ -e /dev/v4l-subdev2 ] && check_exists "default HDMI subdev" /dev/v4l-subdev2 || warn "default HDMI subdev missing: /dev/v4l-subdev2"
check_socket "frame" "$FRAME_SOCKET"

if [ -n "$FRAME_CLI" ]; then
    run_check "frame_service health" "$FRAME_CLI" --socket "$FRAME_SOCKET" health
    run_check "frame_service screenshot" "$FRAME_CLI" --socket "$FRAME_SOCKET" screenshot --out "$SCREENSHOT_OUT"
    if [ -s "$SCREENSHOT_OUT" ]; then
        pass "screenshot written: $SCREENSHOT_OUT ($(wc -c < "$SCREENSHOT_OUT") bytes)"
        command -v file >/dev/null 2>&1 && file "$SCREENSHOT_OUT" | indent
    else
        fail "screenshot file is empty or missing: $SCREENSHOT_OUT"
    fi
    run_check "frame_service latest-frame" "$FRAME_CLI" --socket "$FRAME_SOCKET" latest-frame --out "$FRAME_OUT"
    if [ -s "$FRAME_OUT" ]; then
        pass "raw frame written: $FRAME_OUT ($(wc -c < "$FRAME_OUT") bytes)"
    else
        fail "raw frame file is empty or missing: $FRAME_OUT"
    fi
else
    fail "frame_service_cli missing; cannot test frame service"
fi

if [ "$RUN_V4L2" = "1" ] && command -v v4l2-ctl >/dev/null 2>&1; then
    run_warn_check "v4l2 video0 querycap" v4l2-ctl -d /dev/video0 --querycap
    [ -e /dev/v4l-subdev2 ] && run_warn_check "v4l2 HDMI subdev timings" v4l2-ctl -d /dev/v4l-subdev2 --query-dv-timings
elif [ "$RUN_V4L2" = "1" ]; then
    skip "v4l2-ctl not installed"
else
    skip "v4l2 probes disabled; pass --run-v4l2 to exercise v4l2-ctl"
fi

section "artifacts"
print_artifact "screenshot" "$SCREENSHOT_OUT"
print_artifact "frame" "$FRAME_OUT"

print_summary
exit $?
