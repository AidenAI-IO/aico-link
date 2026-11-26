# Factory Test Guide

[中文版](factory-test-guide.zh-CN.md)

This guide is for the assembly (SMT/PCBA) line and the hardware engineers who
support it. It covers one Aico Link SCH v1 board from a blank flash to a
recorded test result:

1. [Prepare the test station](#1-prepare-the-test-station) (once per station)
2. [Flash the firmware](#2-flash-the-firmware)
3. [Log in to the board](#3-log-in-to-the-board)
4. [Run the hardware tests](#4-run-the-hardware-tests)
5. [Keep the test record](#5-keep-the-test-record)
6. [Read a failure](#6-read-a-failure)
7. [Prepare the board for packing](#7-prepare-the-board-for-packing)

What each check measures and its pass limits are in
[Board Hardware Tests](board-hardware-tests.md). This guide covers the
companion RV1106 side only; the MCU firmware is programmed separately.

> Flashing erases everything on the board, including `/userdata`. Never flash
> a board that a customer or engineer is still using without their consent.

## 1. Prepare the test station

### Equipment

| Item | Purpose |
| --- | --- |
| Test computer, Linux (Ubuntu 22.04 or later) or macOS | Flashing, login and running tests. Windows can flash (see below) but has no built-in driver for the board's USB network (CDC ECM), so use Linux or macOS for login and tests |
| USB-C data cable | Connects the computer to the board's USB-C port J3. A charge-only cable does not work |
| Video source with USB-C video output (DisplayPort Alt Mode) | Needed for the HDMI capture tests. A laptop whose USB-C port outputs video can be both the test computer and the video source |
| USB-to-TTL serial adapter (optional) | Debug console when the USB network does not come up |
| Wi-Fi access point for the line (optional) | Needed for the Wi-Fi checks; use a 2.4 GHz network |
| Quiet place, 5 V charger, TF card, phone with Bluetooth (optional) | For the interactive checks: speaker/microphone, charger detection, TF card, BLE connection |

### Release kit

Engineering provides one kit per firmware release. Do not mix files from
different releases.

| File | Use |
| --- | --- |
| `update.img` | Full firmware image |
| `update.img.sha256` | Its SHA-256, to confirm the file is complete |
| `board/` | The board test scripts (`scripts/board/` from the same release) |
| `board_hw.conf` (optional) | Line settings for the tests, e.g. the video size to expect or a speed-test server. Made from `board/board_hw.conf.example` |
| Station test password | The password the line sets on each board for testing (see step 3) |

### Flashing tools

- **Linux**: `pico-sdk/tools/linux/Linux_Upgrade_Tool/upgrade_tool` and,
  if the kit includes the repository, the guarded helper `scripts/flash.sh`.
  Add the tool to the station once and test it with
  `sudo ./upgrade_tool ld` (lists connected Rockchip devices).
- **macOS**: `upgrade_tool/upgrade_tool` from the repository root.
- **Windows**: in `pico-sdk/tools/windows/`, install `DriverAssitant` once,
  then use `SocToolKit` for single boards or `FactoryTool` for flashing
  several boards in parallel on a production line.

## 2. Flash the firmware

### 2.1 Check the image

Run this once per kit, not once per board:

```bash
sha256sum -c update.img.sha256      # macOS: shasum -a 256 -c update.img.sha256
```

The result must be `update.img: OK`. Otherwise copy the kit again.

### 2.2 Put the board in flashing mode (Maskrom/Loader)

- **New board, or one that does not boot**: hold the BOOT (Maskrom) key
  while connecting the USB-C cable to J3, then release it after about two
  seconds. A blank board enters Maskrom mode by itself.
- **Board that already runs the firmware**: log in (step 3) and run
  `sudo systemctl reboot --reboot-argument=loader`.

Confirm that the computer sees exactly one board:

```bash
sudo ./upgrade_tool ld
```

It must list one device in `Maskrom` or `Loader` mode. If nothing is listed,
try another cable or port and repeat the key sequence.

### 2.3 Write the image

On Linux with the repository helper (checks the hash and that only one board
is connected):

```bash
sudo scripts/flash.sh flash \
  --tool pico-sdk/tools/linux/Linux_Upgrade_Tool/upgrade_tool \
  --image update.img \
  --sha256 "$(awk '{print $1}' update.img.sha256)" \
  --confirm-erase-all-data
```

With the tool alone (Linux or macOS):

```bash
sudo ./upgrade_tool uf update.img
```

On Windows, load `update.img` in SocToolKit or FactoryTool and start the
upgrade. Every tool must finish with a success message (for `upgrade_tool`,
`Upgrade firmware ok.`). The board then restarts by itself.

### 2.4 First boot

The first boot takes longer than later ones (a few minutes): the system
grows its file systems and creates its identity. The board is ready when the computer gets
a new network interface with an address in `192.168.42.0/24`, and
`ping 192.168.42.1` answers.

## 3. Log in to the board

A freshly flashed board has **no login password**: the `aico` account is
locked until a device password is set. Set the station test password first.

### 3.1 Set the test password

1. Open `http://192.168.42.1` in a browser on the test computer. Over USB
   the page opens without signing in.
2. Go to **Advanced → Device password** and set the station test password.

### 3.2 SSH over USB (normal method)

```bash
ssh aico@192.168.42.1
```

Enter the station test password. A board flashed again gets a new SSH host
key; if SSH refuses with "REMOTE HOST IDENTIFICATION HAS CHANGED", remove the
old key and connect again:

```bash
ssh-keygen -R 192.168.42.1
```

Administrator commands use `sudo` and the same password.

### 3.3 Serial console (fallback)

Use it when the USB network does not appear.

- Connect the USB-to-TTL adapter to the board's debug UART (RV1106 UART2 M1;
  see the board silkscreen or schematic for the pads): adapter TX to board
  RX, adapter RX to board TX, and GND to GND. Do not connect the adapter's
  power pin.
- Open the port at **115200 baud, 8N1, no flow control**. For example:
  `screen /dev/ttyUSB0 115200` on Linux, or PuTTY on Windows.
- Press Enter, then log in as `aico` with the device password. If no device
  password has been set yet, the console shows boot messages but does not
  accept a login.

### 3.4 Join the line's Wi-Fi (for the Wi-Fi checks)

On `http://192.168.42.1`, open **Wi-Fi**, choose the line's access point and
enter its password. Skip this when the Wi-Fi checks are not part of the test
plan; they then report the board as not associated.

## 4. Run the hardware tests

### 4.1 Copy the test scripts

From the folder that contains the kit's `board/` folder:

```bash
scp -r board aico@192.168.42.1:/tmp/
```

The copy in `/tmp` disappears when the board restarts; copy it again after
every restart.

### 4.2 Unattended test (every board)

Connect the video source to J3 and make it show any picture. Then run:

```bash
ssh -t aico@192.168.42.1 \
  'sudo sh /tmp/board/board_hw_test.sh --artifact-dir /userdata/aico-hwtest'
```

With a line settings file, copy it with the scripts and add
`--config /tmp/board/board_hw.conf`.

It takes about three minutes and needs no one at the board. It checks the
system services, HID and USB, speaker and microphone (it plays test tones),
HDMI capture, the MCU (battery, timer, wake sources), Wi-Fi, Bluetooth,
storage, the NPU and the RTC.

### 4.3 Interactive test (sampling or repair)

Add `--interactive` to include the steps that need a person. The script
prompts before each step and waits for Enter or a `y`/`n` answer:

```bash
ssh -t aico@192.168.42.1 \
  'sudo sh /tmp/board/board_hw_test.sh --artifact-dir /userdata/aico-hwtest \
     --interactive --hotplug --charger --ble-connect'
```

| Option | What the operator does |
| --- | --- |
| `--interactive` | Speak into the microphone, confirm the LEDs, touch the touch key, say the wake word, insert a TF card |
| `--hotplug` | Unplug and replug the video cable when asked |
| `--sleep-test` | Turn the video source's screen off and on when asked |
| `--charger` | Unplug and replug the charger when asked |
| `--ble-connect` (with `--ble-pair` for a new phone) | Connect a phone to the board over Bluetooth |
| `--modes all` | Nothing; the script switches the HDMI input mode and restores it |

Run a single suite with `--only`, e.g. `--only audio` or `--only mcu,hdmi`.
`--help` lists every option.

### 4.4 Read the result

The end of the output looks like this:

```text
================ summary ================
lowlevel     PASS  pass=..  warn=0   fail=0   skip=..
audio        PASS  ...
hdmi         PASS  ...
mcu          PASS  ...
peripherals  PASS  ...
RESULT: no failures
...
================ evidence ================
run id      20261125T071150Z  (2026-11-25T07:11:50Z -> 2026-11-25T07:14:43Z, exit 0)
board       serial=168028e8bac29c2e boot_id=... host=aico
clock       2026-11-25T07:14:44Z ntp_synchronized=True rtc_minus_system=0s
failures    0 FAIL, 0 WARN; details with context in failures.txt
bundle      /userdata/aico-hwtest/aico_hw_20261125T071150Z.tar.gz
bundle sha256 <64 hex characters>
```

| Result | Meaning | Action |
| --- | --- | --- |
| `RESULT: no failures` and no `WARN` | Pass | Keep the record (step 5) |
| `RESULT: no failures` with `WARN` lines | Pass with remarks: the value is near its limit or could not be fully checked | Keep the record; list the warnings on the report for engineering |
| `RESULT: FAILED` | At least one check failed | Keep the record, then see step 6 |
| `[SKIP]` lines | The check needs an option, an accessory or a person (`--interactive`) | Not a failure |

Each `[FAIL]` and `[WARN]` is also listed under `failures` in the evidence
block with its time and section.

## 5. Keep the test record

Every run writes an evidence bundle on the board. It proves that this board
ran these tests and when: board serial number, boot ID, MAC addresses,
firmware version, the time of every check, the clock state, the kernel log of
the run, all measurements and the SHA-256 of every file.

Copy the bundle to the test computer and file it under the board serial
number printed in the evidence block:

```bash
mkdir -p records
scp 'aico@192.168.42.1:/userdata/aico-hwtest/aico_hw_*.tar.gz' records/
```

On the traveller or line report, write the serial number, the run id, the
result and the `bundle sha256` value. To check later that a record was not
changed:

```bash
sha256sum records/aico_hw_<run-id>.tar.gz          # must equal the recorded value
tar -xzf records/aico_hw_<run-id>.tar.gz -C /tmp
cd /tmp/aico_hw_<run-id> && sha256sum -c SHA256SUMS
```

The timestamps are trustworthy only when the clock was set. The `clock` line
shows `ntp_synchronized=True` when the board reached a time server (it needs
Wi-Fi with internet access). Without it, the times come from the board's RTC,
and the `uptime` values still give the order and duration of the checks.

## 6. Read a failure

Open `failures.txt` in the extracted bundle first. It lists every FAIL, then
every WARN, with the time, the suite and section, the message and the detail
lines the check printed:

```text
[FAIL] 2026-11-25T07:13:15Z (uptime 4070.03 s) hdmi / capture devices
  no locked DV timings on /dev/v4l-subdev2 (source asleep, cable or receiver lock problem)
    | status: plugin
    | Clk-Ch:Unlock	Ch0:Unlock	Ch1:Unlock	Ch2:Unlock
```

The full output around each failure is in `<suite>.log` (search for the
message), and kernel messages from the same minutes are in `kernel.log`.

First checks for common failures:

| Failure | Check first |
| --- | --- |
| `hdmi`: no locked DV timings, frame_service `RECOVERING`, screenshot failed | The video source is connected to J3, awake and outputting video over USB-C; run again with `--only hdmi` |
| `audio`: no speaker signal detected | Speaker connector and wiring; the board was not muted or covered |
| `audio`: noise floor too high, clipping | Test in a quieter place; then microphone assembly |
| `mcu`: UART link not connected, MCU service missing | MCU firmware programmed and running; MCU UART wiring |
| `peripherals` / wifi: interface missing | Wi-Fi module soldering and supply; MCU firmware (it powers the module) |
| `peripherals` / wifi: not associated | The board joined the line's access point (step 3.4) |
| `peripherals` / storage: TF card not detected | Card inserted and readable on a computer; card slot |
| `lowlevel`: `example_usb_hid not found` | Development helper missing from this firmware; report it to engineering but do not reject the board for it |

Rerun the failed suite only (`--only <suite>`) after fixing a connection. Each
run makes its own bundle; keep all of them, on the board and in `records/`. Send the bundle with the board to
engineering when the cause is not clear.

## 7. Prepare the board for packing

After the board passes and its record is copied, on `http://192.168.42.1`
open **Advanced → Device password** and clear the test password. This locks
the `aico` account again, as it was after flashing.
