---
sidebar_position: 3
---

# OTA Device Acceptance Process

Before enabling production OTA rollout, the following acceptance tests should be completed on representative hardware. It is recommended to record device serial number, hardware version, starting slot, target release version, and `abctl read` and `ota status` output at each step.

## Prerequisites

- The production image is built with the production Ed25519 public key.
- A local or self-hosted HTTP(S) endpoint contains the signed `manifest.json`
  and compressed image archives: `boot_a.img.tar.gz`, `boot_b.img.tar.gz`,
  `rootfs.img.tar.gz`, and `update.img.tar.gz`.
- The `update.img` inside `update.img.tar.gz` contains the dedicated empty `ota` partition and the factory baseline in userdata at `/debian/ota/config.json`.
- When UART is available, it is recommended to record SPL rollback logs simultaneously.

`aico-ota-health.service` only handles `/userdata/ota/pending_boot.json` at
startup; it does not perform network or GitHub update checks. Manual updates
must be triggered via `ota update`.

## 1. USB Factory Flash Acceptance

Download the local build's `update.img.tar.gz`, extract `update.img`, then flash it with the normal USB recovery flow:

```bash
tar -xzf update.img.tar.gz update.img
FLASH_TOOL=pico-sdk/tools/linux/Linux_Upgrade_Tool/upgrade_tool
scripts/flash.sh inspect --tool "${FLASH_TOOL}"
sudo scripts/flash.sh flash --tool "${FLASH_TOOL}" \
  --image ./update.img --sha256 "<verified-sha256>" \
  --confirm-erase-all-data
```

The repository-root `upgrade_tool/upgrade_tool` is a macOS Mach-O binary; use
the Linux SDK tool above on Linux hosts.

After device boots, check:

```bash
cat /proc/cmdline
findmnt /
ota_device="$(readlink -f /dev/disk/by-partlabel/ota)"
ota_mount_device="$(awk '$2 == "/userdata/ota" && $3 == "ext4" { print $1 }' /proc/mounts)"
userdata_mount_device="$(awk '$2 == "/userdata" { print $1 }' /proc/mounts)"
test -n "$ota_mount_device"
test "$(readlink -f "$ota_mount_device")" = "$ota_device"
test -n "$userdata_mount_device"
test "$(readlink -f "$userdata_mount_device")" != "$ota_device"
df -h /userdata /userdata/ota
/usr/lib/aico/abctl read /dev/disk/by-partlabel/misc
/usr/lib/aico/ota status
```

Expected:

- Factory boot is in slot A.
- `/proc/cmdline` contains `aico.slot_suffix=_a` and `root=PARTLABEL=rootfs_a`.
- `/` is mounted from `rootfs_a` (`/dev/mmcblk0p7`); `/oem` is absent.
- `/userdata/ota` is an ext4 mount whose source resolves to `/dev/disk/by-partlabel/ota`, and it reports an independent filesystem from `/userdata`.
- `misc` metadata can be parsed normally from byte offset `2048`, slot A is successful.
- `/userdata/debian/ota/config.json` exists, `ota status` does not report missing factory baseline.

## 2. Manual Slot Switch

Switch to inactive slot:

```bash
/usr/lib/aico/abctl set-active /dev/disk/by-partlabel/misc b --tries 3
sync
reboot
```

After reboot, check:

```bash
cat /proc/cmdline
findmnt /
/usr/lib/aico/abctl read /dev/disk/by-partlabel/misc
```

Expected:

- `/proc/cmdline` contains `aico.slot_suffix=_b` and `root=PARTLABEL=rootfs_b`.
- `/` is mounted from `rootfs_b` (`/dev/mmcblk0p8`); `/oem` is absent.
- Slot B has remaining tries before mark successful.

## 3. Mark Successful

After confirming the new slot is usable, commit:

```bash
/usr/lib/aico/abctl mark-successful /dev/disk/by-partlabel/misc b
sync
/usr/lib/aico/abctl read /dev/disk/by-partlabel/misc
```

Expected: slot B successful with tries 0; previous slot is still retained as a fallback slot.

## 4. Rollback Trial Boot

Force trial boot to another slot, but do not mark successful:

```bash
/usr/lib/aico/abctl set-active /dev/disk/by-partlabel/misc a --tries 1
sync
reboot
```

To prevent health success, mask `aico-ota-health-marker.service` for the
bounded test or prevent application readiness during trial boot. Reboot again
after tries are consumed, then remove the mask.

Expected: SPL returns to the previous successful slot. Confirm with UART, `cat /proc/cmdline`, and `abctl read`.

## 5. OTA Happy Path

Confirm configuration:

```bash
cat /userdata/debian/ota/config.json
```

Configuration should point to target repo/channel and include `boot`, `rootfs` hashes in `factory_partition_hashes.a` and `factory_partition_hashes.b`.

Execute OTA once:

```bash
/usr/lib/aico/ota update
```

Expected process:

1. Download and verify signed manifest.
2. Select inactive slot assets.
3. Download, verify, and write to inactive partitions.
4. Write `/userdata/ota/pending_boot.json`.
5. Switch `misc` and reboot.
6. After new slot boots, write matching `/userdata/ota/health.ok`.
7. `ota` marks successful and deletes pending files.

After success, check:

```bash
/usr/lib/aico/ota status
/usr/lib/aico/abctl read /dev/disk/by-partlabel/misc
ls -l /userdata/ota/pending_boot.json /userdata/ota/health.ok 2>&1 || true
```

Expected:

- `ota status` shows committed version/build time.
- Active slot successful with tries 0.
- `pending_boot.json` and `health.ok` are cleaned up.

## 6. Failure Scenarios

At least cover the following failure scenarios:

| Scenario | Expected |
| --- | --- |
| Invalid signature or invalid manifest | Reject update, do not write partitions, do not switch slot |
| Archive SHA256 or size mismatch | Reject update before writing the target partition |
| Extracted image SHA256 mismatch | Reject update during the streamed write; keep the target slot unbootable and do not switch slots |
| Downgrade release | Reject update, do not switch slot |
| Health marker missing or mismatched | Target slot not marked successful, rollback after tries consumed |
| Inactive boot image corrupted | SPL should not hang, should fall back to previous successful slot |
| Download interrupted | Do not switch slot; can retry or re-download after network recovery |
| Dedicated OTA partition unmounted | `ota status`, `ota health`, and `ota update` fail without creating files under the userdata mount point |
| OTA partition lacks remaining download capacity | Reject before requesting any partition asset; keep the inactive slot unchanged |

Power interruption during partition write should be done via controlled power supply or HIL rig; manual random power disconnection is not recommended.
