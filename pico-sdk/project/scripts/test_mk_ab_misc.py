#!/usr/bin/env python3
import importlib.util
import pathlib
import tempfile
import unittest
import zlib


SCRIPT = pathlib.Path(__file__).with_name("mk-ab-misc.py")


def load_module():
    spec = importlib.util.spec_from_file_location("mk_ab_misc", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class MkABMiscTest(unittest.TestCase):
    def test_writes_factory_ab_metadata_at_offset_2048(self):
        module = load_module()
        with tempfile.NamedTemporaryFile() as tmp:
            module.write_misc(tmp.name)
            data = pathlib.Path(tmp.name).read_bytes()

        self.assertEqual(len(data), 4 * 1024 * 1024)
        self.assertEqual(data[:2048], b"\x00" * 2048)
        meta = data[2048:2080]
        self.assertEqual(meta[:4], b"\x00AB0")
        self.assertEqual(meta[4], 1)
        self.assertEqual(meta[5], 0)
        self.assertEqual(meta[6:8], b"\x00" * 2)
        self.assertEqual(meta[8:12], bytes([15, 0, 1, 0]))
        self.assertEqual(meta[12:16], bytes([0, 0, 0, 0]))
        self.assertEqual(meta[16], 0)
        self.assertEqual(meta[17:28], b"\x00" * 11)
        self.assertEqual(int.from_bytes(meta[28:32], "big"), zlib.crc32(meta[:28]) & 0xFFFFFFFF)
        self.assertEqual(data[2080:], b"\x00" * (len(data) - 2080))


if __name__ == "__main__":
    unittest.main()
