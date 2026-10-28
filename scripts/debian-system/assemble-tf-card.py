#!/usr/bin/env python3
"""Assemble or verify a raw RV1106 TF image from the audited partition images."""

import argparse
import hashlib
import os
import re
import shutil
import tempfile
from pathlib import Path


PARTITIONS = (
    "env", "idblock", "uboot", "misc", "boot_a", "boot_b",
    "rootfs_a", "rootfs_b", "ota", "userdata",
)
DEFINITION = re.compile(r"(\d+[KMG]|-)(?:@(\d+)([KMG]))?\(([a-z_]+)\)")
UNIT = {"K": 1024, "M": 1024 ** 2, "G": 1024 ** 3}
# Reserve a finite factory footprint, independent of the target card size.
# Bootloader/Linux resolve the final '-' against the real card capacity;
# first boot expands the ext4 seed to that partition's full size.
USERDATA_SEED_BYTES = 3 * 1024 ** 3


def parse_layout(layout, userdata_seed_bytes=USERDATA_SEED_BYTES):
    if userdata_seed_bytes <= 0 or userdata_seed_bytes % 512:
        raise ValueError("invalid userdata seed size")
    parts = []
    end = 0
    definitions = layout.split(",")
    for index, definition in enumerate(definitions):
        match = DEFINITION.fullmatch(definition)
        if match is None:
            raise ValueError("invalid partition definition: " + definition)
        size_spec, offset, offset_unit, name = match.groups()
        if size_spec == "-":
            if name != "userdata" or index != len(definitions) - 1:
                raise ValueError("only final userdata may use remaining capacity")
            size = userdata_seed_bytes
        else:
            if name == "userdata":
                raise ValueError("userdata must use remaining capacity")
            size = int(size_spec[:-1]) * UNIT[size_spec[-1]]
        start = int(offset) * UNIT[offset_unit] if offset else end
        if size <= 0 or start < end or start % 512 or size % 512:
            raise ValueError("overlapping or unaligned partition: " + definition)
        parts.append((name, start, size))
        end = start + size
    if tuple(name for name, _, _ in parts) != PARTITIONS:
        raise ValueError("TF partition names or order do not match the A/B layout")
    if parts[0][1] != 0 or parts[1][1] != 32 * 1024:
        raise ValueError("TF BootROM requires idblock at byte offset 32768")
    return parts


def image_for(name, image_dir):
    if name.startswith("rootfs_"):
        name = "rootfs"
    return image_dir / (name + ".img")


def validate_inputs(parts, image_dir, env_file, layout):
    if env_file.read_text().splitlines().count("blkdevparts=mmcblk1:" + layout) != 1:
        raise ValueError("BSP environment does not match the TF partition layout")
    for name, _, size in parts:
        image = image_for(name, image_dir)
        image_size = image.stat().st_size
        if image_size == 0 or image_size > size:
            raise ValueError("%s has invalid size %d for %s" % (image, image_size, name))


def assemble(parts, image_dir, output):
    output.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=".tf-card.", dir=output.parent)
    try:
        with os.fdopen(fd, "wb") as target:
            target.truncate(parts[-1][1] + parts[-1][2])
            for name, start, _ in parts:
                target.seek(start)
                with image_for(name, image_dir).open("rb") as source:
                    shutil.copyfileobj(source, target, 1024 * 1024)
            target.flush()
            os.fsync(target.fileno())
        os.replace(temporary, output)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def verify(parts, image_dir, output):
    if output.stat().st_size != parts[-1][1] + parts[-1][2]:
        raise ValueError("TF image size does not match the partition layout")
    with output.open("rb") as card:
        for name, start, _ in parts:
            digest = hashlib.sha256()
            image = image_for(name, image_dir)
            card.seek(start)
            with image.open("rb") as source:
                while block := source.read(1024 * 1024):
                    if card.read(len(block)) != block:
                        raise ValueError("TF image does not match " + str(image))
                    digest.update(block)
            print("%s %s %d" % (name, digest.hexdigest(), start))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--layout", required=True)
    parser.add_argument("--env", type=Path, required=True)
    parser.add_argument("--image-dir", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--verify", action="store_true")
    args = parser.parse_args()
    parts = parse_layout(args.layout)
    validate_inputs(parts, args.image_dir, args.env, args.layout)
    if args.verify:
        verify(parts, args.image_dir, args.output)
    else:
        assemble(parts, args.image_dir, args.output)
        verify(parts, args.image_dir, args.output)


if __name__ == "__main__":
    main()
