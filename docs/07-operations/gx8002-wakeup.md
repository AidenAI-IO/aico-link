# GX8002 wakeup integration and validation

This integration lets an external GX8002 recognizer start the existing Aico
Link voice session through J17-10. It also fixes an RV1106 audio-service crash
observed when a recording session stopped with DTLN noise suppression enabled.

## Connections and audio ownership

| Connection | Function |
| --- | --- |
| GX8002 GPIO0 -> Aico Link J17-10 | Active-high wake pulse into GPIO1_A4 / Linux GPIO36 |
| Common GND | Shared signal reference |
| GX8002 UART0 TX/RX -> external USB serial adapter | Firmware download and diagnostic logs; separate from the wake GPIO |

Use 3.3 V logic and keep the wake line low when idle. The DTS reserves J17-10
as a GPIO input with a pull-down and disables its competing UART1 assignment.
Do not share this pin with UART1 or another output. The GX8002 preset emits a
nominal 100 ms high pulse followed by at least 400 ms low rearm time. All configured
phrases share this pulse; the Agent cannot determine the matched phrase from it.

On `Aico Link SCH v1`, the Agent adds the rising-edge GPIO watcher to the
existing MCU ASR/touch wake sources. Existing debounce, interruption and voice
session handling apply. If the GPIO watcher fails, the Agent logs the error
and retains the other wake sources. Other device models retain their existing
wake configuration.

GX8002 uses its own microphone to detect a wake phrase. The pulse carries no
audio. Once triggered, the Agent records the Aico Link analog microphone via
the RV1106 `rv1106-acodec` capture path and `aico-audio.service`. The tested
mixer uses `ADC Mode=SingadcL`. This direct RV GPIO connection supports a voice
trigger while RV is running; MCU poweroff wake and suspend wake have not been
implemented or validated for this pin.

## Firmware and tools

The current recognizer is a GX8002B AIOT V1.3.2 board with 512 KiB Flash.
The private diagnostic preset uses English CTC phoneme paths for `hi, aico`
and an approximation of `hi, ai4 ke3`, score threshold 930, 30 dB microphone
PGA, UART0 at 115200, LED feedback and a short audible wake tone. It does not
distinguish Mandarin tones. Linked-vowel alternatives improve phrase-boundary
coverage; recognition accuracy still requires labeled acoustic trials.

Tooling is maintained separately in the private
[Aico ASR GX8002 guide](https://github.com/AidenAI-IO/aico-asr/tree/codex/gx8002-wakeup/sdk/gx8002).
It records the private SDK repository, compiler/archive checksums, Docker/SSH
build workflow, native macOS downloader patches and verified readback procedure.
Access to the private SDK is required to reproduce the recognizer firmware.

| Operation | Tool / host |
| --- | --- |
| GX8002 target compile | Linux x86_64 C-SKY Tools V3.10.15 Minilibc abiv2, GCC 6.3.0, CK804 |
| Firmware layout checks | Private SDK `app/aico_wakeup/tools/check_image.py` against ELF and NOR image |
| GX8002 flashing | Native macOS bootx v1.13.11 with UART and serial-stage-handoff patches |
| Verified serial rates | ROM handshake 230400; transfer and current application console 115200 |
| RV1106 business software | This repository's Debian ARM build workflow |
| GD32 power MCU | Separate `aiden-mcu` build/test/official-GigaDevice flash workflow |

The target compiler/model tools need Linux; macOS flashing does not. The tested
576000 transfer failed, so the documented route uses 115200. The earlier public
GX8002D/Viva image is 616,400 bytes and does not fit the tested GX8002B board.
ASRPRO images, downloaders and PG_EN sequences are not interchangeable with GX8002.

## Audio teardown fix

The original audio binary crashed during DTLN tail processing after capture
had released its final Rockit SYS reference. The core showed a call through an
unmapped RKNN runtime function in `hybrid_lstm`, reached from
`AudioDenoiser::finish()` in the capture worker.

The service now initializes capture before DTLN and flushes/destroys DTLN before
capture teardown. Capture recovery uses the same ordering and recreates the
model after capture initialization. EOF is published after cleanup. DTLN stays
enabled; see [audio-service settings](../03-services/audio-service.md).

## Validation record and follow-up

The 2026-11-06 device checks covered:

- GX8002 image checks and full 243,720-byte Flash readback at 115200; image
  SHA256 `a2ae90d3164b43e151c90ea8fc829ff515a8c42094a0aa88c6d9cb10b449bc06`.
- Existing MCU firmware commit `83f13c6`: build/tests/dependency checks, full
  128 KiB Flash and option-byte verification, then successful boot recovery
  communication. There is no new MCU source patch in this integration.
- Fifteen 5-second recording/stop/drain cycles with DTLN enabled and two
  playback/capture overlap cases with opposite shutdown orders. Audio service
  PID stayed constant with zero automatic restarts and no leaked sessions.
- Real GX8002 pulse -> Agent recording -> prompt playback -> VAD speech end ->
  recording close, followed by return to wake waiting, without a crash.
- Required Docker `make check`, including GPIO/wake contracts and the DTLN
  lifetime/tail regression. Run it again after integration changes.

The device test used the earlier main-based GPIO commit `0b76b7465` plus the
audio cleanup patch; preparation of the PR does not flash a new full image.
The separately deployed audio binary had SHA256
`cbd305bfe6b9496917dba3cdaed0a96e7d00889f5305cd65dc3c511dfc4c5069`.
Existing full-image artifacts from before that deployment do not include the fix.

Successful wake/recording does not by itself prove a cloud conversation. The
tested device enabled speaker verification without enrolled profiles and logged
`rejected utterance reason=no_profiles`; streaming STT returned an empty
transcript while those rejected turns were discarded. Enroll a speaker, or
deliberately change that policy, before evaluating the rest of the conversation.
That setting is independent of the repaired audio teardown.

For diagnosis, correlate `[AICO][WAKE]`/CTC logs on GX8002 with the Agent's
`GX8002 GPIO0 pulse received on J17-10` line, audio `dtln_enabled` and session
logs, then `Record session closed`. Check `systemctl show aico-audio.service
-p NRestarts -p MainPID` and `audio_service_cli health` after the session.
Keep device backups, raw logs, core dumps and generated binaries outside Git.
