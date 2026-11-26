# GX8002D keyword wakeup replacement

The replacement keeps the existing voice activation contract:

```text
GX8002D keyword recognition -> active-high GPIO pulse -> GD32 PA0
  -> MCU V1 WAKEUP (source 6) -> mcu_service -> Agent voice session
```

The chip-side implementation lives in the companion `aico-asr` repository,
under `sdk/gx8002/`. It integrates with NationalChip's
[lvp_aiot](https://github.com/NationalChip/lvp_aiot) application interface and
[Viva](https://github.com/NationalChip/viva) configuration generator. Its Viva
project defines `艾登` as a main wake word with event ID 100. The SDK's event ID
100 is local to the recognizer; it is not an MCU wire-protocol command or source.

The product is now named Aico, with the requested wake word pronounced
/ˈaɪkoʊ/. The implementation described here still uses the legacy `艾登`
keyword. English wake-word integration and acoustic validation remain pending;
see the companion repository's `sdk/gx8002/lvp-tws-assessment.md` for the
separate vendor SDK assessment. Renaming the product does not change the
recognition model or establish support for the new word.

## Hardware assumptions

The supplied `aiden_sch_v1_finalnew1.pdf` describes the original ASRPRO board,
not a GX8002 board. The following mapping is a proposed replacement design.
Confirm it against the new schematic before programming a device.

| Function | GX8002D replacement | Existing Aico destination |
| --- | --- | --- |
| Wake output | GPIO_09, QFN20 pin 10, mux function 1 | `ASRPro_WK_MCU_PA2` net to GD32 PA0/WKUP0 |
| Microphone | ADC_P pin 16, ADC_N pin 17; mono 16 kHz ADC | Reworked differential microphone front end |
| Application log | UART0 TX pin 6, RX pin 7; SDK 115200 8N1 | Debug/programming connector after electrical redesign |
| Recognizer power | Correctly regulated always-on GX8002 rails | Preserve MCU `ASR_PWREN` lifecycle |

GPIO_09 is reserved for wake output; SPI master and I2S record/playback modes
that use it must remain disabled. Package pin 10 is not GPIO 10. GX8002 GPIO_02
(package pin 20) participates in boot-time I2C address selection and is not used
for wake output.

The original U29 boost circuit supplies approximately 5 V to `V_ASR`. It cannot
directly power GX8002. Follow GX8002 datasheet V1.15 sections 3.4, 4.6, 4.7 and
6 for IO/core voltages, LDO configuration and power-up timing. Use a 3.3 V IO
domain compatible with GD32 PA0, or provide level conversion. Provide an idle-low
pull-down that also holds the MCU input low while GX8002 is reset or unpowered.
UART0_RX and UART1_RX must have stable levels during power-on/reset.

The original microphone bias is supplied by ASRPRO. GX8002's AVDD_CAP pin is
an analog decoupling node, not a replacement MICBIAS output. Supply a suitable
external microphone bias and preserve RV1106 microphone access. GX8002 has a
single analog ADC channel: the original ASRPRO right-channel speaker-reference
input cannot be retained as a second analog input. Acoustic echo cancellation
and recognition during playback require separate evaluation. The board speaker
continues to use the RV1106 audio path.

GX8002 is not pin-compatible with ASRPRO. ASRPRO firmware images, model blobs,
PA4/PG_EN boot strapping and ASRPRO serial flashing tools do not apply.

## Software contract

The adapter selects only the configured event with the SDK main-wake-word flag.
VAD activity, boot/resume notifications, ordinary commands and unknown events
do not notify the MCU. The output starts low and remains low on resume.

An accepted word drives GPIO_09 high for a nominal 100 ms, followed by at least
400 ms low before another word is admitted. Timing is serviced by the SDK main
loop; a delayed iteration may lengthen the high interval. Duplicate events are
discarded without extending the pulse or queuing a second wake. A PMU lock
prevents GX8002 standby during the pulse and rearm interval. GPIO deassertion
failures retain the lock and are retried in the task loop. The lock is allocated
once because the SDK retains application state and re-enters `main` on resume.

The existing MCU firmware qualifies PA0 high and low for 30 ms. It handles RV
power-on/resume and sends V1 `EVENT`, command `0x07`, payload `06 00 00 00` after
RV readiness. The maintained switch OFF state continues to inhibit all wake
sources. Keep the ASR rail available while RV is off if voice wake from software
poweroff is required. No RV UART-to-GX8002 parser or device-tree change is needed
for this GPIO contract.

`mcu_service` retains `source: "asr"`, `source_code: 6`. Agent STT and realtime
activation, interruption, freshness checks and event cursors keep their existing
behavior. See [MCU service](../03-services/mcu-service.md#agent-voice-activation).

## Validation

The companion repository provides host tests compiled against real `lvp_aiot`
headers and the actual application source. They exercise filtering, pulse and
rearm timing, duplicate suppression, timer wrap, repeated standby/resume and
hardware-call failures. Viva model generation, target cross-compilation and
on-board recognition are separate checks; host success does not establish
recognition accuracy, electrical timing or power consumption.

After a deliberately provisioned replacement board is available:

1. Measure the supplies and microphone bias; confirm PA0 stays low during cold
   boot, reset, silence and GX8002 VAD-only resumes.
2. Say the configured wake word and measure GPIO_09 and PA0 together. Verify
   the high pulse and low rearm interval, including repeated recognition.
3. With the maintained switch ON, validate running, suspend/resume, and
   coordinated software-poweroff wake. Check that RV readiness forwards the
   existing ASR event exactly once per admitted MCU notification.
4. Confirm STT and realtime voice sessions start and interruption behaves as
   configured. Verify that switch OFF suppresses wake and that switching back
   ON does not replay an inhibited pulse.
5. Measure false activations, missed words, distance, noise and playback cases.
   Tune the Viva threshold using measurements; the two-syllable word `艾登`
   needs acoustic acceptance testing.

The existing read-only event check is reusable:

```sh
python3 scripts/test_mcu_after_flash.py --expect-source asr --timeout 60
```

This checks event forwarding, not recording/cloud recognition/speaker output.
No board flashing is part of the software adaptation.

## References

- `GX8002_datasheet_V1.15.pdf`: pin mux table 3-1; power/analog table 3-2;
  power/reset sections 4.6-4.7; mono ADC section 4.8.2.
- `aiden_sch_v1_finalnew1.pdf`: sheet 1 power and GD32 PA0; sheet 14 ASRPRO,
  microphone bias, RV microphone tap and speaker-reference input.
- `aico-asr/sdk/gx8002/README.md`: pinned SDK/Viva revisions, build steps and
  adaptation test commands.
