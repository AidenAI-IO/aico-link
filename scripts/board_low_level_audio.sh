#!/bin/sh
set +e

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/board_low_level_common.sh"

: "${AUDIO_SOCKET:=/run/audio_service/audio_service.sock}"
: "${AUDIO_SECONDS:=2}"
: "${PLAY_AUDIO:=0}"

usage() {
    cat <<'USAGE'
Usage:
  scripts/board_low_level_audio.sh [options]

Runs board-local audio_service checks:
  - audio socket
  - audio_service_cli health and get-volume
  - short PCM recording artifact
  - optional playback of recorded PCM

Options:
  --socket PATH       audio_service Unix socket, default /run/audio_service/audio_service.sock
  --audio-seconds N   Record duration, default 2
  --play-audio        Pipe recorded PCM back through audio_service_cli play-stream
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
            AUDIO_SOCKET="$2"
            shift 2
            ;;
        --audio-seconds)
            [ "$#" -ge 2 ] || die "--audio-seconds requires an argument"
            AUDIO_SECONDS="$2"
            shift 2
            ;;
        --play-audio)
            PLAY_AUDIO=1
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

require_uint "--audio-seconds" "$AUDIO_SECONDS"
AUDIO_RECORD_OUT="$(artifact_path record.pcm)"

section "audio"
AUDIO_CLI="$(find_bin audio_service_cli)"
[ -n "$AUDIO_CLI" ] && pass "audio_service_cli: $AUDIO_CLI" || fail "audio_service_cli not found"
check_socket "audio" "$AUDIO_SOCKET"

if [ -n "$AUDIO_CLI" ]; then
    run_check "audio_service health" "$AUDIO_CLI" --socket "$AUDIO_SOCKET" health
    run_check "audio_service get-volume" "$AUDIO_CLI" --socket "$AUDIO_SOCKET" get-volume
    run_check "audio_service record ${AUDIO_SECONDS}s" sh -c \
        'exec "$1" --socket "$2" record-stream --seconds "$3" > "$4"' \
        sh "$AUDIO_CLI" "$AUDIO_SOCKET" "$AUDIO_SECONDS" "$AUDIO_RECORD_OUT"
    if [ -s "$AUDIO_RECORD_OUT" ]; then
        pass "audio PCM written: $AUDIO_RECORD_OUT ($(wc -c < "$AUDIO_RECORD_OUT") bytes)"
    else
        fail "audio PCM file is empty or missing: $AUDIO_RECORD_OUT"
    fi
    if [ "$PLAY_AUDIO" = "1" ]; then
        run_check "audio_service playback recorded PCM" sh -c \
            'exec "$1" --socket "$2" play-stream --rate 16000 --ch 1 --bits 16 < "$3"' \
            sh "$AUDIO_CLI" "$AUDIO_SOCKET" "$AUDIO_RECORD_OUT"
    else
        skip "audio playback disabled; pass --play-audio to exercise play-stream"
    fi
else
    fail "audio_service_cli missing; cannot test audio service"
fi

section "artifacts"
print_artifact "audio_pcm" "$AUDIO_RECORD_OUT"

print_summary
exit $?
