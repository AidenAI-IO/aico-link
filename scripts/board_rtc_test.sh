#!/usr/bin/env bash
# board_rtc_test.sh - RTC and system-clock test for an Aico board.
#
# Runs ON A HOST connected to the board over USB. It drives the board through
# the Agent shell tool (http://<board>:8080, runs as root), so it works on a
# freshly flashed board without an SSH key or sudo password. Firmware that
# requires sign-in is paired over USB (POST /api/auth/pair) and the session
# token is kept in the report directory with mode 0600.
# Host requirements: bash, curl, jq, and an NTP-synchronized clock (the host
# clock is the reference).
#
# This complements scripts/test_debian_rtc.sh, which unit-tests the
# aico-rtc helper on the build host; this script checks the real hardware.
#
# Usage:
#   scripts/board_rtc_test.sh check          RTC device, ticking, RTC vs system and host
#                                            time, kernel HCTOSYS/SYSTOHC, aico-rtc service
#                                            result and its decision paths (with stubbed
#                                            hwclock/date; the real RTC is not changed),
#                                            RTC alarm interrupt
#   scripts/board_rtc_test.sh reboot         warm reboot; the RTC must keep time
#   scripts/board_rtc_test.sh poweroff-pre   record a baseline, then shut the board down
#   scripts/board_rtc_test.sh poweroff-post  after power-on (ideally before Wi-Fi/NTP):
#                                            did the RTC keep time while the board was off?
#   scripts/board_rtc_test.sh all            check + reboot
#
# Environment: AICO_BOARD (default 192.168.42.1), RTC_TOL allowed error in
# seconds (default 10). Reports and the power-off baseline are written to
# ~/.cache/aico-rtc-test/. The exit status is non-zero when any check fails.
set -u

BOARD=${AICO_BOARD:-192.168.42.1}
AGENT="http://${BOARD}:8080"
TOL=${RTC_TOL:-10}
DIR="${HOME}/.cache/aico-rtc-test"
STATE="${DIR}/${BOARD}.poweroff"
mkdir -p "$DIR"
REPORT="${DIR}/report-$(date -u +%Y%m%dT%H%M%SZ)-${1:-help}.txt"
PASS=0; FAIL=0; WARN=0

say()  { printf '%s\n' "$*" | tee -a "$REPORT"; }
pass() { PASS=$((PASS+1)); say "  PASS  $*"; }
fail() { FAIL=$((FAIL+1)); say "  FAIL  $*"; }
warn() { WARN=$((WARN+1)); say "  WARN  $*"; }
fmt()  { date -u -r "$1" +%FT%TZ 2>/dev/null || date -u -d "@$1" +%FT%TZ 2>/dev/null || echo "$1"; }
absd() { local v=$1; [ "$v" -lt 0 ] && v=$(( -v )); echo "$v"; }

TOKEN_FILE="${DIR}/${BOARD}.token"
TOKEN=""
[ -s "$TOKEN_FILE" ] && TOKEN=$(cat "$TOKEN_FILE")

# Board requests go straight to the board, never through a host proxy.
bcurl() {
    if [ -n "$TOKEN" ]; then
        curl -s --noproxy '*' -H "Authorization: Bearer ${TOKEN}" "$@"
    else
        curl -s --noproxy '*' "$@"
    fi
}

# Pair over USB when the firmware requires sign-in. Pairing needs no password
# and works only on the USB link, so it never touches a LAN-reachable board.
ensure_auth() {
    local code resp
    code=$(bcurl -m 6 -o /dev/null -w '%{http_code}' "${AGENT}/api/history" 2>/dev/null)
    [ "$code" = 401 ] || return 0
    resp=$(curl -s --noproxy '*' -m 10 -X POST -H "Origin: http://${BOARD}" "http://${BOARD}/api/auth/pair")
    TOKEN=$(printf '%s' "$resp" | jq -r '.token // empty' 2>/dev/null)
    if [ -z "$TOKEN" ]; then
        echo "sign-in required and USB pairing failed: $(printf '%s' "$resp" | jq -r '.error // .message // empty' 2>/dev/null)" >&2
        return 1
    fi
    ( umask 077; printf '%s\n' "$TOKEN" > "$TOKEN_FILE" )
}

# Run a command on the board as root; only return stdout from successful calls.
rsh() {
    local response
    response=$(bcurl -f -m "${2:-90}" -X POST -H 'Content-Type: application/json' "${AGENT}/api/tools/shell" \
        -d "$(jq -nc --arg c "$1" '{input:{command:$c}}')") || return 1
    jq -er 'select(.is_error != true) | .output | strings' <<< "$response"
}
val() { printf '%s\n' "$OUT" | sed -n "s/^$1=//p" | head -1; }
agent_up() { bcurl -m 4 -o /dev/null -w '%{http_code}' "${AGENT}/api/history" 2>/dev/null | grep -q 200; }
now_ms() { python3 -c 'import time; print(int(time.time()*1000))' 2>/dev/null || echo "$(date -u +%s)000"; }
boot_id() {
    local id
    id=$(rsh 'cat /proc/sys/kernel/random/boot_id 2>/dev/null' "${1:-15}") || return 0
    if [[ $id =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]]; then
        printf '%s\n' "$id"
    fi
}

for tool in curl jq; do
    command -v "$tool" >/dev/null || { echo "missing host tool: $tool" >&2; exit 2; }
done

# Kernel line: "[    5.14] ... setting system clock to <iso> UTC (<epoch>)".
# Reads the logs only; the timing samples come from a separate short request.
BOOT_PROBE='
r=/sys/class/rtc/rtc0
line=$( (journalctl -b -k -o short-monotonic --no-pager 2>/dev/null; dmesg 2>/dev/null) | grep -m1 "setting system clock to")
echo "boot_epoch=$(printf "%s" "$line" | sed -n "s/.*UTC (\([0-9]*\)).*/\1/p")"
echo "boot_mono=$(printf "%s" "$line" | sed -n "s/^\[ *\([0-9]*\)\..*/\1/p")"
echo "sys=$(date -u +%s)"
echo "rtc=$(cat $r/since_epoch 2>/dev/null)"
echo "ntp=$(timedatectl show -p NTPSynchronized --value 2>/dev/null)"
echo "bad_alarm=$( (journalctl -b -k --no-pager 2>/dev/null; dmesg 2>/dev/null) | grep -c "invalid alarm value")"
echo "baseline=$(sed -n "s/^default_date=\${RTC_DEFAULT_DATE:-\(.*\)}/\1/p" /usr/lib/aico/aico-rtc 2>/dev/null)"
true'

# One short request that samples the board clocks, bracketed by host times.
# Sets T0/T1 (host ms before/after), HOST_MID (host s at the request midpoint)
# and SAMPLE (the board output); the board sampled somewhere between T0 and T1.
sample_clock() {
    T0=$(now_ms)
    SAMPLE=$(rsh 'echo "uptime=$(cut -d" " -f1 /proc/uptime)"; echo "sys=$(date -u +%s)"' 15)
    T1=$(now_ms)
    HOST_MID=$(( (T0 + T1) / 2000 ))
}
sval() { printf '%s\n' "$SAMPLE" | sed -n "s/^$1=//p" | head -1; }

# Compare the RTC value the kernel loaded at boot with the host clock at that moment.
check_boot_rtc() {
    local label=$1 boot_epoch boot_mono uptime expected diff slack
    OUT=$(rsh "$BOOT_PROBE")
    sample_clock
    uptime=$(sval uptime); uptime=${uptime%%.*}
    boot_epoch=$(val boot_epoch); boot_mono=$(val boot_mono)
    if [ -z "$boot_epoch" ] || [ -z "$uptime" ]; then
        fail "$label: kernel did not report loading the system clock from the RTC"
        return 1
    fi
    # The board read its uptime at an unknown moment within the request, so
    # half the round trip (rounded up) widens the allowed error.
    slack=$(( (T1 - T0 + 1999) / 2000 ))
    expected=$(( HOST_MID - uptime + ${boot_mono:-0} ))
    diff=$(( boot_epoch - expected ))
    say "  info  kernel loaded RTC time $(fmt "$boot_epoch") at ${boot_mono:-?}s after boot; host time then $(fmt "$expected") (diff ${diff}s, request $(( T1 - T0 ))ms)"
    if [ "$(absd "$diff")" -le $(( TOL + slack )) ]; then
        pass "$label: RTC kept the time (within ${TOL}s)"
        return 0
    fi
    fail "$label: RTC time at boot is off by ${diff}s"
    [ "$(val bad_alarm)" != "0" ] && say "  info  kernel also reported an invalid RTC alarm value: the RTC lost power and was reset"
    return 1
}

cmd_check() {
    say "== RTC check on ${BOARD} ($(date -u +%FT%TZ)) =="
    OUT=$(rsh '
r=/sys/class/rtc/rtc0
echo "dev=$([ -e /dev/rtc0 ] && echo yes || echo no)"
echo "name=$(cat $r/name 2>/dev/null)"
echo "hctosys=$(cat $r/hctosys 2>/dev/null)"
echo "tick1=$(cat $r/since_epoch 2>/dev/null)"; sleep 3; echo "tick2=$(cat $r/since_epoch 2>/dev/null)"
echo "sys=$(date -u +%s)"; echo "rtc=$(cat $r/since_epoch 2>/dev/null)"
echo "ntp=$(timedatectl show -p NTPSynchronized --value 2>/dev/null)"
cfg=$(zcat /proc/config.gz 2>/dev/null)
echo "cfg_hctosys=$(printf "%s\n" "$cfg" | grep -c "^CONFIG_RTC_HCTOSYS=y")"
echo "cfg_systohc=$(printf "%s\n" "$cfg" | grep -c "^CONFIG_RTC_SYSTOHC=y")"
echo "unit_result=$(systemctl show -p Result --value aico-rtc 2>/dev/null)"
echo "unit_exit=$(systemctl show -p ExecMainStatus --value aico-rtc 2>/dev/null)"
echo "unit_ran=$(systemctl show -p ExecMainStartTimestampMonotonic --value aico-rtc 2>/dev/null)"
echo "script=$([ -x /usr/lib/aico/aico-rtc ] && echo yes || echo no)"
i0=$(awk "/RTC alarm/{print \$2}" /proc/interrupts)
echo 0 > $r/wakealarm 2>/dev/null; echo +3 > $r/wakealarm 2>/dev/null
echo "alarm_set=$(cat $r/wakealarm 2>/dev/null)"
sleep 5
echo "irq_before=$i0"; echo "irq_after=$(awk "/RTC alarm/{print \$2}" /proc/interrupts)"
echo "alarm_left=$(cat $r/wakealarm 2>/dev/null)"
true')
    if [ -z "$OUT" ]; then fail "no answer from ${AGENT}/api/tools/shell (USB connected? Agent running?)"; return; fi

    [ "$(val dev)" = yes ] && pass "/dev/rtc0 present ($(val name))" || fail "/dev/rtc0 missing"
    [ "$(val hctosys)" = 1 ] && pass "rtc0 sets the system clock at boot (hctosys)" || warn "rtc0 hctosys=$(val hctosys)"
    local t1 t2; t1=$(val tick1); t2=$(val tick2)
    if [ -n "$t1" ] && [ -n "$t2" ] && [ $(( t2 - t1 )) -ge 2 ] && [ $(( t2 - t1 )) -le 4 ]; then
        pass "RTC is ticking (+$(( t2 - t1 ))s over 3s)"
    else
        fail "RTC not ticking correctly (${t1:-?} -> ${t2:-?} over 3s)"
    fi
    local d; d=$(( $(val rtc) - $(val sys) ))
    if [ "$(absd "$d")" -le 2 ]; then pass "RTC matches system time (diff ${d}s)"; else warn "RTC and system time differ by ${d}s"; fi
    # The probe above ran for about 8 s (tick + alarm waits), so compare the
    # host with a fresh board sample from one short request instead.
    sample_clock
    local hd slack bsys; bsys=$(sval sys)
    slack=$(( (T1 - T0 + 1999) / 2000 ))
    if [ -z "$bsys" ]; then
        fail "could not read the board system clock for the host comparison"
    else
        hd=$(( bsys - HOST_MID ))
        if [ "$(val ntp)" = yes ]; then
            if [ "$(absd "$hd")" -le $(( TOL + slack )) ]; then pass "system clock NTP-synchronized and matches host (diff ${hd}s)"
            else fail "system clock NTP-synchronized but differs from host by ${hd}s"; fi
        else
            warn "system clock not NTP-synchronized yet (no network?); host diff ${hd}s"
        fi
    fi
    [ "$(val cfg_hctosys)" = 1 ] && pass "kernel CONFIG_RTC_HCTOSYS=y" || warn "kernel CONFIG_RTC_HCTOSYS not set"
    [ "$(val cfg_systohc)" = 1 ] && pass "kernel CONFIG_RTC_SYSTOHC=y (NTP time is written back to the RTC)" || warn "kernel CONFIG_RTC_SYSTOHC not set"
    # Result=success alone is also reported for a unit that never started, so
    # also require a start timestamp from this boot (0 or empty = not run).
    local ran; ran=$(val unit_ran)
    if [ "$(val script)" = yes ] && [ "$(val unit_result)" = success ] && [ -n "$ran" ] && [ "$ran" != 0 ] && [ "$(val unit_exit)" = 0 ]; then
        pass "aico-rtc.service ran at boot (result=success, exit 0)"
    else
        fail "aico-rtc.service script=$(val script) result=$(val unit_result) exit=$(val unit_exit) started_monotonic=${ran:-none}"
    fi
    if [ -n "$(val irq_before)" ] && [ "$(val irq_after)" -gt "$(val irq_before)" ] 2>/dev/null && [ -z "$(val alarm_left)" ]; then
        pass "RTC alarm interrupt fires (count $(val irq_before) -> $(val irq_after))"
    else
        fail "RTC alarm did not fire (irq $(val irq_before) -> $(val irq_after), left=[$(val alarm_left)])"
    fi

    # aico-rtc decision logic, exercised with fake hwclock/date (the real RTC is not touched).
    OUT=$(rsh '
T=$(mktemp -d); mkdir "$T/bin"
cat > "$T/bin/hwclock" <<"EOF"
#!/bin/sh
case "$*" in *--show*) echo "$FAKE_RTC";; *--systohc*) echo SYSTOHC >> "$LOG";; esac
EOF
cat > "$T/bin/date" <<"EOF"
#!/bin/sh
for a in "$@"; do case "$a" in --set=*) echo "SET" >> "$LOG"; exit 0;; esac; done
if [ "$1" = "-u" ] && [ "$2" = "+%s" ]; then echo "$FAKE_NOW"; exit 0; fi
exec /usr/bin/date "$@"
EOF
chmod +x "$T/bin/hwclock" "$T/bin/date"
sim() { : > "$T/log"; LOG="$T/log" FAKE_RTC="$2" FAKE_NOW="$3" PATH="$T/bin:/usr/bin:/bin" sh /usr/lib/aico/aico-rtc >/dev/null 2>&1; echo "$1=$(tr "\n" " " < "$T/log" | sed "s/ *$//")"; }
now=$(date -u +%s)
sim reset_offline "2000-01-01 00:00:05+00:00" 946684805
sim reset_synced  "2000-01-01 00:00:05+00:00" "$now"
sim valid         "$(date -u "+%Y-%m-%d %H:%M:%S+00:00")" "$now"
sim unreadable    "" "$now"
rm -rf "$T"; true')
    [ "$(val reset_offline)" = "SET SYSTOHC" ] && pass "aico-rtc: reset RTC + offline -> sets baseline date and writes RTC" \
        || fail "aico-rtc: reset RTC + offline did [$(val reset_offline)], want [SET SYSTOHC]"
    [ "$(val reset_synced)" = "SYSTOHC" ] && pass "aico-rtc: reset RTC + synced clock -> keeps system time, writes RTC" \
        || fail "aico-rtc: reset RTC + synced clock did [$(val reset_synced)], want [SYSTOHC]"
    [ -z "$(val valid)" ] && pass "aico-rtc: valid RTC -> no change" || fail "aico-rtc: valid RTC did [$(val valid)]"
    [ -z "$(val unreadable)" ] && pass "aico-rtc: unreadable RTC -> no change" || fail "aico-rtc: unreadable RTC did [$(val unreadable)]"
}

# Wait until the Agent answers with a boot ID other than $1. An Agent that
# stays up, or only restarts, keeps the old boot ID and never satisfies this.
# A successful boot ID probe also confirms Agent reachability, so poll it directly.
# Returns 0 on a new boot, 1 after the six-minute wait budget expires.
wait_new_boot() {
    local old=$1 deadline=$(( SECONDS + 360 )) remaining id
    while (( (remaining = deadline - SECONDS) > 0 )); do
        id=$(boot_id "$(( remaining < 15 ? remaining : 15 ))")
        if (( SECONDS < deadline )) && [ -n "$id" ] && [ "$id" != "$old" ]; then
            NEW_BOOT_ID=$id
            return 0
        fi
        remaining=$(( deadline - SECONDS ))
        (( remaining > 0 )) || break
        sleep "$(( remaining < 3 ? remaining : 3 ))"
    done
    return 1
}

cmd_reboot() {
    say "== warm reboot test on ${BOARD} ($(date -u +%FT%TZ)) =="
    local before ack
    before=$(boot_id)
    if [ -z "$before" ]; then fail "cannot read the board boot ID before rebooting"; return 1; fi
    if ! ack=$(rsh '(sleep 2; systemctl reboot) >/dev/null 2>&1 & echo reboot-scheduled' 15) || [ "$ack" != reboot-scheduled ]; then
        fail "the board did not accept the reboot request (${ack:-no answer})"
        return 1
    fi
    say "  info  rebooting (boot ID ${before}), waiting for a new boot..."
    if ! wait_new_boot "$before"; then
        fail "the board did not come back with a new boot ID within 6 minutes"
        return 1
    fi
    say "  info  new boot ID ${NEW_BOOT_ID}"
    sleep 5
    check_boot_rtc "warm reboot"
}

cmd_poweroff_pre() {
    say "== power-off test, step 1: baseline on ${BOARD} ($(date -u +%FT%TZ)) =="
    OUT=$(rsh 'echo "rtc=$(cat /sys/class/rtc/rtc0/since_epoch 2>/dev/null)"; echo "sys=$(date -u +%s)"; echo "ntp=$(timedatectl show -p NTPSynchronized --value 2>/dev/null)"; true')
    [ -z "$(val rtc)" ] && { fail "cannot read the RTC"; return 1; }
    local batt; batt=$(bcurl -m 8 "${AGENT}/api/mcu/battery" | jq -r '.battery | "\(.percent)% \(.charge_state)"' 2>/dev/null)
    { echo "host=$(date -u +%s)"; echo "rtc=$(val rtc)"; echo "sys=$(val sys)"; echo "battery=${batt}"; } > "$STATE"
    say "  info  RTC $(fmt "$(val rtc)"), system $(fmt "$(val sys)"), NTP=$(val ntp), battery ${batt:-unknown}"
    say "  info  baseline saved to ${STATE}"
    say ""
    say "Now: shut the board down (power button), keep it off for at least 3 minutes,"
    say "power it on (ideally with Wi-Fi unavailable), reconnect USB, then run:"
    say "  $0 poweroff-post"
}

cmd_poweroff_post() {
    say "== power-off test, step 2: after power-on on ${BOARD} ($(date -u +%FT%TZ)) =="
    if [ -f "$STATE" ]; then
        local host_off; host_off=$(sed -n 's/^host=//p' "$STATE")
        say "  info  baseline taken $(fmt "$host_off") ($(sed -n 's/^battery=//p' "$STATE"))"
    else
        warn "no baseline in ${STATE}; run poweroff-pre first next time"
    fi
    agent_up || { fail "Agent not reachable at ${AGENT}"; return; }
    check_boot_rtc "power-off"
    local b; b=$(val baseline)
    if [ -n "$b" ] && [ -n "$(val boot_epoch)" ]; then
        local be; be=$(date -u -j -f %F "$b" +%s 2>/dev/null || date -u -d "$b" +%s 2>/dev/null)
        if [ -n "$be" ] && [ "$(val boot_epoch)" -lt "$be" ]; then
            say "  info  RTC came up before the ${b} baseline: aico-rtc should have set ${b} until NTP"
        fi
    fi
    if [ "$(val ntp)" = yes ]; then
        local d; d=$(( $(val rtc) - $(val sys) ))
        if [ "$(absd "$d")" -le 2 ]; then pass "after NTP the RTC was written back (RTC vs system ${d}s)"
        else fail "after NTP the RTC still differs from system time by ${d}s"; fi
    else
        warn "NTP not synchronized yet; rerun later to verify RTC write-back"
    fi
}

summary() {
    say ""
    say "Result: ${PASS} passed, ${FAIL} failed, ${WARN} warnings. Report: ${REPORT}"
    [ "$FAIL" -eq 0 ]
}

case "${1:-}" in
    check|reboot|poweroff-pre|poweroff-post|all)
        ensure_auth || { say "  FAIL  cannot sign in to ${AGENT}"; exit 1; } ;;
    *) sed -n '2,29p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac

case "$1" in
    check)         cmd_check; summary ;;
    reboot)        cmd_reboot; summary ;;
    poweroff-pre)  cmd_poweroff_pre || { summary; exit 1; } ;;
    poweroff-post) cmd_poweroff_post; summary ;;
    all)           cmd_check; cmd_reboot; summary ;;
esac
