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
    def test_design_frames_and_replies(self):
        # Literal wire frames pin command IDs, payloads and CRC across implementations.
        vectors = [
            ("RV_READY", EVENT, b"\1\0\0\0", "aa5501032a060400010000001c98"),
            ("WAKEUP", EVENT, b"\6\0\0\0", "aa5501032a070400060000005071"),
            ("RK628_PLUGIN_DET_CHANGED", EVENT, b"\1\0\0\0", "aa5501032a0804000100000094aa"),
        ]
        output = io.StringIO()
        for command, kind, payload, hex_wire in vectors:
            with self.subTest(command=command):
                wire = bytes.fromhex(hex_wire)
                frame = (kind, 42, SERVICE[command], payload)
                self.assertEqual(encode(*frame), wire)
                self.assertEqual(Parser().feed(wire, 0), [frame])
                with contextlib.redirect_stdout(output):
                    self.assertIsNone(handle(frame))
        self.assertIn("cmd=0x07 name=WAKEUP", output.getvalue())
        self.assertIn("wake_source=0x06 wake_source_name=ASR", output.getvalue())
        self.assertIn("cmd=0x08 name=RK628_PLUGIN_DET_CHANGED", output.getvalue())
        for request, response in (
                ("aa5501012a0100003489", "aa5501022a010600000100010002385c"),
                ("aa5501012a0904000100000092d4", "aa5501022a090100043608")):
            with contextlib.redirect_stdout(output):
                reply = handle(Parser().feed(bytes.fromhex(request), 0)[0])
            self.assertEqual(reply, bytes.fromhex(response))

    def test_fragmented_event_and_unknown_command_are_logged(self):
        parser = Parser()
        first = encode(EVENT, 7, 0x07, b"\x06\0\0\0")
        second = encode(EVENT, 8, 0x77, b"\x00\xaa\x55")
        self.assertEqual(parser.feed(first[:4], 1.0), [])
        frames = parser.feed(first[4:] + second, 1.01)
        self.assertEqual(frames, [(EVENT, 7, 0x07, b"\x06\0\0\0"),
                                  (EVENT, 8, 0x77, b"\x00\xaa\x55")])
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            for frame in frames:
                self.assertIsNone(handle(frame))
        self.assertIn("name=WAKEUP seq=7 payload=06000000", output.getvalue())
        self.assertIn("wake_source=0x06 wake_source_name=ASR", output.getvalue())
        self.assertIn("name=UNKNOWN seq=8 payload=00aa55", output.getvalue())

    def test_corruption_and_partial_timeout_do_not_create_events(self):
        parser = Parser()
        bad = bytearray(encode(EVENT, 1, 0x08, b"\x01\0\0\0"))
        bad[-1] ^= 1
        self.assertEqual(parser.feed(bad, 1.0), [])
        self.assertEqual(parser.feed(b"\xaa\x55\x01", 1.1), [])
        self.assertEqual(parser.feed(encode(EVENT, 2, 0x07, b"\x04\0\0\0"), 1.3),
                         [(EVENT, 2, 0x07, b"\x04\0\0\0")])

    def test_wakeup_sources_are_self_contained_and_never_queried(self):
        for source, name in ((1, "EXTERNAL_POWER"), (2, "POWER_SWITCH"),
                             (3, "TOUCH_KEY"), (4, "RTC_TIMER"), (5, "OTHER"),
                             (6, "ASR"), (0x80, "UNKNOWN")):
            with self.subTest(source=source):
                output = io.StringIO()
                with contextlib.redirect_stdout(output):
                    self.assertIsNone(handle((EVENT, 9, 0x07, bytes([source, 0, 0, 0]))))
                self.assertIn("wake_source=0x%02x wake_source_name=%s" % (source, name),
                              output.getvalue())

    def test_malformed_wakes_remain_visible_without_a_false_source(self):
        for payload in (b"", b"\x06", b"\x06\0\0\0\0",
                        b"\x06\x01\0\0", b"\x06\0\x01\0", b"\x06\0\0\x01"):
            with self.subTest(payload=payload):
                output = io.StringIO()
                with contextlib.redirect_stdout(output):
                    self.assertIsNone(handle((EVENT, 10, 0x07, payload)))
                self.assertIn("payload=" + payload.hex(), output.getvalue())
                self.assertIn("invalid_payload=", output.getvalue())
                self.assertNotIn("wake_source=", output.getvalue())

    def test_get_state_reports_ready_and_shutdown_is_denied(self):
        reply = handle((REQ, 0x2a, 0x01, b""))
        self.assertEqual(Parser().feed(reply, 1.0),
                         [(RESP, 0x2a, 0x01, b"\x00\x01\x00\x01\x00\x02")])
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            denial = handle((REQ, 3, 0x09, b"\x01\0\0\0"))
        self.assertEqual(Parser().feed(denial, 1.0),
                         [(RESP, 3, 0x09, b"\x04")])
        self.assertIn("SHUTDOWN_REQUEST", output.getvalue())
        self.assertEqual(Parser().feed(handle((REQ, 4, 0x7f, b"")), 1.0),
                         [(RESP, 4, 0x7f, b"\x02")])

    def test_invalid_request_payloads(self):
        for command, payload in ((0x01, b"\0"), (0x09, b""),
                                 (0x09, b"\1"), (0x09, b"\2\0\0\0"),
                                 (0x09, b"\1\1\0\0"), (0x09, b"\1\0\1\0"),
                                 (0x09, b"\1\0\0\1"), (0x09, b"\1\0\0\0\0")):
            with self.subTest(command=command, payload=payload):
                reply = handle((REQ, 42, command, payload))
                self.assertEqual(Parser().feed(reply, 0), [(RESP, 42, command, b"\2")])


if __name__ == "__main__":
    unittest.main()
