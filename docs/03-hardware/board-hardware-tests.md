# Board Hardware Tests

`scripts/board/board_hw_test.sh` runs board-side hardware checks for a new or
repaired Aico board and writes one report. It complements the other
scripts in `scripts/board/`: `board_low_level_smoke.sh` (service, HID, HDMI and
audio smoke), `speaker_test.sh` (audible tones), `mcu_touch_test.sh` (touch
key) and `board_jpeg_smoke.py` (JPEG requests). The RTC test,
`scripts/board_rtc_test.sh`, runs on a host and drives the board over USB.

For the production-line procedure (flashing, login, running the tests and
keeping the records), see the [Factory Test Guide](factory-test-guide.md).

The suites run on the board as root. They use the local service sockets and
command-line tools only: no Agent, Config Web, SSH or HTTP API. The analysis
helper `board_hw_tools.py` uses the Python standard library only.

## Run

All board-side scripts live in `scripts/board/`. Copy the directory to the
board and run the entry point with a terminal attached (`ssh -t`), so the interactive prompts can read answers:

```bash
scp -r scripts/board aico@192.168.42.1:/tmp/
ssh -t aico@192.168.42.1 'sudo sh /tmp/board/board_hw_test.sh --interactive \
    --modes all --hotplug --sleep-test --charger --ble-connect \
    --iperf-server <lan-host>'
```

Without `--interactive`, steps that need a person (speech, LED confirmation,
touch key, wake word, TF card insertion, cable and charger handling) are
reported as `[SKIP]`, so an unattended run never waits for input. If the
operator's terminal goes away during an interactive run, the run reports one
`[FAIL]` for the lost input and skips the remaining interactive steps; an
unanswered confirmation is never counted as a hardware result.

Useful options of the entry point:

| Option | Effect |
| --- | --- |
| `--only audio,mcu` / `--skip hdmi` | Select suites: `lowlevel`, `audio`, `hdmi`, `mcu`, `peripherals` |
| `--modes all` / `--modes 1080p30,1080p60` | HDMI: switch through the input modes, then restore the original selection (root) |
| `--hotplug` | HDMI: cable unplug/replug (with `--interactive`) |
| `--sleep-test` | HDMI: source display off and back on with the cable connected (with `--interactive`) |
| `--expect-size 1920x1080` | HDMI: required capture size |
| `--charger` | MCU: charger unplug/replug (with `--interactive`) |
| `--power-key` | MCU: short power-switch press (with `--interactive`; only if a short press does not power off the board) |
| `--iperf-server HOST[:PORT]` | Wi-Fi TCP throughput both ways against `iperf3 -s` on a LAN computer (needs `iperf3` on the board) |
| `--download-url URL` | Wi-Fi download speed with curl, e.g. a large file served from a LAN computer |
| `--ble-connect` / `--ble-pair` | BLE: wait for a phone to connect; `--ble-pair` opens the pairing window first (with `--interactive`) |
| `--config FILE` | Settings for every suite, see [Configuration and extension](#configuration-and-extension) |
| `--artifact-dir DIR`, `--report PATH` | Where captures, screenshots and the report are written (default `/tmp`) |

Each suite also runs on its own; `sh board_hw_<suite>.sh --help` lists its
options and limits. The exit status is 0 only when no check failed.

### Configuration and extension

The suites contain no site-specific values. Without options they test only
what the board itself can verify: the Wi-Fi interface is detected, and the
network it joined is reported but not required. Servers, URLs, an expected
network name (`EXPECT_SSID`) and thresholds go in a settings file: copy
`scripts/board/board_hw.conf.example`, which lists every variable with its
default, and pass it with `--config FILE` (or `AICO_HW_CONFIG=FILE`) to the
entry point or a single suite. The file is a shell file of `VARIABLE=value`
lines; precedence is built-in defaults, then the file, then command-line
options.

New tests plug in without editing existing scripts:

- **Suite**: add `scripts/board/board_hw_<name>.sh` with the header line
  `# aico-hw-suite: <order> <name> - <description>`. It sources
  `board_hw_common.sh`, calls `hw_start <name> "$@"`, sets its defaults, calls
  `hw_load_config`, parses its options and ends with `hw_finish`. The entry
  point lists and runs it by that header, and it gets the same evidence
  bundle.
- **Peripheral check group**: add `group_<name>()` to
  `board_hw_peripherals.sh` and the name to `ALL_GROUPS`; `--only` and
  `--skip` (comma lists) select groups.

## What each suite checks

### Audio (`board_hw_audio.sh`)

| Check | Pass condition (default) |
| --- | --- |
| ALSA cards, DAC/ADC mixer controls | Playback and capture cards present |
| audio_service health, volume set/get | Health answers; a set volume reads back |
| Microphone channels | Captures 2 channels when the device allows, else 1. audio_service uses the left channel only, so a right channel that stays constant (no microphone on that input) is reported and left out of the analysis; a constant or all-zero left channel fails |
| Noise floor (quiet room, 3 s) | Quietest 100 ms window between -95 and -45 dBFS; no clipping; DC offset within 2% |
| Speaker to microphone, straight to ALSA | A tone plan (300 Hz to 6 kHz, -12 dBFS) is found on every live channel by matching per-frequency levels against the plan, which tolerates DC offset and start-up clicks; SNR >= 15 dB from 500 Hz to 4 kHz; THD at 1 kHz <= 10%; response spread <= 20 dB; round trip <= 600 ms (approximate, since the arecord start time is not exact); channels within 6 dB |
| Same loopback through audio_service | As above, through the product playback path and speaker amplifier enable |
| Volume curve | 1 kHz level rises by more than 1 dB at each audio_service volume step (30, 60, 90) |
| audio_service recording | 2 s of processed (VQE/DTLN) audio, not empty and not all zero |
| Voice (interactive) | Speech rises 15 dB above the quietest window; operator confirms the playback is clear |

The loopback plays from the board speaker and records with the board
microphone in the same room, so keep the room quiet. The ALSA capture runs
in parallel with audio_service; if the device is busy, rerun with
`--stop-audio-service`, which stops the service for the ALSA steps and
restarts it on exit. The original volume is restored on exit.

### HDMI (`board_hw_hdmi.sh`)

| Check | Pass condition |
| --- | --- |
| Receiver driver | RK628 or TC358743 probe message in this boot; no receiver/CIF errors |
| Devices and lock | `/dev/video0`; HDMI subdevice from `media-ctl -p`; `v4l2-ctl --query-dv-timings` returns nonzero active size; `/sys/kernel/debug/rk628/*/hdmirx/status` shows no unlocked channel |
| frame_service | State `RUNNING`; no consecutive capture failures |
| Capture rate | Sequence advances at least 10 frames/s (on-demand mode: a fresh frame per request) |
| Latest frame | Not stale; nonzero size; matches `--expect-size` |
| Screenshot content | Fewer than 98% black pixels; some luma variation and colours |
| Source change (interactive) | A second screenshot differs after the source changes; operator confirms the image |
| Input modes (`--modes`) | For each mode: `aico-hdmi-mode` applies it; capture locks within 90 s (`--settle-seconds`); receiver timings within 2 Hz of the mode (30 or 60); 1920x1080 frames; capture rate; screenshot content. The original selection is restored afterwards, including "no saved mode" |
| Hotplug (interactive, `--hotplug`) | Signal loss reported within 30 s of unplugging; capture recovers within 90 s of replugging (warning above 30 s); MCU plug-detect events reported |
| Source sleep (interactive, `--sleep-test`) | With the cable connected and the source display off, loss is reported within 30 s (stopped capture, stale frames, frame age over 3 s or no receiver timings); capture recovers after wake |
| JPEG path | `board_jpeg_smoke.py` with 3 iterations |
| Lock stability | No new lock-failure kernel lines during the test (hotplug excepted) |

Connect an awake source before running. A sleeping host can keep the display
enumerated while sending no video; see
[RK628F hardware adaptation](rk628f-adaptation.md).

### MCU (`board_hw_mcu.sh`)

| Check | Pass condition |
| --- | --- |
| mcu_service | Unit active, socket present |
| Link | `get_state`: UART connected, RV state `ready`, protocol version, recent frame, no invalid events |
| Battery | Percent 0-100; 3000-4500 mV; under 150 mV drift over the samples; charge state `not_charging`, `charging` or `full` |
| Wake-up timer | Set to 3600 s, read back, cancelled. Skipped when a timer is already active |
| LEDs | Red and white: on, blink, off are accepted and read back (operator confirms with `--interactive`), then returned to automatic mode |
| Touch key, wake word (interactive) | `touch_key` and `asr` wake events arrive within 30 s |
| Charger (interactive, `--charger`) | `external_power` events on unplug and replug; charge state follows |
| Power switch (interactive, `--power-key`) | `power_switch` event on a short press. Use only if a short press does not power off the board |

### Peripherals (`board_hw_peripherals.sh`)

| Group | Pass condition |
| --- | --- |
| system | No failed units; no kernel errors (warning); thermal zones at or below 80 C; no CPU voltage/regulator errors |
| wifi | Wi-Fi interface (first wireless interface, or `--wifi-iface`) present; associated (to `--expect-ssid` when given); RSSI >= -75 dBm (warning); IPv4 address; gateway ping loss <= 20% with average RTT and jitter; DNS of `--dns-host` (warning); NTP (warning); HTTPS with `--internet` (any HTTP status, target `--https-url`); with `--iperf-server` or `--download-url`, throughput bound to the Wi-Fi interface (warning below 5 Mbit/s, `--min-wifi-mbps`) |
| bluetooth | `hci0` present; ble_service adapter powered; GATT registered and advertising (warnings); with `--interactive --ble-connect`, a phone (Aico App or a BLE scanner) connects within 90 s, the link is paired and GATT services resolve; with `--ble-pair`, the pairing window opens and a new bond is stored |
| usb | `/dev/hidg0..2`; gadget bound to a UDC; host configured; `usb0` address |
| storage | `/userdata` writable; 32 MiB written and read back intact; write >= 5 MB/s and read >= 10 MB/s (warnings); no MMC I/O errors |
| TF card | SD card detected by `/sys/block/mmcblk*/device/type`; 4 MiB write/read-back on its mount point |
| npu | NPU, RGA and MPP device nodes; `rknn_vad --self-test` and `ui_grounding_helper --self-test` (warnings, since the Agent's resident helper can hold CMA) |
| rtc | `/dev/rtc0` ticking and within 5 s of the system clock. Power-off retention needs `board_rtc_test.sh` from a host |

`--only` and `--skip` take comma-separated groups. `--ble-pair` adds a bonded device, and
`--modes` changes the saved HDMI selection while it runs (it restores it), so
both are opt-in. There is no fan check: no fan control exists on the current
board.

## Limits and calibration

Defaults are starting points, not product specifications. Before using them
as acceptance limits, run the suites on several known-good boards in the
intended test environment and tighten or relax the options (`--max-noise-dbfs`,
`--min-snr-db`, `--min-fps`, `--min-rssi`, `--max-temp-c`; the loopback and
statistics limits are options of `board_hw_tools.py`). Keep the measured
values from the report as the baseline for later boards.

## Evidence of a run

Every run writes an evidence directory, `<artifact-dir>/aico_hw_<run-id>/`
(default `/tmp`), and a tarball of it next to it. The directory is created
fresh and private to the user running the tests; a run never reuses an
existing one, so pass a new `--run-id` for every run (the default timestamp id
gets the process id appended if it is taken). Together they show that the
tests ran on this board, when, with which scripts, and what they measured:

| File | Content |
| --- | --- |
| `failures.txt` | Start here when a run fails: every FAIL, then every WARN, with UTC time, seconds since boot, suite and section, the message and the detail lines the check printed under it (receiver status, error replies). The same list is in `run.json` under `failures` |
| `<suite>.log` | Full console transcript of each suite (`lowlevel.log` for the low-level smoke, `all.log` for the entry point), with a header giving the start time, uptime, boot id, host, operator and command line, and a footer with the end time and exit code |
| `events.jsonl` | One JSON line per section, check result, prompt and operator answer, recorded as it happens, with UTC time, seconds since boot and the section it belongs to |
| `run.json` | Run id, start and end, exit code, board identity (boot id, serial, MAC addresses, model, kernel, `/etc/aico-release`, installed `aico*` packages, OS release, uptime, operator), clock state at the end of the run (NTP enabled and synchronized, time zone, RTC time and its offset from the system clock, which tells whether the timestamps can be trusted), per-suite counts, time span and exit code, failures, SHA-256 of every script, size and SHA-256 of every artifact |
| `kernel.log` | Kernel messages logged since the run started (receiver locks, USB, audio, MMC); never older messages. Without `journalctl` it uses `dmesg` lines stamped after the start |
| `*.pcm`, `*.json`, `*.bmp` | Microphone captures (16-bit little-endian at the reported rate and channel count), loopback measurements and screenshots |
| `report.txt` | The entry point's report |
| `SHA256SUMS` | SHA-256 of every file in the directory |

The last lines of the console print the run id, board serial and boot id,
each suite's result and time span, the clock state, every failure with its time and section (details in `failures.txt`), the tarball path and its SHA-256. Keep the
tarball, or at least record its SHA-256 with the board serial, as the test
record; `sha256sum -c SHA256SUMS` inside the extracted directory shows that
nothing was changed afterwards. Copy the captures to a host to listen to them
or the screenshots to inspect them.

## Host test

`scripts/test_board_hw_tests.sh` (host side) runs in the `script-contracts` suite. It
checks shell syntax, the help texts and the local-only policy, runs the
analysis self-test, a loopback through fake `aplay`/`arecord` (healthy, dead
and single-dead microphone cases), screenshot analysis, and the MCU suite
against a fake mcu_service socket, HDMI input-mode switching with fake
`frame_service_cli`, `aico-hdmi-mode` and `v4l2-ctl`, and a BLE phone
connection with pairing against a fake ble_service socket.
