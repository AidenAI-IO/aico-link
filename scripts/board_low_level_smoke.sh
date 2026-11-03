#!/bin/sh
set +e

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/board_low_level_common.sh"

: "${SEND_HID:=0}"
: "${HID_KEY:=ESCAPE}"
: "${HID_TEXT:=}"
: "${HID_CLICK_X:=16384}"
: "${HID_CLICK_Y:=16384}"
: "${HID_CLICK_BUTTON:=left}"
: "${HID_DURATION_MS:=80}"
: "${AUDIO_SECONDS:=2}"
: "${PLAY_AUDIO:=0}"

RUN_SYSTEM=1
RUN_HID=1
RUN_HDMI=1
RUN_AUDIO=1
RUN_LOGS=1

usage() {
    cat <<'USAGE'
Usage:
  scripts/board_low_level_smoke.sh [options]

Runs all board-local low-level smoke checks by delegating to:
  - board_low_level_system.sh
  - board_low_level_hid.sh
  - board_low_level_hdmi.sh
  - board_low_level_audio.sh
  - board_low_level_logs.sh

Options:
  --send-hid             Actually send HID keyboard/click events
  --hid-key KEY          Key for example_usb_hid keyboard tap, default ESCAPE
  --hid-text TEXT        Optional ASCII text for example_usb_hid keyboard text
  --click X,Y            Raw HID click coordinates 0..32767, default 16384,16384
  --click-button BUTTON  left|right|middle, default left
  --hid-duration-ms N    HID press/click hold duration, default 80
  --audio-seconds N      Record duration for audio_service_cli record-stream, default 2
  --play-audio           Pipe recorded PCM back through audio_service_cli play-stream
  --artifact-dir DIR     Artifact directory, default /tmp
  --run-id ID            Artifact run id, default UTC timestamp
  --only NAME            Run only system|hid|hdmi|audio|logs
  --skip NAME            Skip system|hid|hdmi|audio|logs
  -h, --help             Show this help

This script does not use Agent, Config Web, SSH, or HTTP APIs and must run on the board.
USAGE
}

disable_all() {
    RUN_SYSTEM=0
    RUN_HID=0
    RUN_HDMI=0
    RUN_AUDIO=0
    RUN_LOGS=0
}

enable_only() {
    disable_all
    case "$1" in
        system) RUN_SYSTEM=1 ;;
        hid) RUN_HID=1 ;;
        hdmi) RUN_HDMI=1 ;;
        audio) RUN_AUDIO=1 ;;
        logs) RUN_LOGS=1 ;;
        *) die "--only must be system, hid, hdmi, audio, or logs" ;;
    esac
}

skip_one() {
    case "$1" in
        system) RUN_SYSTEM=0 ;;
        hid) RUN_HID=0 ;;
        hdmi) RUN_HDMI=0 ;;
        audio) RUN_AUDIO=0 ;;
        logs) RUN_LOGS=0 ;;
        *) die "--skip must be system, hid, hdmi, audio, or logs" ;;
    esac
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --send-hid)
            SEND_HID=1
            shift
            ;;
        --hid-key)
            [ "$#" -ge 2 ] || die "--hid-key requires an argument"
            HID_KEY="$2"
            shift 2
            ;;
        --hid-text)
            [ "$#" -ge 2 ] || die "--hid-text requires an argument"
            HID_TEXT="$2"
            shift 2
            ;;
        --click)
            [ "$#" -ge 2 ] || die "--click requires X,Y"
            case "$2" in
                *,*)
                    HID_CLICK_X="${2%%,*}"
                    HID_CLICK_Y="${2#*,}"
                    ;;
                *)
                    die "--click expects X,Y"
                    ;;
            esac
            shift 2
            ;;
        --click-button)
            [ "$#" -ge 2 ] || die "--click-button requires an argument"
            HID_CLICK_BUTTON="$2"
            shift 2
            ;;
        --hid-duration-ms)
            [ "$#" -ge 2 ] || die "--hid-duration-ms requires an argument"
            HID_DURATION_MS="$2"
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
        --only)
            [ "$#" -ge 2 ] || die "--only requires an argument"
            enable_only "$2"
            shift 2
            ;;
        --skip)
            [ "$#" -ge 2 ] || die "--skip requires an argument"
            skip_one "$2"
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

case "$HID_CLICK_BUTTON" in
    left|right|middle) ;;
    *) die "--click-button must be left, right, or middle" ;;
esac
require_uint "--click X" "$HID_CLICK_X"
require_uint "--click Y" "$HID_CLICK_Y"
require_uint "--hid-duration-ms" "$HID_DURATION_MS"
require_uint "--audio-seconds" "$AUDIO_SECONDS"

export RUN_ID AICO_LOW_LEVEL_ARTIFACT_DIR
export SEND_HID HID_KEY HID_TEXT HID_CLICK_X HID_CLICK_Y HID_CLICK_BUTTON HID_DURATION_MS
export AUDIO_SECONDS PLAY_AUDIO

echo "Board-local low-level smoke test"
echo "Agent, Config Web, SSH, and HTTP APIs are intentionally not used."
echo "run_id=$RUN_ID"
echo "artifact_dir=$AICO_LOW_LEVEL_ARTIFACT_DIR"
if [ "$SEND_HID" = "1" ]; then
    echo "HID send is enabled: key=$HID_KEY text=${HID_TEXT:-<none>} click=${HID_CLICK_X},${HID_CLICK_Y},${HID_CLICK_BUTTON}"
else
    echo "HID send is disabled. Add --send-hid to emit keyboard/click reports."
fi

OVERALL_FAILS=0

run_subcheck() {
    aico_subcheck_name="$1"
    aico_subcheck_script="$2"
    section "$aico_subcheck_name"
    "$aico_subcheck_script"
    aico_subcheck_rc=$?
    if [ "$aico_subcheck_rc" -ne 0 ]; then
        OVERALL_FAILS=$((OVERALL_FAILS + 1))
    fi
}

[ "$RUN_SYSTEM" -eq 1 ] && run_subcheck "system" "$SCRIPT_DIR/board_low_level_system.sh"
[ "$RUN_HID" -eq 1 ] && run_subcheck "hid" "$SCRIPT_DIR/board_low_level_hid.sh"
[ "$RUN_HDMI" -eq 1 ] && run_subcheck "hdmi" "$SCRIPT_DIR/board_low_level_hdmi.sh"
[ "$RUN_AUDIO" -eq 1 ] && run_subcheck "audio" "$SCRIPT_DIR/board_low_level_audio.sh"
[ "$RUN_LOGS" -eq 1 ] && run_subcheck "logs" "$SCRIPT_DIR/board_low_level_logs.sh"

section "overall summary"
printf 'failed_sections=%s\n' "$OVERALL_FAILS"
[ "$OVERALL_FAILS" -eq 0 ]
exit $?
