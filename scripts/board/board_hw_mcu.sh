#!/bin/sh
# aico-hw-suite: 40 mcu - MCU link, battery/charger, LEDs, timer, wake sources
# board_hw_mcu.sh - companion MCU test for an Aico board: link, battery and
# charger, LEDs, wake-up timer and wake sources (touch key, ASR, power).
#
# Run ON THE BOARD as root next to board_hw_common.sh,
# board_low_level_common.sh and board_hw_tools.py. See --help.
set +e

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/board_hw_common.sh"
hw_start mcu "$@"

: "${MCU_SOCKET:=/run/mcu_service/mcu_service.sock}"
BATTERY_SAMPLES=5
EVENT_SECONDS=30
TEST_CHARGER=0
TEST_POWER_KEY=0

usage() {
    cat <<'USAGE'
Usage:
  sudo sh board_hw_mcu.sh [options]

Checks the companion MCU through mcu_service (no Agent or network needed):
  - mcu_service unit, socket, UART link, protocol version and RV state
  - battery: percent, voltage and charge state, sampled for stability
  - wake-up timer set / read back / cancel (skipped if a timer is active)
  - red and white LEDs: on, blink, back to automatic (confirmed by the operator
    with --interactive; otherwise the read-back state is checked)
  - with --interactive: touch key and voice (ASR) wake events
  - with --interactive --charger: charger unplug/replug events and charge state
  - with --interactive --power-key: a short power-switch press event
    (only if a short press does not power the board off in your firmware)

Options:
  --interactive         Run the steps that need a person at the board
  --charger             With --interactive, test charger removal and reconnection
  --power-key           With --interactive, test a short power-switch press
  --battery-samples N   Battery readings, one per second, default 5
  --event-seconds N     Time allowed for each physical action, default 30
  --socket PATH         mcu_service socket, default /run/mcu_service/mcu_service.sock
  --config FILE         Settings file (see board_hw.conf.example)
  -h, --help            Show this help
USAGE
}

hw_load_config

while [ "$#" -gt 0 ]; do
    case "$1" in
        --interactive) INTERACTIVE=1; shift ;;
        --charger) TEST_CHARGER=1; shift ;;
        --power-key) TEST_POWER_KEY=1; shift ;;
        --battery-samples) [ "$#" -ge 2 ] || die "$1 needs a value"; BATTERY_SAMPLES="$2"; shift 2 ;;
        --event-seconds) [ "$#" -ge 2 ] || die "$1 needs a value"; EVENT_SECONDS="$2"; shift 2 ;;
        --socket) [ "$#" -ge 2 ] || die "$1 needs a value"; MCU_SOCKET="$2"; shift 2 ;;
        --artifact-dir) [ "$#" -ge 2 ] || die "$1 needs a value"; AICO_LOW_LEVEL_ARTIFACT_DIR="$2"; shift 2 ;;
        --run-id) [ "$#" -ge 2 ] || die "$1 needs a value"; RUN_ID="$2"; shift 2 ;;
        --config) [ "$#" -ge 2 ] || die "$1 needs a value"; shift 2 ;;  # loaded by hw_start
        -h|--help) usage; exit 0 ;;
        *) die "unknown option: $1" ;;
    esac
done
require_uint "--battery-samples" "$BATTERY_SAMPLES"
require_uint "--event-seconds" "$EVENT_SECONDS"

mcu() {
    python3 "$HW_TOOLS" mcu "$@" --socket "$MCU_SOCKET"
}

# json_get JSON PATH -> value at a dotted path, or empty
json_get() {
    printf '%s' "$1" | python3 -c 'import json,sys
d=json.load(sys.stdin)
for k in sys.argv[1].split("."):
    d=d.get(k) if isinstance(d,dict) else None
print("" if d is None else (str(d).lower() if isinstance(d,bool) else d))' "$2" 2>/dev/null
}

RESTORE_LEDS=""
TIMER_ARMED=0
restore_leds() {
    for led in $RESTORE_LEDS; do
        mcu set_led --led "$led" --mode auto >/dev/null 2>&1
    done
    RESTORE_LEDS=""
}
# The test arms a wake-up timer only when none was active, so any timer left
# after its set request (even one with an unknown outcome) is the test's own.
cancel_test_timer() {
    [ "$TIMER_ARMED" = "1" ] || return 0
    if [ "$(json_get "$(mcu cancel_wakeup_timer)" status)" = "OK" ]; then
        TIMER_ARMED=0
        return 0
    fi
    return 1
}
cleanup() {
    restore_leds
    cancel_test_timer >/dev/null 2>&1
}
trap 'cleanup' EXIT
trap 'cleanup; exit 130' INT TERM

section "mcu_service"
warn_if_not_root
require_python || hw_finish
service_check "MCU service" "aico-mcu-event.service aico-mcu.service aico_mcu.service mcu_service.service mcu-service.service" ""
check_socket "MCU service" "$MCU_SOCKET" || hw_finish

section "MCU link"
state="$(mcu get_state)"
if [ "$(json_get "$state" status)" = "OK" ]; then
    pass "get_state answered"
    printf '%s\n' "$state" | indent
    [ "$(json_get "$state" state.uart_connected)" = "true" ] && pass "UART link to the MCU is connected" || fail "UART link to the MCU is not connected"
    rv="$(json_get "$state" state.rv_state)"
    case "$rv" in
        ready|Ready|READY|2) pass "RV state reported to the MCU: $rv" ;;
        *) warn "RV state reported to the MCU: '$rv'" ;;
    esac
    proto="$(json_get "$state" state.protocol_version)"
    [ -n "$proto" ] && pass "MCU protocol version $proto" || warn "MCU protocol version unknown"
    last_rx="$(json_get "$state" state.last_rx_at)"
    [ -n "$last_rx" ] && pass "last frame from the MCU at $last_rx" || warn "no frame received from the MCU yet"
    invalid="$(json_get "$state" state.invalid_events)"
    [ "${invalid:-0}" = "0" ] && pass "no invalid MCU events" || warn "invalid MCU events: $invalid"
    LED_SUPPORT="$(json_get "$state" state.led_support)"
else
    fail "get_state failed: $state"
    hw_finish
fi

section "battery and charger"
i=0
mv_min=""
mv_max=""
charge_states=""
while [ "$i" -lt "$BATTERY_SAMPLES" ]; do
    battery="$(mcu get_battery)"
    if [ "$(json_get "$battery" status)" != "OK" ]; then
        fail "get_battery failed: $battery"
        break
    fi
    percent="$(json_get "$battery" battery.percent)"
    mv="$(json_get "$battery" battery.millivolts)"
    charge="$(json_get "$battery" battery.charge_state)"
    printf '    sample %s: %s%% %smV %s\n' "$((i + 1))" "${percent:-?}" "${mv:-?}" "$charge"
    if [ -n "$mv" ]; then
        if [ -z "$mv_min" ] || [ "$mv" -lt "$mv_min" ]; then mv_min="$mv"; fi
        if [ -z "$mv_max" ] || [ "$mv" -gt "$mv_max" ]; then mv_max="$mv"; fi
    fi
    charge_states="$charge_states $charge"
    i=$((i + 1))
    [ "$i" -lt "$BATTERY_SAMPLES" ] && sleep 1
done
if [ -n "${battery:-}" ] && [ "$(json_get "$battery" status)" = "OK" ]; then
    if [ -n "$percent" ]; then
        [ "$percent" -ge 0 ] && [ "$percent" -le 100 ] && pass "battery level $percent%" || fail "battery level out of range: $percent"
    else
        warn "battery level not reported (raw $(json_get "$battery" battery.raw_percent); gauge not ready or battery absent)"
    fi
    if [ -n "$mv_min" ]; then
        # Every sample must be in range, not only the last one.
        if [ "$mv_min" -ge 3000 ] && [ "$mv_max" -le 4500 ]; then
            pass "battery voltage ${mv_min}..${mv_max} mV within 3000..4500 mV"
        else
            fail "battery voltage ${mv_min}..${mv_max} mV outside 3000..4500 mV"
        fi
        [ $((mv_max - mv_min)) -le 150 ] && pass "voltage stable over $BATTERY_SAMPLES samples (${mv_min}..${mv_max} mV)" \
            || warn "voltage moved $((mv_max - mv_min)) mV over $BATTERY_SAMPLES samples"
    else
        warn "battery voltage not reported"
    fi
    case "$charge" in
        not_charging|charging|full) pass "charge state: $charge" ;;
        absent_or_unknown) warn "charge state absent_or_unknown (no battery or charger not reporting)" ;;
        *) fail "unexpected charge state '$charge'" ;;
    esac
fi

section "wake-up timer"
timer="$(mcu get_wakeup_timer)"
if [ "$(json_get "$timer" status)" != "OK" ]; then
    fail "get_wakeup_timer failed: $timer"
elif [ "$(json_get "$timer" timer.active)" = "true" ]; then
    skip "a wake-up timer is already active ($(json_get "$timer" timer.remaining_s)s left); not touching it"
else
    TIMER_ARMED=1
    set_reply="$(mcu set_wakeup_timer --delay-s 3600)"
    if [ "$(json_get "$set_reply" status)" = "OK" ]; then
        readback="$(mcu get_wakeup_timer)"
        remaining="$(json_get "$readback" timer.remaining_s)"
        if [ "$(json_get "$readback" timer.active)" = "true" ] && [ "${remaining:-0}" -gt 3500 ] && [ "${remaining:-0}" -le 3600 ]; then
            pass "wake-up timer set and read back (${remaining}s left)"
        else
            fail "wake-up timer read back as $readback"
        fi
        cancel_test_timer
        after="$(mcu get_wakeup_timer)"
        if [ "$TIMER_ARMED" = "0" ] && [ "$(json_get "$after" timer.active)" = "false" ]; then
            pass "wake-up timer cancelled"
        else
            fail "wake-up timer cancel failed: $after"
        fi
    else
        fail "set_wakeup_timer failed: $set_reply"
        # A lost acknowledgement can leave the timer armed: reconcile it. Only
        # an OK reply with timer.active=false proves nothing is armed;
        # otherwise the flag stays so the exit cleanup still cancels it.
        reconcile="$(mcu get_wakeup_timer)"
        if [ "$(json_get "$reconcile" status)" = "OK" ] && [ "$(json_get "$reconcile" timer.active)" = "false" ]; then
            TIMER_ARMED=0
        elif cancel_test_timer; then
            warn "set_wakeup_timer reported failure but may have armed the timer; cancelled it"
        else
            fail "a test wake-up timer may still be armed; cancel it with the MCU service"
        fi
    fi
fi

section "LEDs"
if [ -n "${LED_SUPPORT:-}" ] && [ "$LED_SUPPORT" != "supported" ] && [ "$LED_SUPPORT" != "true" ]; then
    printf '    MCU LED support: %s\n' "$LED_SUPPORT"
fi
for led in red white; do
    original="$(mcu get_led --led "$led")"
    if [ "$(json_get "$original" status)" != "OK" ]; then
        status="$(json_get "$original" status)"
        if [ "$status" = "UNSUPPORTED" ]; then
            skip "$led LED: unsupported by this MCU firmware"
        else
            fail "$led LED get_led failed: $original"
        fi
        continue
    fi
    RESTORE_LEDS="$RESTORE_LEDS $led"
    # The lease must outlast the operator's answer; it expires to auto mode.
    lease=10
    [ "$INTERACTIVE" = "1" ] && lease=600
    for mode in on blink off; do
        if [ "$mode" = "blink" ]; then
            reply="$(mcu set_led --led "$led" --mode blink --on-ms 200 --off-ms 200 --lease-s "$lease")"
        else
            reply="$(mcu set_led --led "$led" --mode "$mode" --lease-s "$lease")"
        fi
        readback="$(mcu get_led --led "$led")"
        requested="$(json_get "$readback" led.requested_mode)"
        effective="$(json_get "$readback" led.effective_mode)"
        reason="$(json_get "$readback" led.override_reason)"
        if [ "$(json_get "$reply" status)" != "OK" ] || [ "$requested" != "$mode" ]; then
            fail "$led LED $mode: reply $reply, requested '$requested'"
            continue
        fi
        case "$reason" in
            ''|application|auto) ;;
            *)
                # Startup, low-battery and charging policy take priority over
                # application control; the request is valid but not visible.
                skip "$led LED $mode: accepted, output held by the $reason policy (physical check deferred)"
                continue ;;
        esac
        if [ "$effective" != "$mode" ]; then
            fail "$led LED $mode: requested but effective mode is '$effective'"
            continue
        fi
        pass "$led LED $mode (effective mode read back)"
        if [ "$INTERACTIVE" = "1" ]; then
            case "$mode" in
                on) question="Is the $led LED lit steadily?" ;;
                blink) question="Is the $led LED blinking about 2.5 times per second?" ;;
                off) question="Is the $led LED off?" ;;
            esac
            ask_yes_no "$question"
            answer=$?
            # An answer given after the lease expired describes auto mode.
            still="$(json_get "$(mcu get_led --led "$led")" led.effective_mode)"
            if [ "$answer" -eq 2 ]; then
                skip "$led LED $mode (no operator answer)"
            elif [ "$still" != "$mode" ]; then
                skip "$led LED $mode: the LED left $mode before the answer (now '$still'); repeat the check"
            elif [ "$answer" -eq 0 ]; then
                pass "$led LED $mode (confirmed by operator)"
            else
                fail "$led LED $mode (operator reported a problem)"
            fi
        else
            sleep 1
        fi
    done
    mcu set_led --led "$led" --mode auto >/dev/null 2>&1 && pass "$led LED returned to automatic mode"
done
restore_leds

section "wake sources"
if interactive_or_skip "touch key wake event"; then
    hw_tool mcu-events --socket "$MCU_SOCKET" --expect touch_key --seconds "$EVENT_SECONDS" \
        --prompt "Touch the capacitive key once now (within $EVENT_SECONDS s)."
fi
if interactive_or_skip "voice (ASR) wake event"; then
    hw_tool mcu-events --socket "$MCU_SOCKET" --expect asr --seconds "$EVENT_SECONDS" \
        --prompt "Say the wake word toward the board now (within $EVENT_SECONDS s)."
fi
if [ "$TEST_CHARGER" = "1" ] && interactive_or_skip "charger events"; then
    hw_tool mcu-events --socket "$MCU_SOCKET" --expect external_power --seconds "$EVENT_SECONDS" \
        --prompt "Unplug the charger / external power now (the board stays on battery)."
    unplugged="$(json_get "$(mcu get_battery)" battery.charge_state)"
    [ "$unplugged" = "not_charging" ] && pass "charge state after unplug: not_charging" || warn "charge state after unplug: '$unplugged'"
    hw_tool mcu-events --socket "$MCU_SOCKET" --expect external_power --seconds "$EVENT_SECONDS" \
        --prompt "Plug the charger back in now."
    sleep 3
    plugged="$(json_get "$(mcu get_battery)" battery.charge_state)"
    case "$plugged" in
        charging|full) pass "charge state after replug: $plugged" ;;
        *) fail "charge state after replug: '$plugged' (charger not detected)" ;;
    esac
fi
if [ "$TEST_POWER_KEY" = "1" ] && interactive_or_skip "power switch event"; then
    hw_tool mcu-events --socket "$MCU_SOCKET" --expect power_switch --seconds "$EVENT_SECONDS" \
        --prompt "Press the power switch briefly (do NOT hold it)."
fi

restore_leds
hw_finish
