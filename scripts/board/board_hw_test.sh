#!/bin/sh
# board_hw_test.sh - run the Aico board hardware test suites and summarize them.
#
# Copy scripts/board to the board and run it there as root:
#   scp -r scripts/board aico@192.168.42.1:/tmp/
#   ssh -t aico@192.168.42.1 'sudo sh /tmp/board/board_hw_test.sh --interactive'
set +e

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/board_hw_common.sh"
hw_start all "$@"

# Suites are found by their header line "# aico-hw-suite: <order> <name> - <text>"
# in board_hw_*.sh next to this script, so a new suite needs no change here.
# lowlevel (board_low_level_smoke.sh) always comes first.
SUITE_TABLE="$(for file in "$SCRIPT_DIR"/board_hw_*.sh; do
    sed -n "s|^# aico-hw-suite: \([0-9]*\) \([a-z0-9_-]*\) - \(.*\)$|\1 \2 $(basename "$file") \3|p" "$file" | head -n 1
done | sort -n)"
ALL_SUITES="lowlevel $(printf '%s\n' "$SUITE_TABLE" | awk 'NF {printf "%s ", $2}')"
ALL_SUITES="${ALL_SUITES% }"
SUITES="$ALL_SUITES"
REPORT=""

suite_file() {
    printf '%s\n' "$SUITE_TABLE" | awk -v name="$1" '$2 == name {print $3; exit}'
}

usage() {
    cat <<'USAGE'
Usage:
  sudo sh board_hw_test.sh [options]

Runs the board hardware suites in order and prints one summary:
USAGE
    printf '  %-12s %s\n' lowlevel "board_low_level_smoke.sh: services, HID, HDMI, audio, logs"
    printf '%s\n' "$SUITE_TABLE" | while read -r _order name file text; do
        [ -n "$name" ] && printf '  %-12s %s: %s\n' "$name" "$file" "$text"
    done
    cat <<'USAGE'

Options:
  --interactive        Include the steps that need a person at the board
                       (speech, LED confirmation, touch key, wake word, TF card)
  --only LIST          Comma-separated suites to run, e.g. audio,mcu
  --skip LIST          Comma-separated suites to skip
  --expect-size WxH    HDMI: required capture size
  --modes LIST         HDMI: switch through input modes, e.g. all or 1080p30,1080p60 (root)
  --hotplug            HDMI: cable unplug/replug (needs --interactive)
  --sleep-test         HDMI: source sleep and wake (needs --interactive)
  --charger            MCU: charger unplug/replug (needs --interactive)
  --power-key          MCU: short power-switch press (needs --interactive; only if
                       a short press does not power off the board)
  --iperf-server H[:P] Peripherals: Wi-Fi throughput against an iperf3 server
  --download-url URL   Peripherals: Wi-Fi download speed from this URL
  --ble-connect        Peripherals: wait for a phone BLE connection (needs --interactive)
  --ble-pair           Peripherals: open BLE pairing first (with --ble-connect)
  --config FILE        Settings for every suite (VARIABLE=value lines; see
                       board_hw.conf.example). Command-line options win.
  --report PATH        Report file, default <evidence dir>/report.txt
  --artifact-dir DIR   Where the evidence directory aico_hw_<run-id>/ and its
                       .tar.gz are written, default /tmp
  --run-id ID          Artifact run id, default UTC timestamp
  -h, --help           Show this help

Every run leaves evidence that it really ran on this board: per-suite
transcripts, timestamped events (UTC and seconds since boot), board identity
(boot id, serial, MACs, firmware), script hashes, the kernel log of the run,
all captures and screenshots, SHA256SUMS and a tarball whose SHA-256 is
printed at the end. See run.json in the evidence directory.

Each suite can also be run on its own; run it with --help for its options.
Exit status is 0 only when no suite reported a failure.
USAGE
}

csv_words() {
    printf '%s' "$1" | tr ',' ' '
}

# Options forwarded to one suite: ARGS_<suite>.
ARGS_hdmi=""
ARGS_mcu=""
ARGS_peripherals=""
no_spaces() {
    case "$2" in *' '*|'') die "$1 needs a value without spaces" ;; esac
}
while [ "$#" -gt 0 ]; do
    case "$1" in
        --interactive) INTERACTIVE=1; shift ;;
        --only) [ "$#" -ge 2 ] || die "$1 needs a value"; SUITES="$(csv_words "$2")"; shift 2 ;;
        --skip)
            [ "$#" -ge 2 ] || die "$1 needs a value"
            for skipped in $(csv_words "$2"); do
                SUITES="$(printf '%s\n' $SUITES | grep -vx "$skipped" | tr '\n' ' ')"
            done
            shift 2 ;;
        --hotplug|--sleep-test) ARGS_hdmi="$ARGS_hdmi $1"; shift ;;
        --expect-size|--modes) [ "$#" -ge 2 ] || die "$1 needs a value"; no_spaces "$1" "$2"; ARGS_hdmi="$ARGS_hdmi $1 $2"; shift 2 ;;
        --charger|--power-key) ARGS_mcu="$ARGS_mcu $1"; shift ;;
        --ble-connect|--ble-pair) ARGS_peripherals="$ARGS_peripherals $1"; shift ;;
        --iperf-server|--download-url) [ "$#" -ge 2 ] || die "$1 needs a value"; no_spaces "$1" "$2"; ARGS_peripherals="$ARGS_peripherals $1 $2"; shift 2 ;;
        --config) [ "$#" -ge 2 ] || die "$1 needs a value"; shift 2 ;;  # loaded and exported by hw_start
        --report) [ "$#" -ge 2 ] || die "$1 needs a value"; REPORT="$2"; shift 2 ;;
        --artifact-dir) [ "$#" -ge 2 ] || die "$1 needs a value"; AICO_LOW_LEVEL_ARTIFACT_DIR="$2"; shift 2 ;;
        --run-id) [ "$#" -ge 2 ] || die "$1 needs a value"; RUN_ID="$2"; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) die "unknown option: $1" ;;
    esac
done
for suite in $SUITES; do
    case " $ALL_SUITES " in *" $suite "*) ;; *) die "unknown suite: $suite (use: $ALL_SUITES)" ;; esac
done
export RUN_ID AICO_LOW_LEVEL_ARTIFACT_DIR
ensure_artifact_dir
[ -n "$REPORT" ] || REPORT="${AICO_HW_RUN_DIR:-$AICO_LOW_LEVEL_ARTIFACT_DIR}/report.txt"
: >"$REPORT" 2>/dev/null || die "cannot write report: $REPORT"

COMMON="--artifact-dir $AICO_LOW_LEVEL_ARTIFACT_DIR --run-id $RUN_ID"
INTER=""
[ "$INTERACTIVE" = "1" ] && INTER="--interactive"
SUMMARY=""
OVERALL=0

run_suite() {
    name="$1"
    shift
    log="$(mktemp "${TMPDIR:-/tmp}/aico_hw_suite.XXXXXX")"
    printf '\n################ %s ################\n' "$name" | tee -a "$REPORT"
    # The suites read prompts from /dev/tty, so tee does not block them.
    if [ "$name" = "lowlevel" ]; then
        # The low-level smoke scripts do not write events or a transcript
        # themselves: record both line by line while they run.
        transcript="${AICO_HW_RUN_DIR:-$AICO_LOW_LEVEL_ARTIFACT_DIR}/lowlevel.log"
        printf '# Aico hardware test transcript: suite=lowlevel run_id=%s\n# started=%s uptime_s=%s boot_id=%s\n# command: %s\n' \
            "$RUN_ID" "$(hw_now)" "$(hw_uptime)" "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)" "$*" >"$transcript"
        sh "$@" 2>&1 | (
            HW_SUITE=lowlevel
            HW_SECTION=""
            while IFS= read -r line; do
                printf '%s\n' "$line"
                hw_line_event "$line"
            done
        ) | tee -a "$transcript" | tee "$log" | tee -a "$REPORT"
        printf '# finished=%s uptime_s=%s\n' "$(hw_now)" "$(hw_uptime)" >>"$transcript"
    else
        sh "$@" 2>&1 | tee "$log" | tee -a "$REPORT"
    fi
    passes="$(grep -c '^\[PASS\]' "$log")"
    warns="$(grep -c '^\[WARN\]' "$log")"
    fails="$(grep -c '^\[FAIL\]' "$log")"
    skips="$(grep -c '^\[SKIP\]' "$log")"
    result="PASS"
    [ "$warns" -gt 0 ] && result="WARN"
    [ "$fails" -gt 0 ] && { result="FAIL"; OVERALL=1; }
    SUMMARY="$SUMMARY$(printf '%-12s %-5s pass=%-3s warn=%-3s fail=%-3s skip=%s' "$name" "$result" "$passes" "$warns" "$fails" "$skips")
"
    hw_event SUITE "$name $result pass=$passes warn=$warns fail=$fails skip=$skips"
    rm -f "$log"
}

{
    printf 'Aico board hardware test %s\n' "$RUN_ID"
    printf 'host=%s kernel=%s interactive=%s suites=%s\n' "$(hostname 2>/dev/null)" "$(uname -r)" "$INTERACTIVE" "$SUITES"
    cat /etc/aico-release 2>/dev/null
    dpkg-query -W -f='${Package} ${Version}\n' 'aico*' 2>/dev/null
} | tee -a "$REPORT"

for suite in $SUITES; do
    case "$suite" in
        lowlevel) run_suite lowlevel "$SCRIPT_DIR/board_low_level_smoke.sh" --artifact-dir "${AICO_HW_RUN_DIR:-$AICO_LOW_LEVEL_ARTIFACT_DIR}" --run-id "$RUN_ID" ;;
        *)
            extra=""
            case "$suite" in hdmi|mcu|peripherals) eval "extra=\$ARGS_$suite" ;; esac
            run_suite "$suite" "$SCRIPT_DIR/$(suite_file "$suite")" $INTER $extra $COMMON ;;
    esac
done

{
    printf '\n================ summary ================\n'
    printf '%s' "$SUMMARY"
    if [ "$OVERALL" -eq 0 ]; then
        printf 'RESULT: no failures\n'
    else
        printf 'RESULT: FAILED\n'
    fi
    printf 'report: %s\n' "$REPORT"
} | tee -a "$REPORT"
[ -n "${AICO_HW_RUN_DIR:-}" ] && echo "$OVERALL" >"$AICO_HW_RUN_DIR/.$HW_SUITE.rc"
exit "$OVERALL"
