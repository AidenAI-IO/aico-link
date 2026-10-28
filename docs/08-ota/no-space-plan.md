---
sidebar_position: 5
---

# OTA Dedicated Storage Partition

## Purpose

OTA downloads, state, and health markers live on a dedicated 300 MiB ext4
partition. This replaces the former reserve-file design on `/userdata`.
Ordinary logs, recordings, swap, and Agent state cannot consume OTA capacity,
and OTA activity cannot make `/userdata` appear artificially full.

The production layout places OTA before the final userdata partition:

```text
...,1792M(rootfs_b),300M(ota),-(userdata)
```

Userdata uses all remaining TF capacity. The earlier fixed 3 GiB userdata
layout requires backup and a whole-card reflash; ordinary slot OTA cannot
move the two filesystems into this order.

The partition is mounted at `/userdata/ota`, preserving the existing OTA
runtime paths while placing them on a separate filesystem.

## Boot and Fail-Closed Behavior

The Debian board configuration defines the dedicated partition, and
systemd mounts it with `userdata-ota.mount`:

```text
300M(ota)
/dev/disk/by-partlabel/ota -> /userdata/ota (ext4)
```

`aico-slot-resolve.service` runs before `userdata.mount`,
`userdata-ota.mount`. `aico-ota-health.service` requires the
mounted userdata and OTA workspace before processing health.

The Go updater independently checks `/proc/self/mountinfo` before creating its
lock, state, or download files. The mount must be the ext4 filesystem rooted at
`/dev/disk/by-partlabel/ota`; a bind mount or unrelated filesystem at the same path
is rejected. If the expected mount is not active, `ota update`, `ota health`,
and `ota status` fail instead of writing into the underlying userdata directory.

There is intentionally no legacy reserve-file fallback and no removable-card OTA
cache route. The product has not shipped with the previous layout, so factory
images can move directly to the partition design.

## Capacity Model

The runtime decision is based on actual filesystem availability:

```text
required = remaining signed asset bytes + download safety margin
```

Before downloading partition assets, the updater:

1. Selects only the assets needed for the inactive slot.
2. Skips target partitions whose recorded hash already matches.
3. Removes stale cache entries while preserving verified assets and valid
   resumable `.part` files.
4. Subtracts bytes already present in valid partial downloads.
5. Reads `statfs(...).Bavail` from the dedicated OTA filesystem.
6. Rejects the update before the first asset request when available bytes are
   below the requirement.

The default `download_safety_margin_bytes` is 16 MiB. It covers filesystem
metadata, directory updates, and sync overhead; it is not a reserved file and
does not reduce capacity while the device is idle.

The Debian build also rejects an impossible target-slot download set. Its limit is
derived from the shared partition layout:

```text
300 MiB partition - 30 MiB ext4 unavailable-space allowance - 16 MiB runtime margin
= 254 MiB maximum compressed target-slot download set
```

The 30 MiB allowance conservatively budgets 10% of the partition for ext4
metadata and reserved blocks, based on the validated 256 MiB image exposing
about 232.4 MiB through `statfs`. The runtime `statfs` check remains
authoritative because filesystem metadata, cached assets, and partial downloads
vary by device.

## Factory Image Flow

The firmware build generates an empty generic `ota.img` and includes it in
both the raw `tf-card.img` and Rockchip `update.img`. Runtime state and downloads populate that filesystem only after
the device boots.

After the signed manifest is generated, the Debian system config action writes
the factory baseline into `userdata.img` at:

```text
/debian/ota/config.json
```

It then repacks `update.img`, rebuilds `tf-card.img`, and audits the mounted userdata image against the
signed manifest. The OTA partition itself is not an online OTA target; online
updates continue to write only the inactive `boot` and `rootfs`
partitions.

## Existing Protections That Remain

- Stale download-cache cleanup.
- Resumable `.part` downloads for network interruption.
- `.part` removal after ENOSPC, because retrying the same full filesystem
  position cannot succeed.
- Downloaded archive size and SHA256 verification.
- Extracted image hash verification for compressed images.
- Extracted image size checks against the target partition.
- A single OTA update lock for concurrent OTA commands.
- Existing StorageMonitor cleanup and user-facing storage notifications for
  `/userdata`.

No reserve allocation, release, restoration, SD routing, or StorageManager
migration coordination remains.

## StorageMonitor and SD Migration

StorageMonitor samples `/userdata`. Because `/userdata/ota` is a separate
mounted filesystem, OTA downloads do not reduce the `statfs` availability
reported for `/userdata`. Its existing cleanup stages and notifications still
protect Agent logs, audio archives, and session archives and should remain.

The StorageManager continues to mount separate cards and migrate eligible audio
data based on the existing `/userdata` watermarks. It neither hosts OTA downloads nor
shares capacity or locks with OTA, so SD insertion, removal, formatting, and
migration do not change OTA capacity.

## Diagnostics

```bash
mount | grep ' /userdata/ota '
df -h /userdata /userdata/ota
du -h /userdata/ota/downloads/* /userdata/ota/downloads/*.part 2>/dev/null
/usr/lib/aico/ota status
tail -n 100 /var/log/ota/ota.log
```

Expected mount source:

```text
/dev/disk/by-partlabel/ota on /userdata/ota type ext4
```

## Acceptance Criteria

- Full image builds produce `ota.img` and package it in `update.img`.
- `/userdata/debian/ota/config.json` comes from the audited factory
  `userdata.img`; `ota.img` starts empty.
- OTA commands fail closed when the dedicated mount is missing.
- Insufficient capacity is rejected before any partition asset request.
- Valid partial downloads reduce the remaining required bytes.
- Network failures retain resumable partial files; ENOSPC removes them.
- StorageMonitor and SD migration behavior remain independent of OTA.
