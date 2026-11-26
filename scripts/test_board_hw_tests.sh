#!/bin/sh
# Host-side test for the board hardware suites (board_hw_*.sh, board_hw_tools.py).
# Uses fake aplay/arecord, a fake MCU service socket and generated screenshots;
# no board, sound card or network is needed.
set -eu

# The board-side scripts live in scripts/board.
SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")/board" && pwd)"
TOOLS="$SCRIPT_DIR/board_hw_tools.py"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/aico_hw_test.XXXXXX")"
SERVER_PID=""
cleanup() {
    if [ -n "$SERVER_PID" ]; then
        kill "$SERVER_PID" 2>/dev/null || true
        wait "$SERVER_PID" 2>/dev/null || true
    fi
    rm -rf "$WORK"
}
trap cleanup EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

# Evidence bundles of the runs below go to the work directory.
export AICO_LOW_LEVEL_ARTIFACT_DIR="$WORK/evidence"
mkdir -p "$AICO_LOW_LEVEL_ARTIFACT_DIR"
sha_check() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum -c --quiet SHA256SUMS; else shasum -a 256 -c --quiet SHA256SUMS; fi
}

expect_line() {
    printf '%s\n' "$2" | grep -F -- "$1" >/dev/null || {
        printf '%s\n' "$2" >&2
        fail "expected output line: $1"
    }
}

# 1. Syntax, help and local-only policy -------------------------------------
for script in board_hw_common.sh board_hw_audio.sh board_hw_hdmi.sh board_hw_mcu.sh board_hw_peripherals.sh board_hw_test.sh; do
    sh -n "$SCRIPT_DIR/$script"
    if command -v dash >/dev/null 2>&1; then dash -n "$SCRIPT_DIR/$script"; fi
done
for script in board_hw_audio.sh board_hw_hdmi.sh board_hw_mcu.sh board_hw_peripherals.sh board_hw_test.sh; do
    sh "$SCRIPT_DIR/$script" --help >/dev/null || fail "$script --help"
    if grep -E 'AICO_AGENT|AICO_CONFIG_WEB|/api/|ssh |:8080' "$SCRIPT_DIR/$script" | grep -v '^#' | grep -v 'aico@192.168.42.1' >/dev/null; then
        fail "$script depends on the Agent, Config Web or SSH"
    fi
done
entry_help="$(sh "$SCRIPT_DIR/board_hw_test.sh" --help)"
for suite in lowlevel audio hdmi mcu peripherals; do
    expect_line "$suite" "$entry_help"
done

# 2. Signal analysis self-test ---------------------------------------------
python3 "$TOOLS" self-test >/dev/null || fail "board_hw_tools.py self-test"

# 3. Loopback through fake ALSA tools --------------------------------------
mkdir -p "$WORK/bin"
cat >"$WORK/bin/aplay" <<'EOF'
#!/bin/sh
# Fake aplay: remember what was played.
for last; do :; done
cp "$last" "$FAKE_AUDIO_DIR/played.pcm"
EOF
cat >"$WORK/bin/arecord" <<'EOF'
#!/bin/sh
# Fake arecord: after playback, write the played signal delayed by 150 ms,
# attenuated and with noise, as FAKE_MIC_CHANNELS interleaved channels.
out=""; seconds=1; channels=1
while [ "$#" -gt 0 ]; do
    case "$1" in
        -d) seconds="$2"; shift 2 ;;
        -c) channels="$2"; shift 2 ;;
        -D|-t|-f|-r) shift 2 ;;
        -q) shift ;;
        *) out="$1"; shift ;;
    esac
done
i=0
while [ ! -s "$FAKE_AUDIO_DIR/played.pcm" ] && [ "$i" -lt 100 ]; do sleep 0.1; i=$((i + 1)); done
python3 - "$FAKE_AUDIO_DIR/played.pcm" "$out" "$seconds" "$channels" "${FAKE_MIC_MODE:-ok}" <<'PY'
import struct, sys
src, out, seconds, channels, mode = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4]), sys.argv[5]
rate = 16000
try:
    data = open(src, "rb").read()
    played = list(struct.unpack(f"<{len(data)//2}h", data))
except OSError:
    played = []
total = rate * seconds
delay = int(rate * (0.8 + 0.150))  # capture lead 0.8 s + 150 ms round trip
signal = [0] * total
for i, s in enumerate(played):
    if delay + i < total:
        signal[delay + i] = s // 3
frames = []
for i, s in enumerate(signal):
    noise = (i * 7919) % 41 - 20
    for c in range(channels):
        v = 0 if mode == "dead" else s + noise
        if mode == "onedead" and c == 1:
            v = noise // 10
        frames.append(max(-32768, min(32767, v)))
open(out, "wb").write(struct.pack(f"<{len(frames)}h", *frames))
PY
EOF
chmod +x "$WORK/bin/aplay" "$WORK/bin/arecord"
export FAKE_AUDIO_DIR="$WORK"
loopback() {
    ch="$1"
    shift
    rm -f "$WORK/played.pcm"
    PATH="$WORK/bin:$PATH" python3 "$TOOLS" audio-loopback --player alsa --channels "$ch" --capture-lead-s 0.8 \
        --freqs 500,1000,2000,4000 --segment-ms 300 "$@" 2>&1
}
out="$(FAKE_MIC_MODE=ok loopback 2)" || { printf '%s\n' "$out" >&2; fail "healthy loopback should pass"; }
expect_line "[PASS] mic ch0: 1000 Hz level" "$out"
expect_line "[PASS] mic ch1: 4000 Hz level" "$out"
printf '%s\n' "$out" | grep -qE 'round trip ~1[3-7]0 ms' || fail "loopback latency should be ~150 ms (20 ms alignment steps)"
out="$(FAKE_MIC_MODE=dead loopback 1)" && fail "dead microphone should fail"
expect_line "[FAIL] mic: no speaker signal detected" "$out"
out="$(FAKE_MIC_MODE=onedead loopback 2)" && fail "one dead channel should fail"
expect_line "[FAIL] mic ch1: no speaker signal detected" "$out"

# 4. Capture statistics ----------------------------------------------------
python3 "$TOOLS" audio-gen --kind silence --ms 1000 --out "$WORK/zero.pcm"
out="$(python3 "$TOOLS" audio-stats "$WORK/zero.pcm" --expect quiet --label zero)" && fail "all-zero capture should fail"
expect_line "every sample is zero" "$out"
python3 "$TOOLS" audio-gen --kind tone --freq 440 --level-dbfs -60 --ms 1000 --out "$WORK/quiet.pcm"
out="$(python3 "$TOOLS" audio-stats "$WORK/quiet.pcm" --expect quiet --label quiet)" || { printf '%s\n' "$out" >&2; fail "-60 dBFS hum is under the -45 dBFS noise limit"; }

# A live left channel with an unused, zero-filled right channel passes; a dead
# left channel fails.
python3 - "$WORK" <<'PY'
import math, struct, sys
left = [int(300 * math.sin(i / 3)) for i in range(16000)]
open(sys.argv[1] + "/rightzero.pcm", "wb").write(struct.pack("<32000h", *[v for s in left for v in (s, 0)]))
open(sys.argv[1] + "/leftzero.pcm", "wb").write(struct.pack("<32000h", *[v for s in left for v in (0, s)]))
PY
python3 "$TOOLS" audio-stats "$WORK/rightzero.pcm" --channels 2 --expect any >/dev/null || fail "unused zero right channel should not fail"
python3 "$TOOLS" audio-stats "$WORK/leftzero.pcm" --channels 2 --expect any >/dev/null && fail "zero left channel should fail"

# 5. Screenshot analysis ---------------------------------------------------
python3 - "$WORK" <<'PY'
import struct, sys
def bmp(path, fill):
    w, h = 64, 32
    stride = (w * 3 + 3) & ~3
    rows = b"".join(bytes(sum((fill(x, y) for x in range(w)), [])) + b"\0" * (stride - w * 3) for y in range(h))
    head = b"BM" + struct.pack("<IHHI", 54 + len(rows), 0, 0, 54) + struct.pack("<IiiHHIIiiII", 40, w, h, 1, 24, 0, len(rows), 0, 0, 0, 0)
    open(path, "wb").write(head + rows)
bmp(sys.argv[1] + "/black.bmp", lambda x, y: [0, 0, 0])
bmp(sys.argv[1] + "/ui.bmp", lambda x, y: [(x * 4) % 256, (y * 8) % 256, 255 if (x // 8 + y // 8) % 2 else 40])
bmp(sys.argv[1] + "/ui2.bmp", lambda x, y: [255 - (x * 4) % 256, (y * 8) % 256, 40])
PY
python3 "$TOOLS" frame-stats "$WORK/black.bmp" >/dev/null && fail "black screenshot should fail"
head -c 3000 "$WORK/ui.bmp" >"$WORK/truncated.bmp"
out="$(python3 "$TOOLS" frame-stats "$WORK/truncated.bmp" 2>&1)" && fail "truncated screenshot should fail"
expect_line "is truncated" "$out"
out="$(python3 "$TOOLS" frame-stats "$WORK/ui.bmp" --expect-size 64x32)" || { printf '%s\n' "$out" >&2; fail "UI screenshot should pass"; }
out="$(python3 "$TOOLS" frame-stats "$WORK/ui.bmp" --compare "$WORK/ui.bmp" --expect-change)" && fail "identical screenshots should fail the change check"
python3 "$TOOLS" frame-stats "$WORK/ui2.bmp" --compare "$WORK/ui.bmp" --expect-change >/dev/null || fail "changed screenshots should pass"

# 6. MCU suite against a fake mcu_service ----------------------------------
SOCK="$WORK/mcu.sock"
python3 - "$SOCK" <<'PY' &
import json, os, socket, struct, sys
path = sys.argv[1]
timer = {"timer_id": 0, "active": False, "remaining_s": 0}
leds = {"red": "auto", "white": "auto"}
server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
server.bind(path)
server.listen(16)
while True:
    conn, _ = server.accept()
    with conn:
        size, extra = struct.unpack("<IQ", conn.recv(12))
        body = b""
        while len(body) < size:
            body += conn.recv(size - len(body))
        req = json.loads(body)
        op = req["op"]
        # Like mcu_service: writes need a request id and the current generation.
        if op in ("set_wakeup_timer", "cancel_wakeup_timer", "set_led") and (
                not 1 <= len(req.get("request_id", "")) <= 128 or req.get("generation") != "g"):
            resp = {"status": "INVALID_ARGUMENT", "error": "request_id and current generation required"}
        elif op == "get_state":
            resp = {"status": "OK", "state": {"rv_state": "ready", "protocol_version": 1, "uart_connected": True,
                                               "last_rx_at": "2026-11-25T00:00:00Z", "led_support": "supported",
                                               "invalid_events": 0, "generation": "g"}}
        elif op == "get_battery":
            resp = {"status": "OK", "battery": {"percent": 76, "millivolts": 3920, "charge_state": "charging",
                                                 "raw_percent": 76, "raw_millivolts": 3920}}
        elif op == "get_wakeup_timer":
            resp = {"status": "OK", "timer": dict(timer)}
        elif op == "set_wakeup_timer":
            timer.update(active=True, remaining_s=req["delay_s"] - 1)
            resp = {"status": "OK"}
        elif op == "cancel_wakeup_timer":
            timer.update(active=False, remaining_s=0)
            resp = {"status": "OK"}
        elif op == "get_led":
            mode = leds[req["led"]]
            resp = {"status": "OK", "led": {"led": req["led"], "requested_mode": mode, "effective_mode": mode}}
        elif op == "set_led":
            leds[req["led"]] = req["mode"]
            resp = {"status": "OK"}
        else:
            resp = {"status": "UNSUPPORTED", "error": op}
        data = json.dumps(resp).encode()
        conn.sendall(struct.pack("<IQ", len(data), 0) + data)
PY
SERVER_PID=$!
i=0
while [ ! -S "$SOCK" ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
[ -S "$SOCK" ] || fail "fake MCU service did not start"
out="$(sh "$SCRIPT_DIR/board_hw_mcu.sh" --socket "$SOCK" --battery-samples 2 --run-id evidence-mcu 2>&1)" || {
    printf '%s\n' "$out" >&2
    fail "MCU suite should pass against a healthy fake service"
}
for line in "[PASS] UART link to the MCU is connected" "[PASS] battery level 76%" "[PASS] charge state: charging" \
    "[PASS] wake-up timer set and read back" "[PASS] wake-up timer cancelled" "[PASS] red LED blink" \
    "[PASS] white LED returned to automatic mode" "[SKIP] touch key wake event (needs --interactive)"; do
    expect_line "$line" "$out"
done

# 6a. Config file: loaded by the suite, and the command line wins over it.
mcu_out="$out"
printf 'MCU_SOCKET=%s\nBATTERY_SAMPLES=1\n' "$SOCK" >"$WORK/good.conf"
printf 'MCU_SOCKET=%s\n' "$WORK/missing.sock" >"$WORK/bad.conf"
out="$(sh "$SCRIPT_DIR/board_hw_mcu.sh" --config "$WORK/good.conf" --run-id config-file 2>&1)" || {
    printf '%s\n' "$out" >&2
    fail "MCU suite should take its socket from the config file"
}
out="$(sh "$SCRIPT_DIR/board_hw_mcu.sh" --config "$WORK/bad.conf" --socket "$SOCK" --run-id config-cli 2>&1)" || {
    printf '%s\n' "$out" >&2
    fail "--socket on the command line should override the config file"
}
out="$(sh "$SCRIPT_DIR/board_hw_mcu.sh" --config "$WORK/none.conf" 2>&1)" && fail "a missing config file should be rejected"
expect_line "cannot read config file" "$out"

# 6b. Evidence bundle of the first MCU run ---------------------------------------------
RUN_DIR="$AICO_LOW_LEVEL_ARTIFACT_DIR/aico_hw_evidence-mcu"
for file in mcu.log events.jsonl failures.txt run.json SHA256SUMS; do
    [ -s "$RUN_DIR/$file" ] || fail "evidence file missing: $file"
done
[ -s "$RUN_DIR.tar.gz" ] || fail "evidence tarball missing"
expect_line "bundle sha256" "$mcu_out"
grep -q '^# started=.* boot_id=' "$RUN_DIR/mcu.log" || fail "transcript header missing"
grep -q '^\[PASS\] UART link to the MCU is connected' "$RUN_DIR/mcu.log" || fail "transcript is missing check lines"
python3 - "$RUN_DIR" <<'PY' || fail "events.jsonl or run.json content"
import json, sys
run = sys.argv[1]
events = [json.loads(line) for line in open(run + "/events.jsonl")]
checks = [e for e in events if e["result"] in ("PASS", "WARN", "FAIL", "SKIP")]
assert checks and all(e["time"].endswith("Z") and e["suite"] == "mcu" for e in checks), "event fields"
assert any(e["result"] == "START" for e in events) and any(e["result"] == "END" for e in events), "start/end"
assert all(e["section"] for e in checks), "every check names its section"
manifest = json.load(open(run + "/run.json"))
assert manifest["run_id"] == "evidence-mcu" and manifest["exit_code"] == 0, "manifest header"
suite = manifest["suites"]["mcu"]
assert suite["PASS"] == sum(1 for e in checks if e["result"] == "PASS") and suite["FAIL"] == 0, "suite counts"
assert "board_hw_mcu.sh" in manifest["scripts_sha256"] and "mcu.log" in manifest["artifacts"], "hashes"
assert manifest["board"]["hostname"], "board identity"
assert manifest["clock"]["system_utc"].endswith("Z") and not [f for f in manifest["failures"] if f["result"] == "FAIL"], "clock and failures"
PY
(cd "$RUN_DIR" && sha_check) || fail "SHA256SUMS does not verify"

# 6c. A failing run lists each failure with time, section and detail lines.
out="$(sh "$SCRIPT_DIR/board_hw_mcu.sh" --socket "$WORK/missing.sock" --run-id evidence-fail 2>&1)" && fail "missing MCU socket should fail"
FAILURES="$AICO_LOW_LEVEL_ARTIFACT_DIR/aico_hw_evidence-fail/failures.txt"
grep -qE '^\[FAIL\] [0-9T:-]+Z \(uptime [0-9.?]+ s\) mcu / ' "$FAILURES" || { cat "$FAILURES" >&2; fail "failures.txt entry header"; }
expect_line "details with context in failures.txt" "$out"

kill "$SERVER_PID" 2>/dev/null || true
wait "$SERVER_PID" 2>/dev/null || true
SERVER_PID=""

# 7. HDMI input-mode switching against fake frame_service / v4l2 / mode tool --
FAKE="$WORK/hdmibin"
mkdir -p "$FAKE" "$WORK/art"
export FAKE_HDMI_DIR="$WORK" HDMI_MODE_FILE="$WORK/hdmi-mode"
echo 0 >"$WORK/seq"
cat >"$FAKE/frame_service_cli" <<'EOF'
#!/bin/sh
while [ "$#" -gt 0 ]; do
    case "$1" in --socket) shift 2 ;; --out) out="$2"; shift 2 ;; *) cmd="$1"; shift ;; esac
done
seq=$(( $(cat "$FAKE_HDMI_DIR/seq") + 3 ))
echo "$seq" >"$FAKE_HDMI_DIR/seq"
case "$cmd" in
    health) echo "state=RUNNING capture_mode=buffered latest_seq=$seq frame_age_ms=20 ring=3/4 consecutive_failures=0 last_error=- last_recovery_ts=0 avg_frame_serve_latency_ms=1.0 avg_capture_copy_latency_ms=1.0" ;;
    latest-frame) echo "seq=$seq width=1920 height=1080 pixel_format=NV12 bytes=3110400 stale=false" ;;
    screenshot) cp "$FAKE_HDMI_DIR/ui.bmp" "$out" ;;
esac
EOF
cat >"$FAKE/aico-hdmi-mode" <<'EOF'
#!/bin/sh
if [ "${1:-status}" = "status" ]; then
    if [ -f "$HDMI_MODE_FILE" ]; then echo "Configured HDMI mode: $(cat "$HDMI_MODE_FILE") (saved)"; else echo "Configured HDMI mode: 1080p60 (default)"; fi
else
    echo "$1" >"$HDMI_MODE_FILE"
fi
EOF
cat >"$FAKE/v4l2-ctl" <<'EOF'
#!/bin/sh
mode="$(cat "$HDMI_MODE_FILE" 2>/dev/null || echo 1080p60)"
echo "	Active width: 1920"
echo "	Active height: 1080"
echo "	Pixelclock: 148500000 Hz (${mode#1080p}.00 frames per second)"
EOF
printf '#!/bin/sh\nexit 0\n' >"$FAKE/systemctl"
command -v timeout >/dev/null 2>&1 || printf '#!/bin/sh\nshift\nexec "$@"\n' >"$FAKE/timeout"
chmod +x "$FAKE"/*
out="$(AICO_HW_TEST_ROOT=1 HDMI_SUBDEV=/dev/fake-subdev PATH="$FAKE:$PATH" sh "$SCRIPT_DIR/board_hw_hdmi.sh" \
    --modes all --no-jpeg --fps-seconds 1 --min-fps 1 --settle-seconds 5 --artifact-dir "$WORK/art" --run-id evidence-hdmi 2>&1)" || true
ls "$WORK/art/aico_hw_evidence-hdmi/"hdmi_1080p30.bmp >/dev/null 2>&1 || fail "HDMI screenshots should be kept in the evidence directory"
for line in "[PASS] DV timings locked: 1920x1080@60.00" "[PASS] 1080p30: selected and capture restarted" \
    "[PASS] 1080p30: receiver timings 1920x1080@30.00 match 30 Hz" "[PASS] 1080p60: receiver timings 1920x1080@60.00 match 60 Hz" \
    "[PASS] 1080p30: capture size matches 1920x1080" "[PASS] restored the original selection (default)"; do
    expect_line "$line" "$out"
done
[ ! -e "$HDMI_MODE_FILE" ] || fail "HDMI mode file should be removed when no mode was saved before the test"
out="$(sh "$SCRIPT_DIR/board_hw_hdmi.sh" --modes 720p 2>&1)" && fail "unknown HDMI mode should be rejected"
expect_line "--modes accepts 1080p30, 1080p60 or all" "$out"

# 8. BLE phone connection against a fake ble_service --------------------------
BLE="$WORK/ble.sock"
python3 - "$BLE" <<'PY' &
import json, socket, struct, sys
server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
server.bind(sys.argv[1])
server.listen(16)
calls = 0
pairing = False
while True:
    conn, _ = server.accept()
    with conn:
        size, _extra = struct.unpack("<IQ", conn.recv(12))
        req = json.loads(conn.recv(size))
        calls += 1
        if req["op"] == "pairing_start":
            pairing = True
        connected = pairing and calls > 3
        status = {"backend_available": True, "adapter_powered": True, "gatt_registered": True, "advertising": not connected,
                  "device_name": "Aico-TEST", "pairing_open": pairing, "pairing_deadline": "soon",
                  "bonded_device_count": 1 if connected else 0, "connected": connected, "paired": connected,
                  "services_resolved": connected, "ancs_subscribed": False,
                  "connected_device_name": "Test Phone" if connected else "", "connected_device_address": "AA:BB" if connected else ""}
        data = json.dumps({"status": "OK", "bluetooth": status}).encode()
        conn.sendall(struct.pack("<IQ", len(data), 0) + data)
PY
SERVER_PID=$!
i=0
while [ ! -S "$BLE" ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
out="$(BLE_SOCKET="$BLE" sh "$SCRIPT_DIR/board_hw_peripherals.sh" --only bluetooth --interactive --ble-connect --ble-pair --ble-seconds 20 --run-id evidence-ble 2>&1)" || true
for line in "[PASS] BLE adapter powered" "[PASS] BLE pairing window opened" "[PASS] phone connected over BLE" \
    "[PASS] BLE link is paired (encrypted)" "[PASS] GATT services resolved" "[PASS] new bond stored (0 -> 1)"; do
    expect_line "$line" "$out"
done
# A named interface that does not exist: no host network is touched.
out="$(sh "$SCRIPT_DIR/board_hw_peripherals.sh" --only wifi --wifi-iface aicotest0 --run-id evidence-wifi 2>&1)" && fail "missing Wi-Fi interface should fail"
expect_line "[FAIL] no Wi-Fi interface (aicotest0)" "$out"

out="$(sh "$SCRIPT_DIR/board_hw_peripherals.sh" --only wifi,nosuch 2>&1)" && fail "unknown peripheral group should be rejected"
expect_line "unknown group: nosuch" "$out"

# 9. Suite discovery: a new board_hw_<name>.sh with a header line is picked up
#    by the entry point without editing it.
PLUGIN="$WORK/plugin"
cp -R "$SCRIPT_DIR" "$PLUGIN"
cat >"$PLUGIN/board_hw_dummy.sh" <<'EOF2'
#!/bin/sh
# aico-hw-suite: 90 dummy - example suite added by the host test
SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/board_hw_common.sh"
hw_start dummy "$@"
DUMMY_VALUE=default
hw_load_config
while [ "$#" -gt 0 ]; do
    case "$1" in
        --artifact-dir|--run-id|--config) shift 2 ;;
        *) shift ;;
    esac
done
pass "dummy value $DUMMY_VALUE"
hw_finish
EOF2
expect_line "dummy        board_hw_dummy.sh: example suite added by the host test" "$(sh "$PLUGIN/board_hw_test.sh" --help)"
printf 'DUMMY_VALUE=from-config\n' >"$WORK/dummy.conf"
out="$(sh "$PLUGIN/board_hw_test.sh" --only dummy --config "$WORK/dummy.conf" --run-id discovery 2>&1)" || {
    printf '%s\n' "$out" >&2
    fail "discovered suite should run and pass"
}
expect_line "[PASS] dummy value from-config" "$out"
printf '%s\n' "$out" | grep -qE '^dummy +PASS +pass=1' || fail "discovered suite missing from the summary"

# A suite that stops without a [FAIL] line still fails the run.
cat >"$PLUGIN/board_hw_crash.sh" <<'EOF2'
#!/bin/sh
# aico-hw-suite: 95 crash - suite that exits early, added by the host test
SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/board_hw_common.sh"
hw_start crash "$@"
pass "about to stop"
exit 3
EOF2
out="$(sh "$PLUGIN/board_hw_test.sh" --only dummy,crash --run-id crash-run 2>&1)" && fail "a suite exiting nonzero should fail the run"
printf '%s\n' "$out" | grep -qE '^crash +FAIL' || { printf '%s\n' "$out" >&2; fail "crashed suite should be FAIL in the summary"; }
expect_line "crash exited with status 2 without reporting a failure" "$out"
python3 - "$AICO_LOW_LEVEL_ARTIFACT_DIR/aico_hw_crash-run/run.json" <<'PY' || fail "run.json should mark the crashed suite FAIL"
import json, sys
suites = json.load(open(sys.argv[1]))["suites"]
assert suites["crash"]["result"] == "FAIL" and suites["dummy"]["result"] == "PASS", suites
PY

# Suite names in --skip are literal and validated; evidence is never reused.
out="$(sh "$PLUGIN/board_hw_test.sh" --skip '.*' 2>&1)" && fail "--skip with an unknown name should be rejected"
expect_line "unknown suite: .*" "$out"
out="$(sh "$PLUGIN/board_hw_test.sh" --only dummy --run-id discovery 2>&1)" && fail "an existing evidence directory should be rejected"
expect_line "evidence directory already exists" "$out"

echo "board hardware test scripts: all host checks passed"
