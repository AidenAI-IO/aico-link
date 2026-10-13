#!/usr/bin/env python3
"""Focused MCU V1 event logger protocol checks."""

import contextlib
import io
from pathlib import Path
import runpy
import unittest


SERVICE = runpy.run_path(str(Path(__file__).resolve().parents[1] /
                             "overlay-debian/usr/lib/aiden/aiden-mcu-event"))
Parser = SERVICE["Parser"]
encode = SERVICE["encode"]
handle = SERVICE["handle"]
REQ, RESP, EVENT = SERVICE["REQ"], SERVICE["RESP"], SERVICE["EVENT"]


class McuEventServiceTest(unittest.TestCase):
    def test_fragmented_event_and_unknown_command_are_logged(self):
        parser = Parser()
        first = encode(EVENT, 7, 0x12)
        second = encode(EVENT, 8, 0x77, b"\x00\xaa\x55")
        self.assertEqual(parser.feed(first[:4], 1.0), [])
        frames = parser.feed(first[4:] + second, 1.01)
        self.assertEqual(frames, [(EVENT, 7, 0x12, b""),
                                  (EVENT, 8, 0x77, b"\x00\xaa\x55")])
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            for frame in frames:
                self.assertIsNone(handle(frame))
        self.assertIn("name=WAKEUP seq=7 payload=", output.getvalue())
        self.assertIn("name=UNKNOWN seq=8 payload=00aa55", output.getvalue())

    def test_corruption_and_partial_timeout_do_not_create_events(self):
        parser = Parser()
        bad = bytearray(encode(EVENT, 1, 0x11, b"\x01\0\0\0"))
        bad[-1] ^= 1
        self.assertEqual(parser.feed(bad, 1.0), [])
        self.assertEqual(parser.feed(b"\xaa\x55\x01", 1.1), [])
        self.assertEqual(parser.feed(encode(EVENT, 2, 0x12), 1.3),
                         [(EVENT, 2, 0x12, b"")])

    def test_get_state_reports_ready_and_shutdown_is_denied(self):
        reply = handle((REQ, 0x2a, 0x01, b""))
        self.assertEqual(Parser().feed(reply, 1.0),
                         [(RESP, 0x2a, 0x01, b"\x00\x01\x00\x01\x00\x02")])
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            denial = handle((REQ, 3, 0x20, b"\x01\0\0\0"))
        self.assertEqual(Parser().feed(denial, 1.0),
                         [(RESP, 3, 0x20, b"\x04")])
        self.assertIn("SHUTDOWN_REQUEST", output.getvalue())
        self.assertEqual(Parser().feed(handle((REQ, 4, 0x03, b"")), 1.0),
                         [(RESP, 4, 0x03, b"\x02")])


if __name__ == "__main__":
    unittest.main()
