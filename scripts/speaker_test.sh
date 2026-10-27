#!/bin/sh
# speaker_test.sh - Reusable on-board speaker test for the Aico RV1106 board.
#
# Generates test tones with python3 and plays them through audio_service
# (the normal playback path) or, with --aplay, straight to the ALSA device.
# Designed to run ON THE BOARD. Copy it over and run, e.g.:
#   scp scripts/speaker_test.sh aico@192.168.42.1:/tmp/
#   ssh aico@192.168.42.1 'sh /tmp/speaker_test.sh --max-volume'
#
# Exit status is 0 only when playback reports success.
set -eu

SOCKET="${AUDIO_SOCKET:-/run/audio_service/audio_service.sock}"
CLI="${AUDIO_CLI:-/usr/lib/aico/audio_service_cli}"
RATE=16000
PATTERN=all          # all | beeps | sweep | tone
FREQ=1000            # Hz, used by the "tone" pattern
SECONDS=2            # seconds, used by the "tone" pattern
SET_VOLUME=""        # logical audio_service volume 0..100 (empty = leave as-is)
MAX_MIXER=0          # 1 = push ALSA DAC HPMIX/LINEOUT to maximum
USE_APLAY=0          # 1 = bypass audio_service, play straight to ALSA
KEEP=0               # 1 = keep the generated PCM file
OUT=""               # PCM output path (default /tmp/speaker_test.pcm)

usage() {
    cat <<'USAGE'
Usage: speaker_test.sh [options]

Options:
  --pattern NAME     all | beeps | sweep | tone        (default: all)
  --freq HZ          tone frequency for --pattern tone  (default: 1000)
  --seconds N        tone duration for --pattern tone   (default: 2)
  --rate HZ          sample rate                         (default: 16000)
  --set-volume N     set audio_service logical volume 0..100 before playing
  --max-volume       push ALSA DAC HPMIX/LINEOUT to maximum (amixer)
  --aplay            play straight to the ALSA device, bypassing audio_service
  --socket PATH      audio_service socket (default /run/audio_service/audio_service.sock)
  --out PATH         PCM output file (default /tmp/speaker_test.pcm)
  --keep             keep the generated PCM file instead of deleting it
  -h, --help         show this help

Patterns:
  beeps  three short 1.2 kHz beeps
  sweep  2.5 s logarithmic sweep 300 Hz -> 3 kHz
  tone   single --freq sine for --seconds
  all    beeps, a pause, then the sweep
USAGE
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --pattern)    [ "$#" -ge 2 ] || { echo "--pattern needs an argument" >&2; exit 2; }; PATTERN="$2"; shift 2 ;;
        --freq)       [ "$#" -ge 2 ] || { echo "--freq needs an argument" >&2; exit 2; }; FREQ="$2"; shift 2 ;;
        --seconds)    [ "$#" -ge 2 ] || { echo "--seconds needs an argument" >&2; exit 2; }; SECONDS="$2"; shift 2 ;;
        --rate)       [ "$#" -ge 2 ] || { echo "--rate needs an argument" >&2; exit 2; }; RATE="$2"; shift 2 ;;
        --set-volume) [ "$#" -ge 2 ] || { echo "--set-volume needs an argument" >&2; exit 2; }; SET_VOLUME="$2"; shift 2 ;;
        --max-volume) MAX_MIXER=1; shift ;;
        --aplay)      USE_APLAY=1; shift ;;
        --socket)     [ "$#" -ge 2 ] || { echo "--socket needs an argument" >&2; exit 2; }; SOCKET="$2"; shift 2 ;;
        --out)        [ "$#" -ge 2 ] || { echo "--out needs an argument" >&2; exit 2; }; OUT="$2"; shift 2 ;;
        --keep)       KEEP=1; shift ;;
        -h|--help)    usage; exit 0 ;;
        *)            echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
done

OUT="${OUT:-/tmp/speaker_test.pcm}"

command -v python3 >/dev/null 2>&1 || { echo "python3 not found on board" >&2; exit 1; }

echo "Generating PCM: pattern=$PATTERN rate=$RATE -> $OUT"
python3 - "$OUT" "$RATE" "$PATTERN" "$FREQ" "$SECONDS" <<'PY'
import sys, struct, math

out, rate, pattern, freq, secs = sys.argv[1], int(sys.argv[2]), sys.argv[3], float(sys.argv[4]), float(sys.argv[5])
amp = 0.85
s = []

def env(i, n, ramp=0.01):
    r = max(1, int(rate * ramp))
    return min(1.0, i / r, (n - i) / r)

def tone(f, dur, a=amp):
    n = int(rate * dur)
    for i in range(n):
        s.append(int(a * env(i, n) * 32767 * math.sin(2 * math.pi * f * i / rate)))

def silence(dur):
    s.extend([0] * int(rate * dur))

def sweep(f0, f1, dur):
    n = int(rate * dur)
    for i in range(n):
        t = i / rate
        f = f0 * (f1 / f0) ** (t / dur)
        s.append(int(amp * env(i, n) * 32767 * math.sin(2 * math.pi * f * t)))

def beeps():
    for _ in range(3):
        tone(1200, 0.25); silence(0.15)

if pattern == "beeps":
    beeps()
elif pattern == "sweep":
    sweep(300, 3000, 2.5)
elif pattern == "tone":
    tone(freq, secs)
elif pattern == "all":
    beeps(); silence(0.3); sweep(300, 3000, 2.5)
else:
    sys.stderr.write("unknown pattern: %s\n" % pattern); sys.exit(2)

with open(out, "wb") as f:
    for v in s:
        f.write(struct.pack("<h", max(-32768, min(32767, v))))
sys.stderr.write("wrote %d bytes (%.2fs)\n" % (len(s) * 2, len(s) / rate))
PY

if [ "$MAX_MIXER" = "1" ]; then
    echo "Setting ALSA DAC to maximum..."
    amixer sset 'DAC HPMIX' 2 >/dev/null 2>&1 && echo "  DAC HPMIX = 2/2" || echo "  (DAC HPMIX unavailable)"
    amixer sset 'DAC LINEOUT' 30 >/dev/null 2>&1 && echo "  DAC LINEOUT = 30/30" || echo "  (DAC LINEOUT unavailable)"
fi

cleanup() { [ "$KEEP" = "1" ] || rm -f "$OUT"; }
trap cleanup EXIT

rc=0
if [ "$USE_APLAY" = "1" ]; then
    echo "Playing via aplay (direct ALSA)..."
    aplay -q -f S16_LE -r "$RATE" -c 1 "$OUT" || rc=$?
else
    [ -x "$CLI" ] || { echo "audio_service_cli not found: $CLI (try --aplay)" >&2; exit 1; }
    if [ -n "$SET_VOLUME" ]; then
        echo "Setting audio_service volume = $SET_VOLUME"
        "$CLI" --socket "$SOCKET" set-volume --volume "$SET_VOLUME" >/dev/null 2>&1 || true
    fi
    echo "Playing via audio_service (socket $SOCKET)..."
    "$CLI" --socket "$SOCKET" play-stream --rate "$RATE" --ch 1 --bits 16 < "$OUT" || rc=$?
fi

if [ "$rc" = "0" ]; then
    echo "Playback OK (rc=0). Listen for sound from the speaker."
else
    echo "Playback FAILED (rc=$rc)." >&2
fi
exit "$rc"
