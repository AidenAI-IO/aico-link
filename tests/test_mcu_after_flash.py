#!/usr/bin/env python3
"""Offline checks for the post-flash runner; no UART, HTTP or poweroff calls."""
import contextlib
import importlib.util
import io
from pathlib import Path
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("postflash", Path(__file__).resolve().parents[1] / "scripts/test_mcu_after_flash.py")
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class FakeClient:
    def __init__(self, active=False):
        self.operations = []
        self.active = active

    def request(self, operation, **fields):
        self.operations.append(operation)
        return {"status": "OK", "state": {"rv_state": "ready", "uart_connected": True, "generation": "g"},
                "battery": {"percent": None}, "timer": {"active": self.active, "remaining_s": 10 if self.active else 0},
                "led": {"requested_mode": "auto"}}


class PostFlashTest(unittest.TestCase):
    def test_plain_text_shutdown_503_is_a_disconnect(self):
        response = io.BytesIO(b"Server is shutting down\n")
        response.status = 503
        with patch.object(runner.urllib.request, "urlopen", return_value=response):
            with self.assertRaises(runner.ServiceError) as caught:
                runner.Client(None, "http://192.168.42.1:8080").request("get_state")
        self.assertEqual(caught.exception.status, "SERVICE_UNAVAILABLE")
        response = io.BytesIO(b"broken response")
        response.status = 200
        with patch.object(runner.urllib.request, "urlopen", return_value=response):
            with self.assertRaisesRegex(RuntimeError, "non-JSON"):
                runner.Client(None, "http://192.168.42.1:8080").request("get_state")

    def test_default_is_read_only(self):
        client = FakeClient()
        result = runner.validate(client, runner.arguments(["--report", "/tmp/not-written.json"]))
        self.assertTrue(result["checks"])
        self.assertTrue(all(op.startswith("get_") for op in client.operations))

    def test_active_timer_is_not_replaced(self):
        client = FakeClient(active=True)
        args = runner.arguments(["--report", "/tmp/not-written.json", "--exercise-timer"])
        with self.assertRaisesRegex(RuntimeError, "already active"):
            runner.validate(client, args)
        self.assertNotIn("set_wakeup_timer", client.operations)

    def test_power_cycle_requires_external_host_and_opt_in(self):
        for extra in ([], ["--allow-power-cycle"], ["--url", "http://192.168.42.1:8080"]):
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                runner.arguments(["--report", "/tmp/not-written.json", "--power-cycle", *extra])

    def test_pagination_advances_only_returned_cursor(self):
        pages = [
            {"reset_required": False, "truncated": False, "generation": "g", "next_id": "5", "last_id": "9", "events": [{"type": "other", "id": "5"}]},
            {"reset_required": False, "truncated": False, "generation": "g", "next_id": "6", "last_id": "9", "events": [{"type": "wakeup", "source": "asr", "id": "6"}]},
        ]
        client = FakeClient()
        with patch.object(client, "request", side_effect=pages) as request:
            result = runner.wait_for_source(client, "4", "g", "asr", 1)
        self.assertEqual(result["event"]["id"], "6")
        self.assertEqual(request.call_args_list[1].kwargs["since"], "5")

    def test_running_timer_is_not_claimed_as_power_cycle(self):
        page = {"reset_required": False, "truncated": False, "generation": "g", "next_id": "1", "events": [{"type": "wakeup", "source": "rtc_timer", "id": "1"}]}
        with patch.object(FakeClient, "request", return_value=page), self.assertRaisesRegex(RuntimeError, "did not occur"):
            runner.wait_for_source(FakeClient(), "0", "g", "rtc_timer", 1, True)


if __name__ == "__main__":
    unittest.main()
