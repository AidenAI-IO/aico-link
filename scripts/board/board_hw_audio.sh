#!/bin/sh
# aico-hw-suite: 20 audio - microphone quality, speaker loopback, volume curve
# board_hw_audio.sh - microphone and speaker quality test for an Aico board.
#
# Run ON THE BOARD as root, with board_hw_common.sh, board_low_level_common.sh
# and board_hw_tools.py in the same directory. See --help.
set +e

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/board_hw_common.sh"
hw_start audio "$@"

: "${AUDIO_SOCKET:=/run/audio_service/audio_service.sock}"
CAPTURE_DEVICE="default"
PLAYBACK_DEVICE="default"
CHANNELS="auto"
RATE=16000
NOISE_SECONDS=3
VOICE_SECONDS=4
STOP_SERVICE=0
VOLUME_STEPS="30 60 90"
MAX_NOISE_DBFS=-45
MIN_SNR_DB=15

usage() {
    cat <<'USAGE'
Usage:
  sudo sh board_hw_audio.sh [options]

Measures the board's audio hardware:
  - ALSA inventory and the DAC/ADC mixer controls
  - audio_service health and the volume set/get round trip
  - microphone noise floor, DC offset, clipping and dead or unbalanced channels
  - speaker-to-microphone loopback straight through ALSA: level, SNR and THD
    per frequency (300 Hz .. 6 kHz), response spread and round-trip latency
  - the same loopback through audio_service (the product playback path,
    including the speaker amplifier enable) and the volume curve
  - the audio_service recording path (VQE/DTLN processing)
  - with --interactive: a spoken-voice capture played back for listening

Options:
  --interactive             Run the steps that need a person at the board
  --capture-device DEV      ALSA capture device, default "default"
  --playback-device DEV     ALSA playback device, default "default"
  --channels N|auto         Microphone channels for ALSA capture, default auto (2, else 1)
  --rate HZ                 Sample rate, default 16000
  --noise-seconds N         Quiet-room capture length, default 3
  --volume-steps "A B C"    audio_service volumes for the volume curve, default "30 60 90"
  --max-noise-dbfs DB       Noise floor limit, default -45
  --min-snr-db DB           Loopback SNR limit inside 500-4000 Hz, default 15
  --stop-audio-service      Stop audio_service during the ALSA tests (restarted afterwards)
  --artifact-dir DIR        Captures are kept here, default /tmp
  --run-id ID               Artifact run id, default UTC timestamp
  --config FILE             Settings file (see board_hw.conf.example)
  -h, --help                Show this help

The loopback needs the speaker and microphone on the same board in a quiet
room. Limits are starting points to calibrate against a known-good board.
USAGE
}

hw_load_config

while [ "$#" -gt 0 ]; do
    case "$1" in
        --interactive) INTERACTIVE=1; shift ;;
        --capture-device) [ "$#" -ge 2 ] || die "$1 needs a value"; CAPTURE_DEVICE="$2"; shift 2 ;;
        --playback-device) [ "$#" -ge 2 ] || die "$1 needs a value"; PLAYBACK_DEVICE="$2"; shift 2 ;;
        --channels) [ "$#" -ge 2 ] || die "$1 needs a value"; CHANNELS="$2"; shift 2 ;;
        --rate) [ "$#" -ge 2 ] || die "$1 needs a value"; RATE="$2"; shift 2 ;;
        --noise-seconds) [ "$#" -ge 2 ] || die "$1 needs a value"; NOISE_SECONDS="$2"; shift 2 ;;
        --volume-steps) [ "$#" -ge 2 ] || die "$1 needs a value"; VOLUME_STEPS="$2"; shift 2 ;;
        --max-noise-dbfs) [ "$#" -ge 2 ] || die "$1 needs a value"; MAX_NOISE_DBFS="$2"; shift 2 ;;
        --min-snr-db) [ "$#" -ge 2 ] || die "$1 needs a value"; MIN_SNR_DB="$2"; shift 2 ;;
        --stop-audio-service) STOP_SERVICE=1; shift ;;
        --artifact-dir) [ "$#" -ge 2 ] || die "$1 needs a value"; AICO_LOW_LEVEL_ARTIFACT_DIR="$2"; shift 2 ;;
        --run-id) [ "$#" -ge 2 ] || die "$1 needs a value"; RUN_ID="$2"; shift 2 ;;
        --config) [ "$#" -ge 2 ] || die "$1 needs a value"; shift 2 ;;  # loaded by hw_start
        -h|--help) usage; exit 0 ;;
        *) die "unknown option: $1" ;;
    esac
done
require_uint "--rate" "$RATE"
require_uint "--noise-seconds" "$NOISE_SECONDS"
[ "$CHANNELS" = "auto" ] || require_uint "--channels" "$CHANNELS"

AUDIO_CLI="$(find_bin audio_service_cli)"
AUDIO_UNIT=""
SERVICE_STOPPED=0
ORIGINAL_VOLUME=""

restore() {
    if [ -n "$ORIGINAL_VOLUME" ] && [ -n "$AUDIO_CLI" ] && [ "$SERVICE_STOPPED" = "0" ]; then
        "$AUDIO_CLI" --socket "$AUDIO_SOCKET" set-volume --volume "$ORIGINAL_VOLUME" >/dev/null 2>&1
        ORIGINAL_VOLUME=""
    fi
    if [ "$SERVICE_STOPPED" = "1" ] && [ -n "$AUDIO_UNIT" ]; then
        systemctl start "$AUDIO_UNIT" >/dev/null 2>&1
        SERVICE_STOPPED=0
    fi
}
trap 'restore' EXIT
trap 'restore; exit 130' INT TERM

section "audio tools"
warn_if_not_root
require_python || hw_finish
for tool in arecord aplay amixer; do
    if command -v "$tool" >/dev/null 2>&1; then pass "$tool: $(command -v "$tool")"; else warn "$tool not installed (alsa-utils)"; fi
done
[ -n "$AUDIO_CLI" ] && pass "audio_service_cli: $AUDIO_CLI" || warn "audio_service_cli not found; service-path checks skipped"

section "ALSA inventory"
if command -v aplay >/dev/null 2>&1; then
    aplay -l 2>&1 | indent
    arecord -l 2>&1 | indent
    aplay -l 2>/dev/null | grep -q '^card' && pass "ALSA playback card present" || fail "no ALSA playback card"
    arecord -l 2>/dev/null | grep -q '^card' && pass "ALSA capture card present" || fail "no ALSA capture card"
fi
if command -v amixer >/dev/null 2>&1; then
    for control in 'DAC HPMIX' 'DAC LINEOUT' 'ADC ALC Left' 'ADC ALC Right' 'ADC Digital Left' 'ADC Digital Right' 'ADC MIC Left Gain' 'ADC MIC Right Gain'; do
        value="$(amixer sget "$control" 2>/dev/null | grep -E 'Mono:|Front Left:|Limits:' | tr -s ' ' | tr '\n' ';')"
        [ -n "$value" ] && printf '    %-20s %s\n' "$control" "$value"
    done
    amixer sget 'DAC LINEOUT' >/dev/null 2>&1 && pass "DAC output mixer controls present" || warn "DAC LINEOUT control not found (different codec?)"
fi

section "audio_service"
if [ -n "$AUDIO_CLI" ]; then
    health="$("$AUDIO_CLI" --socket "$AUDIO_SOCKET" health 2>&1)"
    if [ $? -eq 0 ]; then
        pass "audio_service health: $health"
    else
        fail "audio_service health failed: $health"
    fi
    ORIGINAL_VOLUME="$("$AUDIO_CLI" --socket "$AUDIO_SOCKET" get-volume 2>/dev/null | tr -d '[:space:]')"
    if [ -n "$ORIGINAL_VOLUME" ]; then
        pass "audio_service volume: $ORIGINAL_VOLUME"
        probe=$(( ORIGINAL_VOLUME == 55 ? 45 : 55 ))
        "$AUDIO_CLI" --socket "$AUDIO_SOCKET" set-volume --volume "$probe" >/dev/null 2>&1
        readback="$("$AUDIO_CLI" --socket "$AUDIO_SOCKET" get-volume 2>/dev/null | tr -d '[:space:]')"
        [ "$readback" = "$probe" ] && pass "volume set/get round trip ($probe)" || fail "volume set $probe read back as '$readback'"
        "$AUDIO_CLI" --socket "$AUDIO_SOCKET" set-volume --volume "$ORIGINAL_VOLUME" >/dev/null 2>&1
    else
        fail "audio_service get-volume failed"
    fi
    case "$health" in
        *recording_active=true*|*playback_active=true*)
            warn "audio_service is recording or playing; ALSA tests may find the device busy (use --stop-audio-service)" ;;
    esac
else
    skip "audio_service checks (CLI missing)"
fi

if [ "$STOP_SERVICE" = "1" ]; then
    for unit in aico-audio.service aico_audio.service audio_service.service; do
        if systemctl cat "$unit" >/dev/null 2>&1; then AUDIO_UNIT="$unit"; break; fi
    done
    if [ -n "$AUDIO_UNIT" ] && systemctl stop "$AUDIO_UNIT" 2>/dev/null; then
        SERVICE_STOPPED=1
        pass "stopped $AUDIO_UNIT for direct ALSA access (restarted at exit)"
    else
        warn "could not stop the audio service; continuing"
    fi
fi

section "microphone channels"
if [ "$CHANNELS" = "auto" ]; then
    CHANNELS=1
    if command -v arecord >/dev/null 2>&1; then
        probe_file="$(hw_artifact_path probe.pcm)"
        if arecord -q -D "$CAPTURE_DEVICE" -t raw -f S16_LE -r "$RATE" -c 2 -d 1 "$probe_file" >/dev/null 2>&1 && [ -s "$probe_file" ]; then
            CHANNELS=2
        fi
        rm -f "$probe_file"
    fi
fi
pass "capturing $CHANNELS channel(s) at $RATE Hz from '$CAPTURE_DEVICE'"

section "microphone noise floor"
if command -v arecord >/dev/null 2>&1; then
    [ "$INTERACTIVE" = "1" ] && prompt_enter "Keep the room quiet for $NOISE_SECONDS seconds (no speech, no music)."
    noise_file="$(hw_artifact_path mic_noise.pcm)"
    if arecord -q -D "$CAPTURE_DEVICE" -t raw -f S16_LE -r "$RATE" -c "$CHANNELS" -d "$NOISE_SECONDS" "$noise_file" 2>"$noise_file.err"; then
        hw_tool audio-stats "$noise_file" --rate "$RATE" --channels "$CHANNELS" --label "quiet room" \
            --expect quiet --max-noise-dbfs "$MAX_NOISE_DBFS"
        print_artifact "noise" "$noise_file"
    else
        fail "arecord on '$CAPTURE_DEVICE' failed: $(cat "$noise_file.err")"
    fi
    rm -f "$noise_file.err"
else
    skip "noise floor (arecord missing)"
fi

section "speaker to microphone (ALSA)"
if command -v arecord >/dev/null 2>&1 && command -v aplay >/dev/null 2>&1; then
    loop_file="$(hw_artifact_path loopback_alsa.pcm)"
    hw_tool audio-loopback --player alsa --playback-device "$PLAYBACK_DEVICE" --capture-device "$CAPTURE_DEVICE" \
        --rate "$RATE" --channels "$CHANNELS" --min-snr-db "$MIN_SNR_DB" --save "$loop_file" \
        --json "$(hw_artifact_path loopback_alsa.json)"
    print_artifact "loopback" "$loop_file"
else
    skip "ALSA loopback (aplay/arecord missing)"
fi

if [ -n "$AUDIO_CLI" ] && [ "$SERVICE_STOPPED" = "0" ]; then
    section "speaker to microphone (audio_service)"
    loop_file="$(hw_artifact_path loopback_service.pcm)"
    hw_tool audio-loopback --player service --audio-cli "$AUDIO_CLI" --audio-socket "$AUDIO_SOCKET" \
        --capture-device "$CAPTURE_DEVICE" --rate "$RATE" --channels "$CHANNELS" --min-snr-db "$MIN_SNR_DB" \
        --save "$loop_file" --json "$(hw_artifact_path loopback_service.json)"

    section "volume curve (audio_service)"
    previous=""
    monotonic=1
    for volume in $VOLUME_STEPS; do
        "$AUDIO_CLI" --socket "$AUDIO_SOCKET" set-volume --volume "$volume" >/dev/null 2>&1
        json="$(hw_artifact_path "volume_$volume.json")"
        python3 "$HW_TOOLS" audio-loopback --player service --audio-cli "$AUDIO_CLI" --audio-socket "$AUDIO_SOCKET" \
            --capture-device "$CAPTURE_DEVICE" --rate "$RATE" --channels "$CHANNELS" --freqs 1000 \
            --json "$json" >/dev/null 2>&1
        level="$(python3 -c 'import json,sys
d=json.load(open(sys.argv[1]))
v=[t["level_dbfs"] for c in d["channels"] for t in c["tones"] if not t.get("missing")]
print(max(v) if v else "")' "$json" 2>/dev/null)"
        if [ -z "$level" ]; then
            fail "volume $volume: 1 kHz not detected"
            monotonic=0
            continue
        fi
        printf '    volume %-3s -> 1 kHz at %s dBFS\n' "$volume" "$level"
        if [ -n "$previous" ] && python3 -c 'import sys; sys.exit(0 if float(sys.argv[1]) > float(sys.argv[2]) + 1 else 1)' "$level" "$previous"; then
            :
        elif [ -n "$previous" ]; then
            monotonic=0
        fi
        previous="$level"
    done
    [ "$monotonic" = "1" ] && pass "speaker level rises with the audio_service volume ($VOLUME_STEPS)" \
        || fail "speaker level does not rise with the audio_service volume ($VOLUME_STEPS)"
    "$AUDIO_CLI" --socket "$AUDIO_SOCKET" set-volume --volume "$ORIGINAL_VOLUME" >/dev/null 2>&1

    section "audio_service recording path"
    service_file="$(hw_artifact_path service_record.pcm)"
    if "$AUDIO_CLI" --socket "$AUDIO_SOCKET" record-stream --seconds 2 >"$service_file" 2>"$service_file.err" && [ -s "$service_file" ]; then
        hw_tool audio-stats "$service_file" --rate 16000 --channels 1 --label "audio_service record" --expect any
        print_artifact "service" "$service_file"
    else
        fail "audio_service record-stream failed: $(cat "$service_file.err" 2>/dev/null)"
    fi
    rm -f "$service_file.err"
else
    skip "audio_service loopback, volume curve and recording path (service stopped or CLI missing)"
fi

section "voice capture"
if interactive_or_skip "spoken-voice capture"; then
    voice_file="$(hw_artifact_path voice.pcm)"
    prompt_enter "After Enter, speak normally toward the board for $VOICE_SECONDS seconds from about 1 m away."
    if arecord -q -D "$CAPTURE_DEVICE" -t raw -f S16_LE -r "$RATE" -c "$CHANNELS" -d "$VOICE_SECONDS" "$voice_file" 2>/dev/null; then
        hw_tool audio-stats "$voice_file" --rate "$RATE" --channels "$CHANNELS" --label "voice" --expect voice
        printf '    playing the capture back...\n'
        aplay -q -D "$PLAYBACK_DEVICE" -t raw -f S16_LE -r "$RATE" -c "$CHANNELS" "$voice_file" >/dev/null 2>&1
        confirm_check "voice playback intelligible" "Was your voice clear, without crackle, dropouts or hum?"
        print_artifact "voice" "$voice_file"
    else
        fail "voice capture failed"
    fi
fi

restore
hw_finish
