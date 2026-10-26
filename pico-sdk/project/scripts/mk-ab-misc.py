#!/usr/bin/env python3
import argparse
import pathlib
import struct
import zlib


MISC_SIZE = 4 * 1024 * 1024
AB_METADATA_OFFSET = 2048


def factory_ab_metadata():
    data = bytearray(32)
    data[0:4] = b"\x00AB0"
    data[4] = 1
    data[5] = 0
    # Factory boot has no pending OTA health marker, so slot A must start
    # successful. Slot B is populated but disabled until OTA stages a trial boot.
    data[8:12] = bytes([15, 0, 1, 0])
    data[12:16] = bytes([0, 0, 0, 0])
    data[16] = 0
    struct.pack_into(">I", data, 28, zlib.crc32(data[:28]) & 0xFFFFFFFF)
    return bytes(data)


def write_misc(path, size=MISC_SIZE):
    if size < AB_METADATA_OFFSET + 32:
        raise ValueError("misc image is too small for AB metadata")
    image = bytearray(size)
    image[AB_METADATA_OFFSET : AB_METADATA_OFFSET + 32] = factory_ab_metadata()
    pathlib.Path(path).write_bytes(image)


def main():
    parser = argparse.ArgumentParser(description="Create factory Rockchip AVB AB misc.img")
    parser.add_argument("output", help="output misc.img path")
    parser.add_argument("--size", type=int, default=MISC_SIZE, help="image size in bytes")
    args = parser.parse_args()
    write_misc(args.output, args.size)


if __name__ == "__main__":
    main()
