---
sidebar_position: 4
---

# Firmware Build and Flashing

The firmware uses boot/rootfs A/B slots, with 1792 MiB per rootfs slot and
OTA manifest schema version 2. Migration from the former OEM-partition layout
requires full-image flashing; there is no OTA migration from that older layout.
The current firmware has no OEM partition or `/oem` directory.

For boards already using this A/B layout, the transition to the Aico technical
names requires a complete matching boot-and-rootfs OTA, which preserves
`/userdata`. A business `.deb` alone cannot perform this transition. See
[Product Names and Technical Identifiers](../02-architecture/naming.md).

## Getting Firmware

The **Aico Link Channel Release** workflow compares changes against the selected
channel's previous release. System changes produce signed firmware; business-only
changes produce a Debian package. The primary workflow checks `main` hourly at
`:17` UTC and publishes changed content to `dev`; unchanged content is skipped.
All three channels (`dev`, `staging`, `prod`) also support manual releases;
see [Channel Releases](../08-ota/channel-release.md). Primary and backup entries
default to verified publication, using `aico-hosted-01` and `aico-hosted-02`
respectively for firmware builds. Fallback and standalone package builds upload
artifacts only. You can also build the image locally with
`./debian_build.sh`.

For this TF-only variant, provision the whole card with `tf-card.img`.
`update.img` remains a Rockchip recovery package. See
[TF-only RV1106 system](tf-system.md).

## Firmware Features

This project's firmware is built on `pico-sdk` and includes the following customizations:

- The AIC8800D80 Wi-Fi driver is enabled by default (`ENABLE_WIFIDRV=1`). The
  GD32 MCU controls `WIFI_VCC_PWREN`. SCH v1 startup preserves module power and
  retained SDIO state across RV1106 restarts, without pulsing WL_EN or rebinding
  the host. The BSP identifies the supported ROM state before provisioning, or
  validates and reuses running firmware. Partial or unknown firmware states
  fail startup instead of triggering another download. The loader requires
  `fw_state` to report `provisioned` or `reused` and holds the SDIO host active
  through BSP and Wi-Fi driver initialization before restoring its power policy.
  Bluetooth preparation checks the same shared firmware state. Bluetooth HCI
  and the BLE service are enabled by default (`ENABLE_BLUETOOTH_HCI=1`,
  `ENABLE_BLE_SERVICE=1`) and attach the controller on `/dev/ttyS0`;
- Kernel builds the Rockchip RK628 HDMI-to-CSI V4L2 driver and disables the
  unpopulated Toshiba TC358743 path;
- The Aico Link SCH v1 DTS declares RK628F on 100 kHz I2C4 at `0x50`, with
  GPIO1_B0 reset, GPIO1_B1 interrupt, and four continuous-clock CSI lanes;
- RK628F keeps its driver-provided 1080p60 EDID;
- USB-C port is configured as a composite gadget on boot: keyboard HID,
  pointer/touch HID, and CDC ECM networking (`usb0`, default `192.168.42.1`);
- Builds a Debian 13 rootfs from `overlay-debian/`, BSP modules and libraries,
  and the installed `aico-business` package. No OEM partition or `/oem` directory is created.

The related low-level changes are tracked directly in the vendored `pico-sdk/`
directory.

## Building the Full Firmware Locally

This requires an x86_64 Linux + Docker environment, or a compatible environment capable of running amd64 containers:

```bash
./debian_build.sh
```

The command requires an external Agent configuration and matching Ed25519 OTA
key pair (see `./debian_build.sh --help`). Process overview:

1. Build and audit the Debian armhf C/C++ and Go application bundle;
2. Build the RV1106 BSP, bootloader, kernel modules, and A/B boot images from `pico-sdk`;
3. Build the pinned Debian 13 rootfs, install `aico-business`, and apply `overlay-debian/`;
4. Install BSP libraries/modules and the OTA public key into rootfs before archiving it;
5. Create rootfs, userdata, and OTA images and validate their contents;
6. Generate the signed local OTA manifest and full USB first-flash package.

After the build completes, the images are located in:

```text
output/debian/image/
```

## Firmware pip

The Debian production package set includes `python3` and `python3-pip`. After
building an image, verify the runtime with:

```bash
/usr/bin/python3 -m pip --version
```

Runtime-installed packages stay under `/userdata`; see
[Persistent Python Packages](../04-agent/python-packages.md).

## System Suspend

The Debian board configuration includes `rv1106-pm.config` and the Aico Link board
DTS settings for both suspend-to-idle (`s2idle`) and suspend-to-RAM (`deep`).
The image installs `/usr/bin/aico-power`, available in the normal shell PATH.
Run either command as root:

```bash
aico-power sleep       # Coordinated s2idle suspend
aico-power deep-sleep  # Reports unsupported on this board
```

The command starts `aico-suspend.service` and waits for its result. The worker
runs independently of the invoking SSH session. It verifies both the suspend
mode and the enabled `rv1106_idle` timekeeping callback, syncs filesystems,
and selects `/sys/power/mem_sleep`. It temporarily disconnects connected or
connecting `wlan0` through wpa_supplicant, then obtains an MCU suspend grant through
the local service, then reads `/sys/power/wakeup_count` and confirms the grant
is still active before writing the count and `mem` to `/sys/power/state`.
Each control uses a single unbuffered write: a failed suspend is not retried
on file close with an expired wakeup-count check.
It sends SUSPEND_RESUME and requests Wi-Fi reconnection after wake or failure.
An originally disconnected, inactive or disabled interface stays offline;
no network credentials, interface power policy or saved network configuration
are changed. A failed supplicant command prevents suspend. Association and
address acquisition are asynchronous after the reconnect request.

Recovery intents are saved in root-owned `/run/aico-suspend/state.json` before
each external action. `ExecStopPost` retries cleanup if the worker exits or is
killed. Each invocation makes one bounded attempt. `aico-suspend-recover.timer`,
started by `aico.target`, also runs recovery 15 seconds after boot and ten seconds
after each recovery activation finishes. Failed cleanup retains pending actions
for the next activation; successful cleanup sends no further UART requests.
The grant can survive an RV reboot with MCU/UART still powered, while `/run`
cannot. Recovery therefore confirms SUSPEND_RESUME once per RV boot and records
`/run/aico-suspend/boot-recovered`; a sleep request must complete that same
recovery before PREPARE. Repeated RV_READY is not sufficient. All recovery
entry points use the worker's lock so they cannot revoke a live suspend grant.
Persistent MCU failure blocks new sleeps and leaves recovery pending.
`sudo aico-power --recover` retries them without requesting another sleep.
Inspect `journalctl -u aico-suspend.service -u aico-suspend-recover.service`
and `systemctl list-timers aico-suspend-recover.timer` for the result. Do not invoke the
internal `--execute` worker directly from an SSH session.
If kernel wakeup sources remain active for five seconds, the worker cancels
the grant, restores Wi-Fi and reports the blocker.
It waits until the suspend request returns and reports
kernel errors with a nonzero exit status. Check the available states with:

```bash
cat /sys/power/state
cat /sys/power/mem_sleep
```

Both `s2idle` and `deep` may appear in `mem_sleep`; brackets indicate the
currently selected mode. Only `s2idle` is enabled by the board command.
Deep sleep retains RAM and the RV power-hold signal; it does not power off the
RV1106. The RK628 driver powers down its enable GPIO during system suspend and
restores it on resume. ASRPRO and Wi-Fi/Bluetooth supply controls are not changed
by this command. A peripheral driver that refuses suspend causes the request
to fail; the command does not detach drivers to bypass that error.

UART5 remains a wake source for the MCU link, so periodic MCU traffic can wake
`s2idle` immediately. The MCU suspends periodic probes after granting a suspend
request. Deep resume requires a suitable PMU-bank GPIO wake source and its kernel
configuration; UART5 is not a deep-sleep wake path. The current board schematic
does not connect an MCU output to a PMU-bank RV wake pin, so `deep-sleep`
returns an error without changing the power state. A future board revision
needs a verified MCU wake output and kernel wake IRQ before enabling deep.
The temporary
J2-13/GPIO0_A6 wake key used in hardware testing is not installed by the
production DTS. Shell and network access pause while suspended.

For hardware validation, compare `/sys/kernel/debug/suspend_stats` before and
after the command and inspect the kernel log for the resume IRQ. A touch wake
on this board should report `ttyS5` and a matching MCU `touch_key` event.
SSH becoming unresponsive alone does not prove successful suspend.
SCH v1 connects USB_VBUSDET through R204/R203 to VSYS_LOAD. The OTG PHY uses
`rockchip,vbus-always-on` to describe that wiring and skip external-VBUS charger
detection, whose SDP/CDP path otherwise holds `rockchip_otg` indefinitely.
The controller stays in peripheral mode. System suspend disconnects the USB
gadget; resume re-enumerates it, so a phone connection is interrupted during
sleep. This is not USB session retention or USB remote wake support.

The RK628 resume callback restores its 1194 MHz CPLL reference after power loss;
see [RK628F hardware adaptation](../03-hardware/rk628f-adaptation.md) for the
raw-frame corruption caused by the uncalibrated resume clock. Validate an actual
fresh screenshot after wake, in addition to HDMI lock and service health.

The RV1106 CPUidle driver supplies a WFI `enter_s2idle` callback at a nonzero
state index, as required by the 5.10 CPUidle core. This lets the core freeze
the tick and timekeeping while userspace is frozen. `CLOCK_MONOTONIC` excludes
that time; `CLOCK_BOOTTIME` and wall time include it. Systemd service watchdogs
remain enabled. The old kernel had no registered idle driver, so its monotonic
clock advanced during sleep and services could expire immediately on resume.
The suspend helper refuses that old configuration. This callback only executes
WFI: it does not enable PMU deep sleep or DDR self-refresh.

For a long-duration check, arm an MCU wake timer, record the two clocks and
service PIDs/NRestarts, then sleep for longer than the service watchdog period.
After wake, confirm the sleep-time difference grew, watchdogs stayed enabled,
services did not restart, Wi-Fi returned and a fresh screenshot is intact.
Use the public command for this check so that the systemd worker and automatic
Wi-Fi recovery are exercised together. Arm the MCU timer before invoking it;
the timer includes filesystem sync and suspend preparation, so the measured
frozen interval will be shorter than the timer's requested delay.

Keep cold-module startup and retained-module reboot results separate from
suspend acceptance. On SCH v1, the maintained OFF switch leaves Wi-Fi and ASR
powered; toggling it does not establish a cold Wi-Fi baseline. Record the
kernel version, firmware state, boot ID and suspend counters when comparing
these paths. A reachable serial shell or a successful SDIO enumeration alone
does not establish that the Wi-Fi firmware can answer commands.

## Provisioning the TF System Card

Check the board's pre-BootROM TF power path before cold boot. Write the
locally built raw image to a verified whole-card device, after unmounting
its filesystems:

```bash
cd output/debian/image
sha256sum --check tf-card.img.sha256
sudo dd if=tf-card.img of=/dev/sdX bs=4M conv=fsync status=progress
```

Replace `/dev/sdX` with the actual whole-card device. See
[TF-only RV1106 system](tf-system.md) for layout, power requirements, and
the difference between this image and `update.img`.

The firmware image includes the `adb` client on the board so it can act as an
ADB host for external Android devices.  It does not expose `adbd` for
host-side `adb shell` login into the board itself. The client is built from the
nmeum/android-tools 30.0.5p1 release (AOSP platform-tools-30) and reports
version 1.0.41, so it speaks the current adb auth and pairing protocol.

## Partition Reference

The production image uses an A/B partition layout:

| Partition | Size | Purpose |
| --- | ---: | --- |
| `env` | 32 KB | Bootloader environment, factory/USB recovery only |
| `idblock` | 512 KB @ 32 KB | Rockchip idblock, factory/USB recovery only |
| `uboot` | 256 KB | Bootloader, factory/USB recovery only |
| `misc` | 4 MB | SPL A/B metadata, AVB A/B record at byte offset `2048` |
| `boot_a` | 32 MB | Slot A FIT boot image, points to `rootfs_a` |
| `boot_b` | 32 MB | Slot B FIT boot image, points to `rootfs_b` |
| `rootfs_a` | 1792 MiB | Slot A root filesystem |
| `rootfs_b` | 1792 MiB | Slot B root filesystem |
| `userdata` | 3 GB | Shared non-OTA persistent data |
| `ota` | 300 MiB | Dedicated OTA state, health markers, and download cache |

The raw `tf-card.img` contains every factory partition at its Rockchip
`blkdevparts` offset. It has no conventional MBR/GPT.

The production image uses an A/B partition layout. Online OTA only writes to the inactive slot's `boot_*` and `rootfs_*` partitions; `env`, `idblock`, and `uboot` are used only for factory or USB recovery flashing and are not updated via OTA. The `misc` partition holds the Rockchip SPL A/B metadata, which is located at byte offset `2048`.

The Debian `update.img` includes an initially empty `ota.img`. The generated factory configuration is stored in `userdata.img` at `/debian/ota/config.json` and appears at `/userdata/debian/ota/config.json` after boot. It contains `repo`, `channel`, `factory_version`, `factory_build_time`, and slot-aware `factory_partition_hashes`; `/userdata/ota` remains the dedicated workspace for state and downloads.

For more OTA details, see [OTA Overview](../08-ota/README.md).
