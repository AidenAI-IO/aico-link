#!/bin/sh
# aico-hw-suite: 30 hdmi - receiver lock, capture rate, content, input modes, hotplug, source sleep
# board_hw_hdmi.sh - HDMI receiver and video capture test for an Aico board.
#
# Run ON THE BOARD as root with a powered-on HDMI source connected, next to
# board_hw_common.sh, board_low_level_common.sh and board_hw_tools.py.
set +e

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/board_hw_common.sh"
hw_start hdmi "$@"

: "${FRAME_SOCKET:=/run/frame_service/frame_service.sock}"
: "${HDMI_MODE_FILE:=/userdata/system/hdmi-mode}"
EXPECT_SIZE=""
FPS_SECONDS=5
MIN_FPS=10
HOTPLUG=0
SLEEP_TEST=0
MODES=""
SETTLE_SECONDS=90
JPEG=1

usage() {
    cat <<'USAGE'
Usage:
  sudo sh board_hw_hdmi.sh [options]

Checks the HDMI receiver (RK628 or TC358743) and the capture pipeline:
  - receiver driver probe, kernel errors and lock failures in this boot
  - /dev/video0, the HDMI subdevice, DV timings and the RK628 lock status
  - configured input mode (aico-hdmi-mode) and frame_service health
  - capture rate, frame freshness, resolution and pixel format
  - screenshot content (not black, not blank) and, with --interactive,
    that a change on the source shows up in the capture
  - with --modes: each RK628 input mode in turn (EDID switch, source
    renegotiation, lock, refresh rate, capture), then the original mode
  - with --interactive --hotplug: cable unplug/replug, loss detection,
    recovery time and the MCU HDMI plug events
  - with --interactive --sleep-test: the source display turned off with the
    cable connected (no video clock), loss detection and recovery on wake
  - the JPEG encoder path (board_jpeg_smoke.py, when present)

Options:
  --interactive           Run the steps that need a person at the source
  --modes LIST            Switch through these aico-hdmi-mode modes, e.g.
                          1080p30,1080p60; "all" means both. Restores the
                          original selection afterwards. Needs root.
  --hotplug               With --interactive, test cable unplug and replug
  --sleep-test            With --interactive, test the source going to sleep
  --settle-seconds N      Time allowed to lock after a switch or replug, default 90
  --expect-size WxH       Required capture size, e.g. 1920x1080
  --fps-seconds N         Capture-rate measurement window, default 5
  --min-fps N             Capture-rate limit, default 10
  --no-jpeg               Skip the JPEG encoder check
  --socket PATH           frame_service socket, default /run/frame_service/frame_service.sock
  --artifact-dir DIR      Screenshots are kept here, default /tmp
  --run-id ID             Artifact run id, default UTC timestamp
  --config FILE           Settings file (see board_hw.conf.example)
  -h, --help              Show this help
USAGE
}

hw_load_config

while [ "$#" -gt 0 ]; do
    case "$1" in
        --interactive) INTERACTIVE=1; shift ;;
        --modes) [ "$#" -ge 2 ] || die "$1 needs a value"; MODES="$(printf '%s' "$2" | tr ',' ' ')"; shift 2 ;;
        --hotplug) HOTPLUG=1; shift ;;
        --sleep-test) SLEEP_TEST=1; shift ;;
        --settle-seconds) [ "$#" -ge 2 ] || die "$1 needs a value"; SETTLE_SECONDS="$2"; shift 2 ;;
        --expect-size) [ "$#" -ge 2 ] || die "$1 needs a value"; EXPECT_SIZE="$2"; shift 2 ;;
        --fps-seconds) [ "$#" -ge 2 ] || die "$1 needs a value"; FPS_SECONDS="$2"; shift 2 ;;
        --min-fps) [ "$#" -ge 2 ] || die "$1 needs a value"; MIN_FPS="$2"; shift 2 ;;
        --no-jpeg) JPEG=0; shift ;;
        --socket) [ "$#" -ge 2 ] || die "$1 needs a value"; FRAME_SOCKET="$2"; shift 2 ;;
        --artifact-dir) [ "$#" -ge 2 ] || die "$1 needs a value"; AICO_LOW_LEVEL_ARTIFACT_DIR="$2"; shift 2 ;;
        --run-id) [ "$#" -ge 2 ] || die "$1 needs a value"; RUN_ID="$2"; shift 2 ;;
        --config) [ "$#" -ge 2 ] || die "$1 needs a value"; shift 2 ;;  # loaded by hw_start
        -h|--help) usage; exit 0 ;;
        *) die "unknown option: $1" ;;
    esac
done
require_uint "--fps-seconds" "$FPS_SECONDS"
require_uint "--min-fps" "$MIN_FPS"
require_uint "--settle-seconds" "$SETTLE_SECONDS"
[ "$MODES" = "all" ] && MODES="1080p30 1080p60"
for mode in $MODES; do
    case "$mode" in 1080p30|1080p60) ;; *) die "--modes accepts 1080p30, 1080p60 or all" ;; esac
done

FRAME_CLI="$(find_bin frame_service_cli)"
HDMI_MODE_BIN="$(find_bin aico-hdmi-mode)"
SUBDEV="${HDMI_SUBDEV:-}"

kernel_log() {
    journalctl -b -k --no-pager 2>/dev/null || dmesg 2>/dev/null
}

lock_failures() {
    kernel_log | grep -ciE 'rxphy lock failed|lock failed|no signal|signal lost'
}

frame_health() {
    "$FRAME_CLI" --socket "$FRAME_SOCKET" health 2>/dev/null
}

frame_meta() {
    "$FRAME_CLI" --socket "$FRAME_SOCKET" latest-frame 2>/dev/null
}

# dv_timings -> "WIDTHxHEIGHT@FPS" when the receiver reports a locked input, else empty
dv_timings() {
    [ -n "$SUBDEV" ] && command -v v4l2-ctl >/dev/null 2>&1 || return 0
    timeout 5 v4l2-ctl -d "$SUBDEV" --query-dv-timings 2>/dev/null | awk '
        /Active width:/ {w=$NF}
        /Active height:/ {h=$NF}
        /frames per second/ {for (i=1;i<=NF;i++) if ($i ~ /^\(/) {f=substr($i,2)}}
        END {if (w>0 && h>0) printf "%sx%s@%s\n", w, h, f}'
}

# Signal is lost when capture stops, frames go stale, or the receiver drops its timings.
signal_lost_now() {
    h="$(frame_health)"
    m="$(frame_meta)"
    age="$(value_of frame_age_ms "$h")"
    [ "$(value_of state "$h")" != "RUNNING" ] && return 0
    [ "$(value_of stale "$m")" = "true" ] && return 0
    [ "${age:-0}" -gt 3000 ] && return 0
    if [ -n "$SUBDEV" ] && command -v v4l2-ctl >/dev/null 2>&1 && [ -z "$(dv_timings)" ]; then
        return 0
    fi
    return 1
}

capture_ready_now() {
    h="$(frame_health)"
    m="$(frame_meta)"
    [ "$(value_of state "$h")" = "RUNNING" ] || return 1
    [ "$(value_of stale "$m")" = "false" ] || return 1
    if [ -n "$SUBDEV" ] && command -v v4l2-ctl >/dev/null 2>&1; then
        [ -n "$(dv_timings)" ] || return 1
    fi
    return 0
}

# wait_until CHECK TIMEOUT -> 0 when CHECK succeeded; WAITED holds the seconds taken
wait_until() {
    wu_start="$(date +%s)"
    while :; do
        WAITED=$(( $(date +%s) - wu_start ))
        "$1" && return 0
        [ "$WAITED" -ge "$2" ] && return 1
        sleep 1
    done
}

# measure_rate LABEL -> checks the capture rate; MEASURED_FPS holds the result
measure_rate() {
    mr_label="$1"
    first="$(value_of seq "$(frame_meta)")"
    seqs=""
    calls=0
    start="$(date +%s)"
    while [ $(( $(date +%s) - start )) -lt "$FPS_SECONDS" ]; do
        seqs="$seqs $(value_of seq "$(frame_meta)")"
        calls=$((calls + 1))
    done
    elapsed=$(( $(date +%s) - start ))
    unique="$(printf '%s\n' $seqs | sort -u | grep -c .)"
    last="$(printf '%s\n' $seqs | sort -n | tail -n 1)"
    MEASURED_FPS=0
    if [ -z "$first" ] || [ -z "$last" ] || [ "$elapsed" -le 0 ]; then
        fail "$mr_label: could not read frame sequence numbers"
        return 1
    fi
    MEASURED_FPS=$(( (last - first) / elapsed ))
    printf '    %s: %s requests in %ss, %s distinct frames, seq %s -> %s\n' "$mr_label" "$calls" "$elapsed" "$unique" "$first" "$last"
    if [ "$MEASURED_FPS" -ge "$MIN_FPS" ]; then
        pass "$mr_label: capture advances at ~$MEASURED_FPS frames/s (>= $MIN_FPS)"
    elif [ "$(value_of capture_mode "$(frame_health)")" = "on_demand" ] && [ "$unique" -ge "$calls" ]; then
        pass "$mr_label: on-demand capture returned a fresh frame for every request ($unique/$calls)"
    else
        fail "$mr_label: capture advances at ~$MEASURED_FPS frames/s (< $MIN_FPS)"
    fi
}

# check_frame LABEL EXPECTED_SIZE
check_frame() {
    meta="$(frame_meta)"
    printf '    %s: %s\n' "$1" "$meta"
    size="$(value_of width "$meta")x$(value_of height "$meta")"
    [ "$(value_of stale "$meta")" = "false" ] && pass "$1: latest frame is fresh" || fail "$1: latest frame is stale"
    case "$size" in
        x|0x*|*x0) fail "$1: latest frame has no size ($size)" ;;
        *) pass "$1: capture size $size, pixel format $(value_of pixel_format "$meta")" ;;
    esac
    if [ -n "$2" ]; then
        [ "$size" = "$2" ] && pass "$1: capture size matches $2" || fail "$1: capture size $size, expected $2"
    fi
}

# screenshot_check LABEL NAME -> path of the screenshot in SHOT
screenshot_check() {
    SHOT="$(hw_artifact_path "$2.bmp")"
    if "$FRAME_CLI" --socket "$FRAME_SOCKET" screenshot --out "$SHOT" >/dev/null 2>&1 && [ -s "$SHOT" ]; then
        hw_tool frame-stats "$SHOT"
        print_artifact "$1" "$SHOT"
        return 0
    fi
    fail "$1: frame_service screenshot failed"
    SHOT=""
    return 1
}

# loss_and_recovery LABEL LOSE_PROMPT RESTORE_PROMPT
loss_and_recovery() {
    prompt_enter "$2" || { skip "$1 (no operator input)"; return 0; }
    if wait_until signal_lost_now 30; then
        pass "$1: signal loss detected after ~${WAITED}s (state $(value_of state "$h"), stale $(value_of stale "$m"), frame age $(value_of frame_age_ms "$h") ms)"
    else
        fail "$1: no signal loss reported within 30s"
    fi
    prompt_enter "$3" || { skip "$1 recovery (no operator input)"; return 0; }
    if wait_until capture_ready_now "$SETTLE_SECONDS"; then
        pass "$1: capture recovered ${WAITED}s after restoring the signal"
        [ "$WAITED" -gt 30 ] && warn "$1: recovery took over 30s (EDID renegotiation can take ~30s)"
        screenshot_check "$1 after recovery" "hdmi_$(printf '%s' "$1" | tr ' ' '_')_recovered" >/dev/null
    else
        fail "$1: capture did not recover within ${SETTLE_SECONDS}s"
    fi
}

section "receiver driver"
warn_if_not_root
require_python
probe="$(kernel_log | grep -iE 'rk628|tc358743' | grep -iE 'probe|RK628F|RK628D|version|20[0-9]{6}|chip id' | tail -n 3)"
if [ -n "$probe" ]; then
    pass "HDMI receiver driver loaded"
    printf '%s\n' "$probe" | indent
else
    fail "no RK628/TC358743 probe message in this boot's kernel log"
fi
errors="$(kernel_log | grep -iE 'rk628|tc358743|rkcif|cif' | grep -iE 'error|fail|timeout|oops' | tail -n 5)"
if [ -n "$errors" ]; then
    warn "receiver/CIF errors in this boot (latest 5):"
    printf '%s\n' "$errors" | indent
else
    pass "no receiver/CIF errors in this boot's kernel log"
fi
LOCK_FAILS_BEFORE="$(lock_failures)"
printf '    lock-failure lines this boot: %s\n' "$LOCK_FAILS_BEFORE"

section "capture devices"
check_exists "video device" /dev/video0
if [ -z "$SUBDEV" ] && command -v media-ctl >/dev/null 2>&1; then
    SUBDEV="$(media-ctl -p 2>/dev/null | awk '/entity .*(rk628|tc358743|m00)/{f=1} f && /device node name/{print $NF; exit}')"
fi
[ -z "$SUBDEV" ] && [ -e /dev/v4l-subdev2 ] && SUBDEV=/dev/v4l-subdev2
if [ -n "$SUBDEV" ]; then
    pass "HDMI subdevice: $SUBDEV"
    if command -v v4l2-ctl >/dev/null 2>&1; then
        timings="$(dv_timings)"
        [ -n "$timings" ] && pass "DV timings locked: $timings" \
            || fail "no locked DV timings on $SUBDEV (source asleep, cable or receiver lock problem)"
    else
        skip "DV timings (v4l2-ctl not installed)"
    fi
else
    warn "HDMI subdevice not found (media-ctl -p)"
fi
for status in /sys/kernel/debug/rk628/*/hdmirx/status; do
    [ -r "$status" ] || continue
    content="$(cat "$status" 2>/dev/null)"
    printf '%s\n' "$content" | head -n 12 | indent
    if printf '%s' "$content" | grep -qiE 'unlock|not lock|no lock|fail'; then
        fail "RK628 HDMI RX status reports an unlocked clock or data channel ($status)"
    else
        pass "RK628 HDMI RX status shows no unlocked channel ($status)"
    fi
done
if [ -n "$HDMI_MODE_BIN" ]; then
    mode="$("$HDMI_MODE_BIN" status 2>/dev/null | head -n 1)"
    [ -n "$mode" ] && pass "$mode" || warn "aico-hdmi-mode status returned nothing"
fi

section "frame_service"
if [ -z "$FRAME_CLI" ]; then
    fail "frame_service_cli not found"
    hw_finish
fi
health="$(frame_health)"
printf '%s\n' "$health" | indent
[ "$(value_of state "$health")" = "RUNNING" ] && pass "frame_service state RUNNING" || fail "frame_service state is '$(value_of state "$health")'"
failures="$(value_of consecutive_failures "$health")"
[ "${failures:-0}" = "0" ] && pass "no consecutive capture failures" || warn "consecutive capture failures: $failures"

section "capture rate and freshness"
measure_rate "current mode"
check_frame "current mode" "$EXPECT_SIZE"

section "screenshot content"
if screenshot_check "screenshot" hdmi_a; then
    shot_a="$SHOT"
    if interactive_or_skip "source-change detection"; then
        if prompt_enter "Change what the source shows (open another app or page)."; then
            shot_b="$(hw_artifact_path hdmi_b.bmp)"
            "$FRAME_CLI" --socket "$FRAME_SOCKET" screenshot --out "$shot_b" >/dev/null 2>&1
            hw_tool frame-stats "$shot_b" --compare "$shot_a" --expect-change
            confirm_check "capture matches the source" "Open $shot_b on a computer (or trust the stats): does it show the new screen without tearing, colour shift or garbage?"
        else
            skip "source-change detection (no operator input)"
        fi
    fi
fi

MODE_RESTORE_PENDING=0
# restore_mode - put back the input-mode selection found before --modes.
# Returns nonzero when the selection could not be restored.
# The pending flag is cleared only once restoration succeeds, so an exit or
# signal trap retries a failed or interrupted restoration.
restore_mode() {
    [ "$MODE_RESTORE_PENDING" = "1" ] || return 0
    if [ -n "$ORIGINAL_MODE" ]; then
        "$HDMI_MODE_BIN" "$ORIGINAL_MODE" >/dev/null 2>&1 || return 1
    else
        { rm -f "$HDMI_MODE_FILE" && systemctl restart aico-frame.service >/dev/null 2>&1; } || return 1
    fi
    MODE_RESTORE_PENDING=0
}

if [ -n "$MODES" ]; then
    section "input modes"
    if ! is_root; then
        skip "input-mode switching (needs root)"
    elif [ -z "$HDMI_MODE_BIN" ]; then
        skip "input-mode switching (aico-hdmi-mode not installed; TC358743 boards have no mode profiles)"
    else
        if [ -f "$HDMI_MODE_FILE" ]; then
            ORIGINAL_MODE="$(tr -d '[:space:]' <"$HDMI_MODE_FILE")"
        else
            ORIGINAL_MODE=""
        fi
        printf '    original selection: %s\n' "${ORIGINAL_MODE:-default (no saved mode)}"
        # Restore the selection on every exit path, including an interrupted run.
        MODE_RESTORE_PENDING=1
        trap 'restore_mode >/dev/null; exit 130' INT TERM HUP
        trap 'restore_mode >/dev/null' EXIT
        for mode in $MODES; do
            expected_fps="${mode#1080p}"
            if "$HDMI_MODE_BIN" "$mode" >/dev/null 2>&1; then
                pass "$mode: selected and capture restarted"
            else
                fail "$mode: aico-hdmi-mode $mode failed"
                continue
            fi
            if wait_until capture_ready_now "$SETTLE_SECONDS"; then
                pass "$mode: locked and capturing ${WAITED}s after the switch"
            else
                fail "$mode: no locked capture within ${SETTLE_SECONDS}s (the source may not support this mode)"
                continue
            fi
            timings="$(dv_timings)"
            if [ -n "$timings" ]; then
                rate="${timings##*@}"
                if python3 -c 'import sys; sys.exit(0 if abs(float(sys.argv[1]) - float(sys.argv[2])) <= 2 else 1)' "${rate:-0}" "$expected_fps" 2>/dev/null; then
                    pass "$mode: receiver timings $timings match ${expected_fps} Hz"
                else
                    fail "$mode: receiver timings $timings, expected ${expected_fps} Hz"
                fi
            fi
            check_frame "$mode" "${EXPECT_SIZE:-1920x1080}"
            measure_rate "$mode"
            screenshot_check "$mode screenshot" "hdmi_$mode" >/dev/null
        done
        if ! restore_mode; then
            fail "could not restore the original selection (${ORIGINAL_MODE:-default}); set it with: sudo aico-hdmi-mode ${ORIGINAL_MODE:-1080p60}"
        elif wait_until capture_ready_now "$SETTLE_SECONDS"; then
            pass "restored the original selection (${ORIGINAL_MODE:-default}) and capture recovered in ${WAITED}s"
        else
            fail "restored the original selection (${ORIGINAL_MODE:-default}) but capture did not recover within ${SETTLE_SECONDS}s"
        fi
        # After a failed restoration the traps stay, so exit retries it.
        [ "$MODE_RESTORE_PENDING" = "1" ] || trap - INT TERM HUP EXIT
    fi
fi

if [ "$HOTPLUG" = "1" ] && interactive_or_skip "HDMI hotplug"; then
    section "HDMI hotplug"
    events_log="$(hw_artifact_path hdmi_events.log)"
    events_pid=""
    if [ -S /run/mcu_service/mcu_service.sock ]; then
        python3 "$HW_TOOLS" mcu-events --expect hdmi_removed,hdmi_inserted --seconds $((SETTLE_SECONDS + 90)) >"$events_log" 2>&1 &
        events_pid=$!
    fi
    loss_and_recovery "cable unplug" "Unplug the HDMI cable from the board, then press Enter." \
        "Plug the HDMI cable back in, then press Enter."
    if [ -n "$events_pid" ]; then
        kill "$events_pid" 2>/dev/null
        wait "$events_pid" 2>/dev/null
        grep -E '^    event' "$events_log" | indent
        grep -q '"inserted": false' "$events_log" && pass "MCU reported the HDMI unplug" || warn "MCU did not report the HDMI unplug (no plug-detect wiring?)"
        grep -q '"inserted": true' "$events_log" && pass "MCU reported the HDMI replug" || warn "MCU did not report the HDMI replug"
    fi
fi

if [ "$SLEEP_TEST" = "1" ] && interactive_or_skip "source sleep"; then
    section "source sleep and wake"
    printf '    Keep the cable connected. A sleeping source can stay enumerated while\n'
    printf '    sending no video clock; the receiver must report no signal.\n'
    loss_and_recovery "source sleep" \
        "Turn the source display off or let it sleep (lock screen, power button, or sleep), keeping the cable plugged in, then press Enter." \
        "Wake the source and unlock its screen, then press Enter."
fi

if [ "$JPEG" = "1" ] && [ -f "$SCRIPT_DIR/board_jpeg_smoke.py" ]; then
    section "JPEG encoder path"
    run_warn_check "board_jpeg_smoke.py (3 iterations)" python3 "$SCRIPT_DIR/board_jpeg_smoke.py" \
        --socket "$FRAME_SOCKET" --iterations 3 --output-dir "$(hw_artifact_path jpeg)"
fi

section "lock stability"
new=$(( $(lock_failures) - LOCK_FAILS_BEFORE ))
if [ "$HOTPLUG" = "1" ] || [ "$SLEEP_TEST" = "1" ] || [ -n "$MODES" ]; then
    printf '    lock-failure lines during the test: %s (expected during unplug, sleep or mode switches)\n' "$new"
elif [ "$new" -gt 0 ]; then
    warn "$new new HDMI lock-failure lines during the test"
else
    pass "no new HDMI lock failures during the test"
fi

hw_finish
