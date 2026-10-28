---
sidebar_position: 7
---

# TF-only RV1106 system

This variant retains the RV1106G3 and removes the board eMMC. The TF card in
J4 carries the bootloader, A/B Debian system, userdata, and OTA workspace.
The production board configuration is
`scripts/debian-system/BoardConfig-TF-Debian13-RV1106_Aico_Link-IPC.mk`.

## Boot and power prerequisites

The intended chain is RV1106 BootROM -> TF `idblock`/SPL -> U-Boot -> slot
`boot_a` or `boot_b` -> Linux -> matching `rootfs_a` or `rootfs_b`. U-Boot and
Linux disable the eMMC controller. U-Boot's TF supply GPIO0_A1 is active high,
matching the Linux device tree and the WS4622C switch; TF card detect on
GPIO3_A1 is active low. Both bootloader and kernel clear the RV1106 JTAG
override when selecting SDMMC DAT2/DAT3 for 4-bit transfers.

The vendor SPL/U-Boot `dwmci_get_cd()` explicitly negates the logical GPIO
value. Its `cd-gpios` must therefore use `GPIO_ACTIVE_HIGH` to pass the raw
electrical level to that inversion; Linux uses `GPIO_ACTIVE_LOW` normally.
Using `GPIO_ACTIVE_LOW` in this vendor bootloader inverts detection twice and
reports `MMC: no card present` (`-123`) with a card inserted. The TF regression
test compiles the actual GPIO and MMC detection functions with the board DTB
flag and checks insertion/removal for both SPL and U-Boot.

BootROM runs before SPL can configure GPIO0_A1. On SCH v1, R125 pulls this
signal low, which leaves the WS4622C TF supply off by default. A software-only
image cannot make an unpowered TF card readable to BootROM. The board design
must provide TF power before BootROM access, and the power handoff through
BootROM, SPL, U-Boot and Linux must be measured on a cold start. A board with
eMMC still installed must also select TF as the BootROM source; disabling eMMC
in the later device trees does not change BootROM selection. Cold boot and
power-handoff acceptance on the revised hardware remain required.

## Card layout and images

The A/B boot and rootfs offsets are retained on `mmcblk1`. `idblock` starts at
byte 32768; the `env` region occupies the first 32 KiB. Partition sizes are:

| Region | Size |
| --- | ---: |
| `env`, `idblock`, `uboot`, `misc` | 32 KiB, 512 KiB, 256 KiB, 4 MiB |
| `boot_a`, `boot_b` | 32 MiB each |
| `rootfs_a`, `rootfs_b` | 1792 MiB each |
| `ota` | 300 MiB |
| `userdata` | All remaining card capacity |

The layout ends in `300M(ota),-(userdata)`: OTA is partition 9 and userdata
is partition 10. Userdata starts at byte 4,144,791,552. U-Boot and Linux resolve
`-` against the physical card capacity, so a 15,931,539,456-byte card provides
about 10.98 GiB for userdata. Filesystem metadata and reserved blocks reduce
the free space reported by `df`.

The factory image reserves a finite 3 GiB userdata seed and ends at byte
7,366,017,024 (about 6.86 GiB), regardless of the eventual card size. Use a TF
card larger than this image; 16 GB high-endurance media is the practical minimum
for system writes. On first boot, `aico-rootfs-grow` expands the userdata ext4
filesystem to its partition boundary before mounting it. Subsequent boots
recheck its size, allowing a provisioned card copied to a larger card to grow
while retaining user data. Already full filesystems skip fsck and resize.
Large cards can take several minutes on first boot; the growth service allows
up to 15 minutes. The `rootfs.img` payload is placed in both factory rootfs slots.

The previous `3G(userdata),300M(ota)` layout is incompatible with this layout.
Back up data and reflash the whole card to migrate; do not install these boot
images through ordinary A/B OTA onto the previous layout. OTA does not move
existing filesystems or update the bootloader environment.
`/userdata` and `/userdata/ota` are mounted through udev's filesystem-label
links (`/dev/disk/by-label/userdata` and `/dev/disk/by-label/ota`). The vendor
`blkdevparts` parser exposes partition names in sysfs, but udev does not create
systemd device units for the compatibility links under `/dev/disk/by-partlabel`.
Mounting from those links leaves systemd waiting for a device despite the
partition nodes and ext4 filesystems being present. The slot resolver still
provides partition-label links for A/B and OTA tooling. Online OTA only updates
the inactive `boot_*` and `rootfs_*` slots; it does not replace SPL, U-Boot,
userdata, or the OTA workspace.

Run `./debian_build.sh` from the repository root. The directly writable image
is `output/debian/image/tf-card.img`, with a matching `.sha256` file. Write it
to the **whole** card, after verifying the destination device and unmounting
its filesystems, for example:

```bash
sudo dd if=output/debian/image/tf-card.img of=/dev/sdX bs=4M conv=fsync status=progress
```

`update.img` is a Rockchip recovery package, not a raw TF card image. Factory
OTA configuration is installed in userdata before the final `tf-card.img` is
assembled and audited. The image does not contain a conventional MBR/GPT;
Linux uses Rockchip `blkdevparts` and `PARTLABEL` names from the boot arguments.

After first boot, check `lsblk -b` and `df -h /userdata /userdata/ota`.
`/userdata` should mount from `mmcblk1p10`, end at the card boundary, and fill
that partition to ext4 block granularity. `/userdata/ota` should mount from
`mmcblk1p9` and remain a separate 300 MiB partition.

The system TF card is excluded from StorageManager's removable-card discovery,
including an explicit device override. Config Web cannot format or eject the
card that holds `/`. Effective storage mode `1` means system TF storage in this
variant. The existing migration tier requires a separate SD device, which the
single-slot SCH v1 board does not provide.
