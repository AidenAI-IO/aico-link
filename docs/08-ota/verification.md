---
sidebar_position: 4
---

# A/B and `abctl` Verification

`abctl` is a diagnostic and factory test tool for OTA A/B metadata. It can operate on regular files or `/dev/disk/by-partlabel/misc` on devices.

## Metadata Convention

- A/B metadata is located at `misc` partition byte offset `2048`.
- Data structure is 32 bytes, compatible with Android AVB A/B layout version `1.0`.
- CRC32 uses big-endian IEEE, covering metadata bytes `0..27`.
- Reserved bytes remain reserved. Application OTA state is saved on the dedicated partition mounted at `/userdata/ota`, not written to `misc` reserved bytes.
- Factory `misc.img` initial state: slot A priority 15, successful; slot B disabled.

## Host Image Check

Use regular files for safe testing:

```bash
build/bin/abctl init /tmp/misc.img --size 4M
build/bin/abctl read /tmp/misc.img
```

Expected output includes slot A successful, slot B priority 0, `last_boot=A`.

Manual state switching:

```bash
build/bin/abctl set-active /tmp/misc.img b --tries 3
build/bin/abctl read /tmp/misc.img
build/bin/abctl mark-successful /tmp/misc.img b
build/bin/abctl read /tmp/misc.img
```

Explicitly write test state:

```bash
build/bin/abctl write /tmp/misc.img \
  --a-priority 14 --a-tries 0 --a-successful 1 \
  --b-priority 15 --b-tries 3 --b-successful 0
build/bin/abctl read /tmp/misc.img
```

`abctl write` will reject illegal states, such as a successful slot with non-zero tries.

## Device Check

Read real `misc` on device:

```bash
/usr/lib/aico/abctl read /dev/disk/by-partlabel/misc
```

Manually switch slot:

```bash
/usr/lib/aico/abctl set-active /dev/disk/by-partlabel/misc b --tries 3
sync
reboot
```

After boot, confirm active slot:

```bash
cat /proc/cmdline
findmnt /
/usr/lib/aico/abctl read /dev/disk/by-partlabel/misc
```

After confirming device health, commit slot:

```bash
/usr/lib/aico/abctl mark-successful /dev/disk/by-partlabel/misc b
sync
```

## Rollback Test

Set up a trial boot and do not mark successful:

```bash
/usr/lib/aico/abctl set-active /dev/disk/by-partlabel/misc b --tries 1
sync
reboot
```

Do not commit B. Continue rebooting until SPL exhausts tries and returns to the previous successful slot. Confirm with `cat /proc/cmdline`, `findmnt /`, and `abctl read`.

## Expected Diagnostic Signals

- `/proc/cmdline` contains `aico.slot_suffix=_a` or `_b`.
- `root=PARTLABEL=rootfs_a|rootfs_b` in `/proc/cmdline` matches slot suffix.
- `/` uses `rootfs_a` or `rootfs_b`, matching the slot suffix. `/oem` is absent.
- `abctl read` can parse AVB A/B metadata without CRC or layout errors.
- `ota status` can display OTA state, active slot, pending boot, and raw A/B data.
