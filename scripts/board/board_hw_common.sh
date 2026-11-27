#!/bin/sh
# Shared helpers for the board hardware test suites (board_hw_*.sh).
# Builds on board_low_level_common.sh: same [PASS]/[WARN]/[FAIL]/[SKIP] lines,
# counters, run ids and artifact paths, plus interactive prompts, root checks,
# the Python helper board_hw_tools.py, and the run evidence bundle.
#
# Evidence bundle: every run writes <artifact-dir>/aico_hw_<run-id>/ with
#   <suite>.log     full console transcript of each suite
#   events.jsonl    every check result, prompt and operator answer with UTC
#                   time, seconds since boot and the section it belongs to
#   failures.txt    every FAIL and WARN with time, suite, section and the
#                   detail lines printed under it
#   run.json        board identity (boot id, serial, MACs, firmware, packages),
#                   clock state (NTP sync, RTC), start/end, arguments, script
#                   hashes, per-suite results and times, failures and artifact
#                   hashes
#   kernel.log      kernel messages logged during the run
#   captures, screenshots and measurements written by the suites
#   SHA256SUMS      hashes of every file in the bundle
# and aico_hw_<run-id>.tar.gz next to it, whose SHA-256 is printed at the end.
#
# Writing a new suite: name it board_hw_<name>.sh, add a header line
#   # aico-hw-suite: <order> <name> - <one-line description>
# source this file, call hw_start <name> "$@", set defaults, hw_load_config,
# parse options (accept --config, --artifact-dir, --run-id, --interactive),
# report with pass/warn/fail/skip and end with hw_finish. board_hw_test.sh
# finds it by the header line; no other file needs to change.

if [ -n "${AICO_HW_COMMON_LOADED:-}" ]; then
    return 0 2>/dev/null || exit 0
fi
AICO_HW_COMMON_LOADED=1

: "${SCRIPT_DIR:=$(CDPATH= cd -- "$(dirname "$0")" && pwd)}"
. "$SCRIPT_DIR/board_low_level_common.sh"

: "${INTERACTIVE:=0}"
HW_TOOLS="$SCRIPT_DIR/board_hw_tools.py"
HW_SUITE="${HW_SUITE:-}"
HW_EVENTS=""

# ------------------------------------------------------------ evidence log

hw_now() {
    date -u +%Y-%m-%dT%H:%M:%SZ
}

hw_uptime() {
    cut -d' ' -f1 /proc/uptime 2>/dev/null || echo null
}

hw_json_escape() {
    printf '%s' "$1" | tr -d '\000-\010\013-\037' | sed 's/\\/\\\\/g; s/"/\\"/g' | tr '\n\t' '  '
}

HW_SECTION=""

# hw_event RESULT MESSAGE -> one JSON line in events.jsonl
hw_event() {
    [ -n "$HW_EVENTS" ] || return 0
    printf '{"time":"%s","uptime_s":%s,"suite":"%s","section":"%s","result":"%s","message":"%s"}\n' \
        "$(hw_now)" "$(hw_uptime)" "$HW_SUITE" "$(hw_json_escape "$HW_SECTION")" "$1" "$(hw_json_escape "$2")" >>"$HW_EVENTS"
}

# hw_line_event LINE - record a console line of a script that does not write
# events itself ("== section ==" and "[RESULT] message" lines), as it happens.
hw_line_event() {
    case "$1" in
        "== "*" ==") HW_SECTION="${1#== }"; HW_SECTION="${HW_SECTION% ==}"; hw_event SECTION "$HW_SECTION" ;;
        "[PASS] "*|"[WARN] "*|"[FAIL] "*|"[SKIP] "*) hw_line_result="${1#\[}"; hw_event "${hw_line_result%%\]*}" "${1#\[*\] }" ;;
    esac
}

# The result functions of board_low_level_common.sh, plus an event per line.
section() {
    printf '\n== %s ==\n' "$*"
    HW_SECTION="$*"
    hw_event SECTION "$*"
}

pass() {
    printf '[PASS] %s\n' "$*"
    hw_event PASS "$*"
}

warn() {
    WARNS=$((WARNS + 1))
    printf '[WARN] %s\n' "$*"
    hw_event WARN "$*"
}

fail() {
    FAILS=$((FAILS + 1))
    printf '[FAIL] %s\n' "$*"
    hw_event FAIL "$*"
}

skip() {
    printf '[SKIP] %s\n' "$*"
    hw_event SKIP "$*"
}

# hw_start SUITE "$@" - call right after sourcing this file, before parsing.
# Runs the suite again with its output copied to <run dir>/<suite>.log, then
# (in the outermost invocation) writes run.json, kernel.log and SHA256SUMS.
hw_start() {
    HW_SUITE="$1"
    shift
    for hw_arg in "$@"; do
        case "$hw_arg" in -h|--help) return 0 ;; esac
    done
    hw_prev=""
    hw_run_id_given=0
    for hw_arg in "$@"; do
        case "$hw_prev" in
            --artifact-dir) AICO_LOW_LEVEL_ARTIFACT_DIR="$hw_arg" ;;
            --run-id) RUN_ID="$hw_arg"; hw_run_id_given=1 ;;
            --config) AICO_HW_CONFIG="$hw_arg" ;;
        esac
        hw_prev="$hw_arg"
    done
    if [ -n "${AICO_HW_CONFIG:-}" ]; then
        [ -r "$AICO_HW_CONFIG" ] || die "cannot read config file: $AICO_HW_CONFIG"
        AICO_HW_CONFIG="$(CDPATH= cd -- "$(dirname "$AICO_HW_CONFIG")" && pwd)/$(basename "$AICO_HW_CONFIG")"
        export AICO_HW_CONFIG
    fi
    if [ -z "${AICO_HW_RUN_DIR:-}" ]; then
        # The outermost run creates a fresh directory that only its owner can
        # write, and never adopts an existing one: under a shared parent such
        # as /tmp another user could have prepared it. Child suites inherit it.
        # A default (timestamp) id that is taken gets this process's id appended.
        if [ "$hw_run_id_given" = "0" ] && { [ -e "$AICO_LOW_LEVEL_ARTIFACT_DIR/aico_hw_$RUN_ID" ] || [ -L "$AICO_LOW_LEVEL_ARTIFACT_DIR/aico_hw_$RUN_ID" ]; }; then
            RUN_ID="$RUN_ID-$$"
        fi
        AICO_HW_RUN_DIR="$AICO_LOW_LEVEL_ARTIFACT_DIR/aico_hw_$RUN_ID"
        mkdir -p "$AICO_LOW_LEVEL_ARTIFACT_DIR" 2>/dev/null || die "cannot create artifact directory: $AICO_LOW_LEVEL_ARTIFACT_DIR"
        [ -e "$AICO_HW_RUN_DIR" ] || [ -L "$AICO_HW_RUN_DIR" ] &&
            die "evidence directory already exists: $AICO_HW_RUN_DIR (use another --run-id)"
        (umask 077 && mkdir "$AICO_HW_RUN_DIR") 2>/dev/null || die "cannot create evidence directory: $AICO_HW_RUN_DIR"
    fi
    [ -d "$AICO_HW_RUN_DIR" ] && [ ! -L "$AICO_HW_RUN_DIR" ] || die "evidence directory missing: $AICO_HW_RUN_DIR"
    export AICO_HW_RUN_DIR RUN_ID
    HW_EVENTS="$AICO_HW_RUN_DIR/events.jsonl"

    if [ "${AICO_HW_TRANSCRIPT_SUITE:-}" = "$HW_SUITE" ]; then
        return 0  # already running inside our own transcript
    fi
    export AICO_HW_TRANSCRIPT_SUITE="$HW_SUITE"
    hw_outer=0
    if [ -z "${AICO_HW_OUTER:-}" ]; then
        export AICO_HW_OUTER="$HW_SUITE"
        hw_outer=1
        date -u +%s >"$AICO_HW_RUN_DIR/.start_epoch"
    fi
    hw_rc_file="$AICO_HW_RUN_DIR/.$HW_SUITE.rc"
    rm -f "$hw_rc_file"
    {
        printf '# Aico hardware test transcript: suite=%s run_id=%s\n' "$HW_SUITE" "$RUN_ID"
        printf '# started=%s uptime_s=%s boot_id=%s host=%s user=%s\n' "$(hw_now)" "$(hw_uptime)" \
            "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)" "$(hostname 2>/dev/null)" "${SUDO_USER:-$(id -un)}"
        printf '# command: %s' "$0"
        for hw_arg in "$@"; do printf ' %s' "$hw_arg"; done
        printf '\n'
    } >>"$AICO_HW_RUN_DIR/$HW_SUITE.log"
    hw_event START "$0 $*"
    sh "$0" "$@" 2>&1 | tee -a "$AICO_HW_RUN_DIR/$HW_SUITE.log"
    hw_rc="$(cat "$hw_rc_file" 2>/dev/null)"
    [ -n "$hw_rc" ] || hw_rc=2
    rm -f "$hw_rc_file"
    printf '# finished=%s uptime_s=%s exit=%s\n' "$(hw_now)" "$(hw_uptime)" "$hw_rc" >>"$AICO_HW_RUN_DIR/$HW_SUITE.log"
    hw_event END "exit=$hw_rc"
    if [ "$hw_outer" = "1" ]; then
        if ! python3 "$HW_TOOLS" evidence-finalize --run-dir "$AICO_HW_RUN_DIR" --run-id "$RUN_ID" \
            --entry "$HW_SUITE" --exit-code "$hw_rc" --script-dir "$SCRIPT_DIR" -- "$0" "$@"; then
            printf '[FAIL] evidence bundle incomplete: finalizing %s failed\n' "$AICO_HW_RUN_DIR"
            [ "$hw_rc" -ne 0 ] || hw_rc=1
        fi
    fi
    exit "$hw_rc"
}

# hw_load_config - call after a suite sets its defaults and before it parses
# options, so the precedence is: defaults < config file < command line.
# The config file (--config FILE or AICO_HW_CONFIG) is a plain shell file of
# VARIABLE=value lines; see board_hw.conf.example for every variable.
hw_load_config() {
    [ -n "${AICO_HW_CONFIG:-}" ] || return 0
    # shellcheck disable=SC1090
    . "$AICO_HW_CONFIG" || die "config file failed to load: $AICO_HW_CONFIG"
}

# End of a suite: print the summary and hand the exit status to hw_start.
hw_finish() {
    print_summary
    hw_finish_rc=$?
    if [ -n "${AICO_HW_RUN_DIR:-}" ] && [ -n "$HW_SUITE" ]; then
        echo "$hw_finish_rc" >"$AICO_HW_RUN_DIR/.$HW_SUITE.rc"
    fi
    exit "$hw_finish_rc"
}

# ------------------------------------------------------------ helpers

# Count the [PASS]/[WARN]/[FAIL] lines a helper printed into the suite totals.
hw_tool() {
    hw_tool_out="$(mktemp "${TMPDIR:-/tmp}/aico_hw_tool.XXXXXX")" || {
        fail "cannot create temporary log"
        return 1
    }
    python3 "$HW_TOOLS" "$@" >"$hw_tool_out" 2>&1
    hw_tool_rc=$?
    cat "$hw_tool_out"
    hw_tool_fails="$(grep -c '^\[FAIL\]' "$hw_tool_out" 2>/dev/null)"
    hw_tool_warns="$(grep -c '^\[WARN\]' "$hw_tool_out" 2>/dev/null)"
    FAILS=$((FAILS + ${hw_tool_fails:-0}))
    WARNS=$((WARNS + ${hw_tool_warns:-0}))
    if [ -n "$HW_EVENTS" ]; then
        while IFS= read -r hw_line; do
            case "$hw_line" in "== "*) ;; *) hw_line_event "$hw_line" ;; esac
        done <"$hw_tool_out"
    fi
    if [ "$hw_tool_rc" -ne 0 ] && [ "${hw_tool_fails:-0}" -eq 0 ]; then
        fail "board_hw_tools.py $1 exited with rc=$hw_tool_rc"
    fi
    rm -f "$hw_tool_out"
    return "$hw_tool_rc"
}

require_python() {
    if ! command -v python3 >/dev/null 2>&1; then
        fail "python3 not found; the analysis helpers need it"
        return 1
    fi
    if [ ! -f "$HW_TOOLS" ]; then
        fail "helper missing: $HW_TOOLS (copy it next to this script)"
        return 1
    fi
    return 0
}

is_root() {
    # AICO_HW_TEST_ROOT lets the host test exercise root-only paths with fakes.
    [ "${AICO_HW_TEST_ROOT:-0}" = "1" ] || [ "$(id -u)" -eq 0 ]
}

warn_if_not_root() {
    is_root || warn "not running as root; some checks will be skipped or fail (run with sudo)"
}

# Interactive steps need a person at the board. Without --interactive they
# are reported as skipped so an unattended run never waits for input.
interactive_or_skip() {
    if [ "$INTERACTIVE" = "1" ]; then
        return 0
    fi
    skip "$1 (needs --interactive)"
    return 1
}

# Operator answers come from the terminal, or from stdin when there is none.
# A failed read (no terminal, stdin closed) is lost input, not an answer: it
# stops the remaining interactive steps instead of being taken as yes or no.
HW_INPUT_LOST=0

hw_read_answer() {
    hw_answer=""
    if [ "$HW_INPUT_LOST" = "1" ]; then
        return 1
    fi
    if { : </dev/tty; } 2>/dev/null; then
        read -r hw_answer </dev/tty && return 0
    elif read -r hw_answer; then
        return 0
    fi
    HW_INPUT_LOST=1
    INTERACTIVE=0
    hw_event ANSWER "no input (terminal unavailable or closed)"
    fail "operator input unavailable; the remaining interactive steps are skipped (run with ssh -t)"
    return 1
}

# Returns 0 when the operator pressed Enter, 1 when no input is available.
prompt_enter() {
    printf '\n>>> %s\n>>> Press Enter when ready... ' "$1"
    hw_event PROMPT "$1"
    hw_read_answer || return 1
    hw_event ANSWER "Enter"
}

# Returns 0 for yes, 1 for no, 2 when no input is available.
ask_yes_no() {
    hw_event PROMPT "$1"
    while :; do
        printf '\n??? %s [y/n] ' "$1"
        hw_read_answer || return 2
        case "$hw_answer" in
            y|Y|yes|YES) hw_event ANSWER "yes"; return 0 ;;
            n|N|no|NO) hw_event ANSWER "no"; return 1 ;;
        esac
    done
}

confirm_check() {
    aico_confirm_name="$1"
    ask_yes_no "$2"
    case $? in
        0) pass "$aico_confirm_name (confirmed by operator)" ;;
        1) fail "$aico_confirm_name (operator reported a problem)" ;;
        *) skip "$aico_confirm_name (no operator answer)" ;;
    esac
}

# value_of KEY "k1=v1 k2=v2 ..." -> v
value_of() {
    printf '%s\n' "$2" | tr ' ' '\n' | sed -n "s/^$1=//p" | head -n 1
}

# Captures and screenshots go into the evidence bundle.
hw_artifact_path() {
    if [ -n "${AICO_HW_RUN_DIR:-}" ]; then
        printf '%s/%s\n' "$AICO_HW_RUN_DIR" "$1"
    else
        artifact_path "hw_$1"
    fi
}
