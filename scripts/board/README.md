# Board-side scripts

Everything in this directory runs **on the Aico board**, as root, using local
service sockets and command-line tools only (no Agent, Config Web, SSH or HTTP
API). Copy the whole directory to the board; the scripts find their helpers
next to themselves.

```bash
scp -r scripts/board aico@192.168.42.1:/tmp/
ssh -t aico@192.168.42.1 'sudo sh /tmp/board/board_hw_test.sh --interactive'
```

Use `ssh -t` for interactive runs so prompts can read answers. Every script
prints `[PASS]`, `[WARN]`, `[FAIL]` or `[SKIP]` lines and exits non-zero when a
check failed; `--help` lists its options.

The `board_hw_*` suites also leave evidence of each run in
`/tmp/aico_hw_<run-id>/` and `/tmp/aico_hw_<run-id>.tar.gz`: console
transcripts, timestamped events, a failure list with details (`failures.txt`), board identity, firmware and clock state (`run.json`),
script and artifact hashes, the kernel log of the run, all captures and
screenshots, and `SHA256SUMS`. The console ends with the tarball's SHA-256.
Copy the tarball back to keep the record:

```bash
scp aico@192.168.42.1:/tmp/aico_hw_*.tar.gz .
```

## Hardware test suites

| Script | Covers |
| --- | --- |
| `board_hw_test.sh` | Entry point: runs the suites below plus the low-level smoke and writes one report |
| `board_hw_audio.sh` | Microphone noise floor and channels, speaker-to-microphone loopback (level, SNR, THD, latency), volume curve, audio_service paths, voice capture |
| `board_hw_hdmi.sh` | Receiver lock, capture rate and content, input modes (`--modes`), cable hotplug, source sleep, JPEG path |
| `board_hw_mcu.sh` | MCU link, battery and charger, wake-up timer, LEDs, touch key, wake word, charger and power-switch events |
| `board_hw_peripherals.sh` | System health, Wi-Fi (incl. throughput), Bluetooth (incl. phone connection), USB gadget, storage and TF card, NPU models, RTC |

Site-specific values (servers, URLs, an expected Wi-Fi network, thresholds)
are never built in. Put them in a copy of `board_hw.conf.example` and pass it
with `--config FILE` to the entry point or any suite; options on the command
line override the file.

To add a suite, create `board_hw_<name>.sh` with a header line
`# aico-hw-suite: <order> <name> - <description>`; `board_hw_test.sh` finds it
without changes (the steps are listed at the top of `board_hw_common.sh`). To
add a check group to the peripherals suite, write `group_<name>()` and add the
name to `ALL_GROUPS`.

Shared code: `board_hw_common.sh` (prompts and helpers) and `board_hw_tools.py`
(signal analysis, screenshot analysis, MCU/BLE socket client; Python standard
library only). Details and pass conditions:
[docs/03-hardware/board-hardware-tests.md](../../docs/03-hardware/board-hardware-tests.md).

## Smoke and single-purpose checks

| Script | Covers |
| --- | --- |
| `board_low_level_smoke.sh` | Services, HID, HDMI, audio and logs smoke; delegates to `board_low_level_{system,hid,hdmi,audio,logs}.sh`, sharing `board_low_level_common.sh` |
| `speaker_test.sh` | Audible test tones through audio_service or straight to ALSA |
| `mcu_touch_test.sh` | Counts capacitive touch-key events from the MCU service log |
| `board_jpeg_smoke.py` | frame_service JPEG requests: formats, crops, latency |
| `setup_audio_volume.sh` | Sets the DAC output mixers to maximum (operations helper, not a test) |

## Not here

- `scripts/board_rtc_test.sh` runs on a **host** and drives the board over USB.
- `scripts/test_board_low_level_smoke.sh` and `scripts/test_board_hw_tests.sh`
  are host-side tests of these scripts (run in CI).
