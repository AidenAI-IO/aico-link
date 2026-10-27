---
sidebar_label: Overview
sidebar_position: 0
---

# OTA Overview

This project's production OTA uses A/B partitioning, signed manifests, and boot health confirmation mechanisms. During runtime, devices only write to the inactive slot. After reboot, Rockchip SPL selects the new slot; if the new system does not confirm success within the health window, SPL automatically rolls back to the previous successful slot.

This version removes the OEM partition and `/oem`, supports full-image
flashing only, and does not provide OTA migration from the old layout.
The new layout uses boot/rootfs A/B slots, with 1792 MiB per rootfs slot and
OTA manifest schema version 2.

## Scope

- Target hardware: Luckfox Pico Zero / RV1106 + eMMC.
- Distribution: manually triggered dev/staging/prod GitHub Releases, local build
  artifacts, or a manually hosted HTTP(S) directory. Channel releases select a
  business package or complete signed OTA from source changes; see
  [Channel Releases](channel-release.md).
- Update method: The device-side `/usr/lib/aico/ota` fetches the manifest, verifies signatures, validates SHA256, writes to the inactive slot, switches `misc`, and reboots.
- Rollback method: Rockchip SPL A/B metadata controls boot tries; mark successful only after application health confirmation.

## Documentation Index

- [OTA Architecture and Runtime](architecture.md)
- [Health Checks, Data Compatibility, and A/B Rollback](health-check-rollback-design.md)
- [OTA Key Management](key-management.md)
- [Device Acceptance Process](device-acceptance.md)
- [A/B and `abctl` Verification](verification.md)
- [OTA Dedicated Storage Partition](no-space-plan.md)
- [GitHub Proxy Configuration](ota-github-proxy.md)
- [External Developer Guide](ota-external-developers.md)
- [Distribution Quick Examples](ota-quick-examples.md)
- [Release Channel Strategy](ota-release-channels.md)
- [Manual dev / staging / prod Releases](channel-release.md)
- [Debian Business Packages](debian-package.md)

## Core Constraints

- OTA does not update `env`, `idblock`, or `uboot`; these are only updated via factory or USB recovery.
- OTA only writes to `boot_*` and `rootfs_*` of the inactive slot.
- A dedicated 300 MiB `ota` partition is mounted at `/userdata/ota` and stores OTA state, download cache, and health markers. The factory configuration remains in the persistent userdata filesystem at `/userdata/debian/ota/config.json`.
- `boot_a.img` and `boot_b.img` contain different slot bootargs; manifests must use slot-specific boot assets.
- When factory baseline is missing or manifest signature/hash verification fails, devices must fail closed.
- OTA commands fail closed unless `/userdata/ota` is the ext4 mount rooted at `/dev/disk/by-partlabel/ota`, and require actual free bytes for remaining downloads plus a 16 MiB margin. For the current 300 MiB partition, `debian_build.sh` additionally caps a target-slot download set at 254 MiB.

## Common Commands

```bash
# View OTA status
/usr/lib/aico/ota status

# Check and perform OTA update immediately
/usr/lib/aico/ota update

# View A/B metadata
/usr/lib/aico/abctl read /dev/disk/by-partlabel/misc

# View current slot and rootfs
cat /proc/cmdline
findmnt /
```

`check-now` is still retained as a compatibility alias; new scripts and documentation should use `update`.

`ota check` only checks signed release metadata and prints `available`, `version`,
`build_time` and `current_version` as JSON. It does not install firmware. Config
Web uses this check before triggering firmware installation, followed by scoped
`aico-business` APT updates. See [APT updates](apt-repository.md).

## Related Source Code

| Path | Description |
| --- | --- |
| `src/agent/cmd/ota` | OTA CLI entry point, including manual update and health handling |
| `src/agent/cmd/abctl` | A/B metadata diagnostic tool |
| `src/agent/internal/ota` | OTA core logic for manifest, download, state machine, slot, health, etc. |
| `overlay-debian/etc/systemd/system/aico-slot-resolve.service` | Resolve the active A/B slot before local mounts |
| `overlay-debian/etc/systemd/system/aico-ota-health.service` | Process pending OTA health state at boot |
| `scripts/generate_ota_manifest.sh` | Generate signed OTA manifest |
| `scripts/generate_ota_device_config.sh` | Generate factory configuration from manifest |
| `scripts/ota_partition_layout.sh` | Reads the Debian system OTA partition size and derives release capacity |
| `scripts/debian-system/container-install-ota-config.sh` | Install factory OTA configuration and repack `update.img` |
| `pico-sdk/project/scripts/mk-ab-misc.py` | Generate factory `misc.img` A/B metadata |
