# SCH V2.19 adaptation

Schematic `AIDEN_SCH_V2_19.pdf` (sheets dated 2026-11-23 and 2026-11-24)
replaces the charger and the voice chip of Aico Link SCH v1 and adds an
expansion connector. This page lists what changed for firmware and how the
RV1106 image and the GD32 MCU image handle it.

No SCH V2.19 board was available when this adaptation was written. Everything
below is derived from the schematic, the SGM41574 datasheet and the RV1106
pinctrl tables, and is covered by host tests only. See
[Not verified on hardware](#not-verified-on-hardware).

## One image for both revisions

The RV1106 image and the MCU image each serve SCH v1 and SCH V2.19.

- **RV1106.** Every pin the two revisions use differently is unconnected on
  the other revision, so one device tree describes both. The model string
  stays `Aico Link SCH v1`; userspace and the image audit use it as the board
  family name.
- **MCU.** The firmware probes the charger on its I2C bus: an SGM41574 at
  `0x6B` selects V2.19 behavior and an MP2720 at `0x3F` selects v1 behavior.
  Until a charger answers, the voice and external wake inputs are ignored.
  The MCU debug log reports the result in a `board:` line.

## Hardware changes that affect firmware

| Area | SCH v1 | SCH V2.19 | Handled by |
| --- | --- | --- | --- |
| Charger | MP2720, I2C `0x3F` | SGM41574 buck-boost (U71), I2C `0x6B`, 5–20 V input, ship FET U72 | MCU |
| Charger interrupt, MCU PA5 | Charger INT | Charger INT# and BQ27220 GPOUT wired together | MCU (unchanged falling-edge refresh) |
| Voice chip | ASRPRO | GX8002D (J9) with separate 3.3 V (U31) and 1.2 V core (U30) supplies | MCU |
| Voice wake, MCU PA0 | Active high, idle low | Pulled up to MCU_3V3 by R224, active low | MCU |
| Voice core supply enable, MCU PB0 | Not connected | GX8002_CORE_PWR_EN | MCU |
| Voice UART, RV1106 pins 91/90 (UART4 M1) | ASRPRO UART | GX8002 UART0 through R210/R173 | Unchanged (`/dev/ttyS4`) |
| Microphone | On-board microphone, buffered by U25 into codec MIC0; bias from ASRPRO | Microphone on FPC connector J10, same U25 buffer into codec MIC0; bias and buffer supply from the GX8002 3.3 V rail. Codec MIC1 and MICBIAS go to J2-5/6/7 | Unchanged capture path. It needs MCU PA1 on, which holds whenever the RV1106 is powered |
| RV1106 pin 87, GPIO1_D1 | RV_HOLD to MCU PA8 | Not connected | Device tree keeps the output; no effect |
| MCU PA8 | RV_HOLD | EXT_WAKE_IN from J2-18 | MCU, wake source 7 |
| RV1106 pin 63, GPIO0_A4 | Not connected | Deep-sleep wake line from MCU PA12, R205 pull-down | Device tree `mcu-wake` key, MCU pulse |
| RV1106 pins 64/65, GPIO0_A5/A6 | UART1 RTS/CTS on the header | I2C1 on J2-13/14, 4.7 kΩ pull-ups R226/R227 | Device tree enables I2C1 M0 |
| RV1106 pins 77/83/84, GPIO1_B0/C6/C7 | Not connected | GPIOs on J2-15/16/17 | Left as unclaimed GPIOs |
| RV1106 pins 7/8, GPIO3_D1/D2 | I2C3 on the header | Not connected | I2C3 stays disabled |
| Recovery | Button SW2 on the SARADC pin | No button; RECOVERY_3V3 from J2-18 in debug mode pulls the pin low through Q8 | Unchanged (`recovery` key, active low) |
| VDD_ARM regulator | SY8892 | JW5292 (U36), same feedback and PWM network | Unchanged PWM regulator |
| Expansion connector | J17 | J2, 21 pins, with analog switch U70 | See below |

Parts that changed without a firmware effect: the Type-C connectors, the
5–20 V to 5 V converter (MP2328), the 1.8 V, DDR and RK628 1.1 V converters
(JW5292), the 24 MHz crystals and the speaker connector.

## Expansion connector J2

J2 pins 13–18 pass through analog switch U70 (TS3A27518E). The MODE pin
(J2-12, pulled down by R228) selects what they carry. Neither the RV1106 nor
the MCU can read or drive MODE.

| J2 pin | MODE low (default) | MODE high (debug) |
| ---: | --- | --- |
| 13 | RV1106 I2C1 SCL (GPIO0_A5) | GX8002 UART0 RX |
| 14 | RV1106 I2C1 SDA (GPIO0_A6) | GX8002 UART0 TX |
| 15 | RV1106 GPIO1_B0 | GX8002_CORE_PWR_EN |
| 16 | RV1106 GPIO1_C6 | MCU SWDIO |
| 17 | RV1106 GPIO1_C7 | MCU SWCLK |
| 18 | EXT_WAKE_IN (MCU PA8) | RECOVERY_3V3 |

Fixed pins: 1 VCHG, 2 and 21 GND, 3/4 RV1106 debug UART TX/RX, 5/6 RV_MIC1
P/N, 7 CODEC_MICBIAS, 8/9 RV1106 UART1 RX/TX, 10/11 MCU debug UART RX/TX,
12 MODE, 19 touch key, 20 MCU_3V3.

In debug mode, pulling J2-18 high holds the RV1106 recovery pin low. Doing so
across a reset enters Loader mode; at run time it is reported as the
`recovery` key.

## RV1106 device tree

`rv1106g-aico-custom.dts` changes:

- **`mcu-wake` key.** GPIO0_A4 is a `gpio-keys` entry with `KEY_WAKEUP`,
  active high, `wakeup-source`, and an internal pull-down so the pin stays low
  on SCH v1, where it is unconnected. The line is the board's deep-sleep
  wake source: GPIO0 is the PMU bank, which stays powered in deep sleep. Each
  pulse also arrives as a `KEY_WAKEUP` press and release on the `gpio-keys`
  input device; nothing consumes those events.
- **I2C1.** Enabled at 100 kHz on the M0 pins (`/dev/i2c-1`) for devices
  attached to J2. No device is described on it. On SCH v1 the two pins are on
  the header without pull-ups and the bus is idle. I2C1 M1 must not be
  selected: it takes the Bluetooth UART0 pins.
- **Comments** record which nodes apply to which revision. The `rv-hold`
  output, UART4 for the voice chip, UART5 for the MCU, the RK628, TF card,
  Wi-Fi/Bluetooth, audio and CPU regulator nodes are unchanged.

GPIO1_B0, GPIO1_C6 and GPIO1_C7 are not claimed by any node, so userspace can
request them. GPIO1_C6 and GPIO1_C7 are also the UART4 M1 CTS/RTS pins; the
voice UART does not use flow control and must not select them.

The image rebuild is required for these changes: the device tree is part of
the BSP boot image, not of the Agent package.

## MCU firmware

The matching change is in the `aico-mcu` repository; its
`docs/application.md` describes the behavior in full. In summary, on V2.19:

- **Charge current.** The SGM41574 starts with a 2 A fast-charge limit. The
  MCU sets 700 mA (0.5 C for the 1400 mAh cell), disables the charger's I2C
  watchdog so the limit is not reset, and rechecks both on every battery
  refresh. Battery percentage, voltage and charge state reach the RV1106
  through the same `GET_BATTERY` reply as before.
- **GX8002 power.** The MCU switches the 3.3 V supply and the 1.2 V core
  supply together with the former ASR supply policy: on while the RV1106 is
  powered or software-off with the switch ON, off after a switch OFF.
- **GX8002 wake.** A low level on the wake line, stable for 30 ms, at least
  500 ms after the supplies came up, is reported as wake source `asr` (6).
- **EXT_WAKE_IN.** A high level stable for 30 ms is reported as wake source
  `external_input` (7). It can start a software-powered-off RV1106 like the
  touch key, and the maintained switch gates it like every other source.
- **Deep-sleep wake line.** The MCU raises GPIO0_A4 for 100 ms with every
  wake it reports and with every shutdown request, in addition to the UART
  frame, whether or not the RV1106 is suspended. A wake that powers the
  RV1106 on is signalled once the system is ready, together with its
  `wakeup` event. Power-on by the maintained switch is not a wake and gives
  no pulse.
- **Frame after the pulse.** While the RV1106 holds a suspend grant, the MCU
  sends the pulse first and keeps the `wakeup` event or shutdown request
  until `aico-power` reports the resume (`suspend_resume`, its first action
  after the kernel returns), for at most 2 s. An RV1106 in deep sleep is
  therefore woken by the pulse and still receives the event with its source.
  A running RV1106 gets pulse and event together with no added delay.

## RV1106 userspace

- `mcu_service` names wake source 7 `external_input`. No application acts on
  it. An older service reports it as `unknown` with `source_code: 7`.
- The voice path is unchanged: the Agent starts voice interaction from the
  `asr` wake event, whichever chip produced it.
- `aico-power sleep` (s2idle) is unchanged and still wakes through UART5.
  The `mcu-wake` key adds a second wake path.
- On SCH V2.19, `aico-power sleep` now receives its `wakeup` event after
  the resume report instead of as the frame that wakes the system.
- `aico-power deep-sleep` stays disabled. Besides board verification, the
  command cannot tell SCH v1, which has no wake line, from SCH V2.19, because
  both report the same model string.

## Not verified on hardware

Check these on the first V2.19 board, with the MCU and RV1106 images built
from this change:

- The MCU log shows `board: SGM41574 charger, SCH V2.19 (GX8002)`, and an
  SCH v1 board still shows the MP2720 line.
- Charge current, termination and temperature behavior with the 700 mA limit;
  battery state through `GET /api/mcu/battery`.
- GX8002 start-up, its wake-output polarity and pulse width, and the `asr`
  event (`scripts/test_mcu_after_flash.py --expect-source asr`).
- EXT_WAKE_IN with MODE low
  (`scripts/test_mcu_after_flash.py --expect-source external_input`).
- The wake line: `mcu-wake` appears in `/proc/interrupts` and counts once for
  each `wakeup` event, with and without `aico-power sleep`.
- The time from the pulse to the resume report against the MCU's 2 s wait,
  for s2idle and for deep sleep.
- A wake that arrives after the suspend grant but before the kernel sleeps.
  Its event is now held, so `aico-power` no longer sees it in the MCU state
  check; the suspend has to be stopped by the kernel wakeup count that the
  `mcu-wake` key raises.
- `/dev/i2c-1` with a device attached to J2-13/14.
- VDD_ARM at several CPU frequencies with the JW5292.
- Recovery entry through J2-18 in debug mode.
- GX8002 firmware download and its UART protocol on `/dev/ttyS4`, which this
  change does not implement.
