---
sidebar_position: 1
---

# OTA Architecture and Runtime

OTA is accomplished through three layers: `debian_build.sh` orchestrates the Debian apps/system images and signed artifacts, the vendor SDK supplies BSP and image-packing internals, and the device-side `ota` completes download, write, and switching on manual trigger. One-shot systemd health handling commits a healthy slot after startup. The manually dispatched [channel release workflow](channel-release.md) automates classification, contracts, builds and publication.

## Partition Layout

Production images use A/B layout:

```text
32K(env),512K@32K(idblock),256K(uboot),4M(misc),32M(boot_a),32M(boot_b),1792M(rootfs_a),1792M(rootfs_b),3G(userdata),300M(ota)
```

| Partition | A/B | OTA Behavior |
| --- | --- | --- |
| `env` | No | Not updated via OTA; factory or USB recovery only |
| `idblock` | No | Not updated via OTA; avoid brick risk |
| `uboot` | No | Not updated via OTA; old bootloader requires full flash update |
| `misc` | No | Stores A/B metadata, OTA only modifies slot state |
| `boot` | Yes | Write to inactive `boot_a` or `boot_b` |
| `rootfs` | Yes | Write to inactive `rootfs_a` or `rootfs_b` |
| `userdata` | No | Preserved across upgrades, stores non-OTA persistent data |
| `ota` | No | Dedicated OTA config, state, health markers, and download cache; factory flash only |

## Boot Process

1. ROM loads SPL.
2. SPL reads Android AVB A/B metadata from `misc` partition at byte offset `2048`.
3. SPL selects the slot with highest priority and bootable status, loading `boot_a` or `boot_b`.
4. Slot-specific FIT boot image provides `root=PARTLABEL=rootfs_a|rootfs_b` and `aico.slot_suffix=_a|_b`.
5. Linux mounts the matching `rootfs_*`.
6. `aico-slot-resolve.service` resolves stable partition paths for the active slot.
7. `userdata.mount` and `userdata-ota.mount` mount persistent data and the dedicated OTA workspace. Platform files and business binaries are already in the active rootfs.
8. `aico-ota-health-marker.service` aggregates required application health and
   the Config Web recovery portal, then `aico-ota-health.service` processes
   pending OTA state once. Agent startup is optional during this confirmation.
   The OTA command controls the health-marker wait through `health_timeout_seconds`
   (five minutes by default). The health service has a ten-minute outer startup
   timeout for interruptible stalls. If increasing the health wait beyond that
   budget, also increase `TimeoutStartSec` in a systemd drop-in, leaving time for
   the metadata commit.

## Update Process

1. `ota` reads `/userdata/debian/ota/config.json` and `/usr/share/keyrings/aico-ota.pem`.
2. Fetch the configured `manifest_url`, or discover the highest OTA version for
   the configured dev/staging/prod channel from the repository's release list.
   Business-only releases are skipped. Older factory configurations retain the
   `releases/latest` fallback; custom static sources use an explicit manifest URL.
3. Download `manifest.json`, remove `signature.value`, and verify its canonical JSON Ed25519 signature. Managed channels must also match the signed channel and version prefix, including when a direct manifest URL is used.
4. Reject downgrades with older `build_time` or different version with same build time.
5. Select inactive slot and parse corresponding slot assets from manifest.
6. Clean stale download cache and calculate the remaining bytes after verified cache and resumable partials.
7. Read actual available bytes from the dedicated OTA filesystem and require the remaining downloads plus the configured safety margin.
8. Download images and verify the archive size and signed SHA256 before touching a partition.
9. Mark the inactive target slot unbootable, then stream each extracted image to its partition while calculating and checking `image_sha256`; fsync after the hash passes, and reject the update without activating the slot on any mismatch.
10. Read the written image bytes back from eMMC and verify the same image SHA256, then personalize rootfs when required.
11. Delete old `health.ok`, write `/userdata/ota/pending_boot.json`.
12. Modify `misc`, set target slot as active trial slot with default tries of 3.
13. Reboot into target slot.

Normal updates therefore decompress a compressed image once, during the write, instead of separately for pre-write image verification and archive pre-scans. The full eMMC readback remains as the independent write-integrity check. Dry-run mode still decompresses and hashes the image without writing it.

## Health Confirmation and Rollback

After the new slot boots, the Go daemon calls OTA health write logic after runtime init completes. `/userdata/ota/health.ok` is only written when all the following conditions are met:

- `pending_boot.json` exists.
- Current `aico.slot_suffix` equals pending target slot.
- Current rootfs slot equals pending target slot.
- Health marker write includes version, build time, nonce, and current boot ID from pending.

When `ota` sees a matching marker:

1. Durably save the current CRC-protected `misc` metadata to
   `/userdata/ota/ab-commit.pending`, then mark the slot successful, sync `misc`,
   and verify the metadata readback.
2. Update committed version/build time and per-slot partition hashes in `/userdata/ota/state.json`.
3. Durably delete `pending_boot.json`, then retire the metadata backup.

Health processing, early boot recovery, and manual rollback recover an interrupted
health commit before using `misc`. Valid metadata is preserved and synced;
CRC-invalid metadata is restored from the backup. If committed state was already
published, recovery finishes the pending-marker cleanup. This permits retrying a
process interrupted during the write, sync, or state publication. Recovery runs
in Linux; it does not add redundant metadata support to SPL or guarantee slot
selection after power loss before Linux can start. A systemd timeout also cannot
terminate a process stuck in uninterruptible kernel I/O.

If the health window times out, `ota health` actively reboots, allowing SPL to consume tries. When tries are exhausted and the target slot is not successful, SPL falls back to the previous successful slot. When `ota health` observes a rollback in the old slot, it cleans up pending state and marks the state phase as `rolled-back`.

## Manifest Convention

Manifest schema 2 is required; schema 1 and OEM parts are rejected. `parts[].name` in the manifest can only be `boot` or `rootfs`. Each part uses one of the following asset forms:

- `asset`: slot-neutral `{name,size,sha256}`, only applicable to byte-identical images on both sides.
- `asset_a` and `asset_b`: slot-specific `{name,size,sha256}`.

For `.img.tar.gz` assets, `size` and `sha256` describe the downloaded archive. The required `image_sha256` field describes the extracted `.img`; OTA state and `requires_partitions` compare this extracted image hash.

Production Debian OTA must include both boot and rootfs atomically. `boot` must use `asset_a` and `asset_b` because the boot image contains slot-specific DTB bootargs. `rootfs` can use slot-specific assets or slot-neutral assets when confirmed as byte-identical.

## Factory Baseline

Release `update.img` must include the factory configuration in userdata at
`/userdata/debian/ota/config.json`. The Debian build generates that configuration
from the signed manifest, installs it through the system image builder, and
repacks `update.img` before the final mounted-image audit.

`config.json` must contain at least:

- `factory_version` - factory flash version number, used for downgrade protection and selective update verification
- `factory_build_time` - factory flash build time
- `factory_partition_hashes.a.boot|rootfs` - SHA256 of each slot A partition
- `factory_partition_hashes.b.boot|rootfs` - SHA256 of each slot B partition

Optional configuration fields:

- `manifest_url` - directly specify the manifest URL (the current local/self-hosted path)
- `public_key_path` - override default public key path (default `/usr/share/keyrings/aico-ota.pem`)
- `github_token_path` - GitHub token file path (required for private repositories)
- `download_safety_margin_bytes` - free bytes retained beyond remaining downloads (default 16 MiB)

The dedicated storage identity is not configurable: production OTA always
requires `/dev/disk/by-partlabel/ota` mounted as an ext4 filesystem rooted at `/`
on `/userdata/ota`. Test code can inject synthetic mount information without
exposing a device-side configuration bypass.

The Debian build derives its target-slot download limit from the same layout contract.
The current 300 MiB partition reserves a conservative 30 MiB for ext4 metadata
and reserved blocks plus the 16 MiB runtime safety margin, producing a 254 MiB
maximum compressed download set.

Factory baseline must be slot-aware because `boot_a.img` and `boot_b.img` have different hashes. When baseline is missing, OTA initialization must fail; it should not guess current partition versions.

Note: The `config.json` generated by `generate_ota_device_config.sh` also includes `repo` and `channel` fields, but these fields are only for human readability and are not read by OTA code. Actual channel verification comes from the manifest itself.

## Private Repository Token

Public GitHub releases do not require a device token. Private repositories can place a read-only token at:

```text
/userdata/ota/gh_token
```

When a token exists, `ota` adds a bearer token to GitHub Release metadata, manifest, and image download requests. The OTA signing key and GitHub token are two independent credentials.
