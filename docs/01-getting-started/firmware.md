---
sidebar_position: 4
---

# Firmware Build and Flashing

This version removes the OEM partition and `/oem`, supports full-image
flashing only, and does not provide OTA migration from the old layout.
The new layout uses boot/rootfs A/B slots, with 1792 MiB per rootfs slot and
OTA manifest schema version 2.

## Getting Firmware

The **Aiden Channel Release** workflow compares changes against the selected
channel's previous release. System changes produce signed firmware; business-only
changes produce a Debian package. The primary workflow checks `main` hourly at
`:17` UTC and publishes changed content to `dev`; unchanged content is skipped.
All three channels (`dev`, `staging`, `prod`) also support manual releases;
see [Channel Releases](../08-ota/channel-release.md). Primary and backup entries
default to verified publication, using `aiden-hosted-01` and `aiden-hosted-02`
respectively for firmware builds. Fallback and standalone package builds upload
artifacts only. You can also build the image locally with
`./debian_build.sh`, or obtain a reviewed `update.img` through the project's
manual distribution process.

When flashing the full firmware, you typically use `update.img`.

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
- The Aiden SCH v1 DTS declares RK628F on 100 kHz I2C4 at `0x50`, with
  GPIO1_B0 reset, GPIO1_B1 interrupt, and four continuous-clock CSI lanes;
- RK628F keeps its driver-provided 1080p60 EDID;
- USB-C port is configured as a composite gadget on boot: keyboard HID,
  pointer/touch HID, and CDC ECM networking (`usb0`, default `192.168.42.1`);
- Builds a Debian 13 rootfs from `overlay-debian/`, BSP modules and libraries,
  and the installed `aiden-business` package. No OEM partition or `/oem` directory is created.

The related low-level changes can be found in the `pico-sdk/` submodule.

## Building the Full Firmware Locally

This requires an x86_64 Linux + Docker environment, or a compatible environment capable of running amd64 containers:

```bash
./debian_build.sh
```

The command requires an external Agent configuration and matching Ed25519 OTA
key pair (see `./debian_build.sh --help`). Process overview:

1. Build and audit the Debian armhf C/C++ and Go application bundle;
2. Build the RV1106 BSP, bootloader, kernel modules, and A/B boot images from `pico-sdk`;
3. Build the pinned Debian 13 rootfs, install `aiden-business`, and apply `overlay-debian/`;
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

## Flashing the Firmware

> You need to connect the Luckfox Pico Zero's onboard USB-C port to a computer. There are several flashing methods; for the complete instructions, refer to the [Luckfox Pico Zero official flashing guide](https://wiki.luckfox.com/zh/Luckfox-Pico-Zero/Flash-image/).

### 1. Enter Maskrom / Loader Mode

Available methods:

- Hold down the board's BOOT button while plugging in USB-C;
- If triggering flash mode with the BOOT button doesn't work well, you can first log in to the board via SSH on the USB network or the TTL serial port, then ask systemd to pass the loader argument on reboot:

```bash
systemctl reboot --reboot-argument=loader
```

The firmware image includes the `adb` client on the board so it can act as an
ADB host for external Android devices.  It does not expose `adbd` for
host-side `adb shell` login into the board itself. The client is built from the
nmeum/android-tools 30.0.5p1 release (AOSP platform-tools-30) and reports
version 1.0.41, so it speaks the current adb auth and pairing protocol.

### 2. Flash with upgrade_tool

On Linux, use the x86_64 tool generated in the repository `pico-sdk`
submodule. The repository-root `upgrade_tool/upgrade_tool` is a macOS Mach-O
binary and will not run on Linux. The guarded flash helper verifies the image
digest and requires an explicit confirmation because a full factory flash
overwrites userdata:

```bash
FLASH_TOOL=pico-sdk/tools/linux/Linux_Upgrade_Tool/upgrade_tool
IMAGE=output/debian/image/update.img
SHA256=$(awk '{print $1}' "${IMAGE}.sha256")
scripts/flash.sh inspect --tool "${FLASH_TOOL}"
sudo scripts/flash.sh flash \
  --tool "${FLASH_TOOL}" \
  --image "${IMAGE}" \
  --sha256 "${SHA256}" \
  --confirm-erase-all-data
```

For a prebuilt image, replace `IMAGE` and provide its independently verified
SHA-256. On macOS, the repository-root `upgrade_tool/upgrade_tool` command can
be used for a locally built image:

```bash
./upgrade_tool/upgrade_tool uf ./output/debian/image/update.img
```

Do not use that Mach-O binary from a Linux shell.

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

`upgrade_tool` supports updating individual partitions; a full upgrade generally uses `uf update.img`.

The production image uses an A/B partition layout. Online OTA only writes to the inactive slot's `boot_*` and `rootfs_*` partitions; `env`, `idblock`, and `uboot` are used only for factory or USB recovery flashing and are not updated via OTA. The `misc` partition holds the Rockchip SPL A/B metadata, which is located at byte offset `2048`.

The Debian `update.img` includes an initially empty `ota.img`. The generated factory configuration is stored in `userdata.img` at `/debian/ota/config.json` and appears at `/userdata/debian/ota/config.json` after boot. It contains `repo`, `channel`, `factory_version`, `factory_build_time`, and slot-aware `factory_partition_hashes`; `/userdata/ota` remains the dedicated workspace for state and downloads.

For more OTA details, see [OTA Overview](../08-ota/README.md).
