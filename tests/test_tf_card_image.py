#!/usr/bin/env python3
"""Check raw TF image placement, shared A/B rootfs, and corruption rejection."""

import importlib.util
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).resolve().parents[1] / "scripts/debian-system/assemble-tf-card.py"
SPEC = importlib.util.spec_from_file_location("assemble_tf_card", SCRIPT)
ASSEMBLER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ASSEMBLER)
LAYOUT = (
    "32K(env),512K@32K(idblock),1K(uboot),1K(misc),1K(boot_a),"
    "1K(boot_b),1K(rootfs_a),1K(rootfs_b),1K(ota),-(userdata)"
)


class TFCardImageTest(unittest.TestCase):
    def test_recovery_package_keeps_remaining_userdata_seed(self):
        packer = SCRIPT.parents[2] / "pico-sdk/tools/linux/Linux_Pack_Firmware/mk-update_pack.sh"
        for remaining, with_seed in ((True, True), (True, False), (False, True)):
            with self.subTest(remaining=remaining, with_seed=with_seed), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                tools = root / "tools"
                images = root / "images"
                tools.mkdir()
                images.mkdir()
                shutil.copy2(packer, tools / packer.name)
                # Exercise the real packer's partition discovery and manifest
                # generation. Preserve that manifest instead of invoking the
                # proprietary binary encoders, which the image audit checks.
                for name, body in (
                    ("afptool", 'cp "$2/package-file" "$3"'),
                    ("rkImageMaker", 'cp "$3" "$4"'),
                ):
                    tool = tools / name
                    tool.write_text("#!/bin/sh\nset -eu\n" + body + "\n")
                    tool.chmod(0o755)
                for name in ASSEMBLER.PARTITIONS:
                    ASSEMBLER.image_for(name, images).write_bytes(b"payload")
                if not with_seed:
                    (images / "userdata.img").unlink()
                (images / "download.bin").write_bytes(b"loader")
                layout = LAYOUT if remaining else LAYOUT.replace("-(userdata)", "1K(userdata)")
                (images / "env.img").write_text("blkdevparts=mmcblk1:" + layout + "\n")
                result = subprocess.run(
                    ["bash", str(tools / packer.name), "-id", "rv1106", "-i", str(images)],
                    text=True, capture_output=True, timeout=20,
                )
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                entries = dict(line.split() for line in (images / "update.img").read_text().splitlines() if line.strip())
                if with_seed:
                    self.assertEqual(entries.get("userdata"), "userdata.img")
                else:
                    self.assertNotIn("userdata", entries)
                self.assertEqual(entries["ota"], "ota.img")
                self.assertEqual(entries["rootfs_a"], "rootfs.img")
                self.assertEqual(entries["rootfs_b"], "rootfs.img")

    def test_all_partitions_and_both_rootfs_slots(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            images = root / "images"
            images.mkdir()
            parts = ASSEMBLER.parse_layout(LAYOUT, userdata_seed_bytes=2048)
            for index, (name, _, _) in enumerate(parts):
                image = ASSEMBLER.image_for(name, images)
                image.write_bytes(bytes([index + 1]) * 512)
            env = root / "bsp-env.txt"
            env.write_text("blkdevparts=mmcblk1:" + LAYOUT + "\n")
            ASSEMBLER.validate_inputs(parts, images, env, LAYOUT)
            output = root / "tf-card.img"
            ASSEMBLER.assemble(parts, images, output)
            ASSEMBLER.verify(parts, images, output)
            self.assertEqual(output.stat().st_size, parts[-1][1] + parts[-1][2])
            with output.open("rb") as card:
                for name, start, _ in parts:
                    card.seek(start)
                    self.assertEqual(card.read(512), ASSEMBLER.image_for(name, images).read_bytes())

            with output.open("r+b") as card:
                card.seek(parts[4][1])
                card.write(b"X")
            with self.assertRaisesRegex(ValueError, "boot_a"):
                ASSEMBLER.verify(parts, images, output)

    def test_layout_and_image_size_are_rejected(self):
        with self.assertRaisesRegex(ValueError, "idblock"):
            ASSEMBLER.parse_layout(LAYOUT.replace("512K@32K", "512K@64K"))
        with self.assertRaisesRegex(ValueError, "partition names"):
            ASSEMBLER.parse_layout(LAYOUT.replace("rootfs_b", "rootfs_c"))
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            env = root / "bsp-env.txt"
            env.write_text("blkdevparts=mmcblk0:" + LAYOUT + "\n")
            with self.assertRaisesRegex(ValueError, "TF partition layout"):
                ASSEMBLER.validate_inputs(ASSEMBLER.parse_layout(LAYOUT), root, env, LAYOUT)

    def test_remaining_userdata_is_last_and_factory_image_stays_finite(self):
        board = SCRIPT.parent / "BoardConfig-TF-Debian13-RV1106_Aico_Link-IPC.mk"
        layout = next(line.split('"')[1] for line in board.read_text().splitlines()
                      if line.startswith("export RK_PARTITION_CMD_IN_ENV="))
        parts = ASSEMBLER.parse_layout(layout)
        self.assertEqual(parts[-2], ("ota", 3830218752, 300 * 1024 ** 2))
        self.assertEqual(parts[-1], ("userdata", 4144791552, 3 * 1024 ** 3))
        self.assertEqual(parts[-1][1] + parts[-1][2], 7366017024)
        # All fixed offsets fit the smallest factory footprint. Only the final
        # partition grows, so larger cards cannot move OTA or either rootfs.
        for card_bytes in (15931539456, 32 * 1024 ** 3, 128 * 1024 ** 3):
            self.assertGreater(card_bytes - parts[-1][1], parts[-1][2])

    def test_unsafe_variable_layouts_and_oversized_seed_are_rejected(self):
        for layout in (
            LAYOUT.replace("1K(ota),-(userdata)", "-(userdata),1K(ota)"),
            LAYOUT.replace("1K(ota)", "-(ota)"),
            LAYOUT.replace("-(userdata)", "3G(userdata)"),
        ):
            with self.subTest(layout=layout), self.assertRaises(ValueError):
                ASSEMBLER.parse_layout(layout)
        for size in (0, -512, 513):
            with self.subTest(size=size), self.assertRaises(ValueError):
                ASSEMBLER.parse_layout(LAYOUT, userdata_seed_bytes=size)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            parts = ASSEMBLER.parse_layout(LAYOUT, userdata_seed_bytes=1024)
            for name, _, _ in parts:
                ASSEMBLER.image_for(name, root).write_bytes(b"x" * 512)
            (root / "userdata.img").write_bytes(b"x" * 1536)
            env = root / "bsp-env.txt"
            env.write_text("blkdevparts=mmcblk1:" + LAYOUT + "\n")
            with self.assertRaisesRegex(ValueError, "userdata.img has invalid size"):
                ASSEMBLER.validate_inputs(parts, root, env, LAYOUT)


if __name__ == "__main__":
    unittest.main()
