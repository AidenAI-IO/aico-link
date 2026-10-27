#!/usr/bin/env python3
"""Exercise the installed suspend command without accessing host power controls."""

import contextlib
import errno
import fcntl
import importlib.machinery
import importlib.util
import io
import os
import json
import subprocess
from pathlib import Path
import tempfile
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[1]
COMMAND = ROOT / "overlay-debian/usr/bin/aico-power"
LOADER = importlib.machinery.SourceFileLoader("aico_power", str(COMMAND))
SPEC = importlib.util.spec_from_loader(LOADER.name, LOADER)
CLI = importlib.util.module_from_spec(SPEC)
LOADER.exec_module(CLI)


class AicoPowerTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.power = self.root / "power"
        self.power.mkdir()
        self.state = self.power / "state"
        self.mem_sleep = self.power / "mem_sleep"
        self.wakeup_count = self.power / "wakeup_count"
        self.state.write_text("freeze mem disk\n")
        self.mem_sleep.write_text("[s2idle] deep\n")
        self.wakeup_count.write_text("42\n")
        self.patch("POWER_DIR", self.power)
        self.patch("LOCK_PATH", self.root / "aico-power.lock")
        self.patch("RECOVERY_STATE", self.root / "recovery/state.json")
        self.patch("WIFI_INTERFACE", self.root / "wlan0")
        idle_driver = self.root / "idle_driver"
        idle_driver.write_text("rv1106_idle\n")
        idle_disabled = self.root / "idle_disabled"
        idle_disabled.write_text("0\n")
        self.patch("CPU_IDLE_DRIVER", idle_driver)
        self.patch("CPU_IDLE_DISABLED", idle_disabled)
        self.wifi = self.patch("wifi")
        self.wifi.return_value = "DISCONNECTED"
        self.patch("os.geteuid", return_value=0)
        self.sync = self.patch("os.sync")
        self.mcu = self.patch("mcu_request")
        self.mcu.return_value = {"state": {"rv_state": "sleeping"}}

    def patch(self, name, *args, **kwargs):
        owner, attribute = CLI, name
        if "." in name:
            module, attribute = name.split(".")
            owner = getattr(CLI, module)
        patcher = mock.patch.object(owner, attribute, *args, **kwargs)
        result = patcher.start()
        self.addCleanup(patcher.stop)
        return result

    def run_command(self, argument):
        errors = io.StringIO()
        with contextlib.redirect_stderr(errors), contextlib.redirect_stdout(io.StringIO()):
            result = CLI.main([argument, "--execute"])
        return result, errors.getvalue()

    def assert_untouched(self):
        self.assertEqual(self.state.read_text(), "freeze mem disk\n")
        self.assertEqual(self.mem_sleep.read_text(), "[s2idle] deep\n")
        self.sync.assert_not_called()

    def test_installed_command_is_executable(self):
        self.assertTrue(COMMAND.stat().st_mode & 0o111)

    def test_sleep_selects_s2idle_before_requesting_mem(self):
        events = []
        write_power = CLI.write_power

        def record_write(path, value, *args, **kwargs):
            events.append((path.name, value))
            return write_power(path, value, *args, **kwargs)

        with mock.patch.object(CLI, "write_power", record_write), \
                mock.patch.object(CLI.os, "sync", side_effect=lambda: events.append("sync")):
            result, errors = self.run_command("sleep")
        self.assertEqual(result, 0, errors)
        self.assertEqual(self.mem_sleep.read_text(), "s2idle\n")
        self.assertEqual(self.state.read_text(), "mem\n")
        self.assertLess(events.index(("mem_sleep", "s2idle\n")),
                        events.index(("state", "mem\n")))
        self.assertLess(events.index("sync"), events.index(("state", "mem\n")))
        self.assertEqual(self.mcu.call_args_list,
                         [mock.call("suspend_prepare"), mock.call("get_state"),
                          mock.call("suspend_resume")])

    def test_deep_sleep_is_disabled_without_hardware_wake_line(self):
        result, errors = self.run_command("deep-sleep")
        self.assertEqual(result, 1)
        self.assertIn("PMU wake line", errors)
        self.assert_untouched()
        self.mcu.assert_not_called()

    def test_s2idle_currently_selected_in_brackets_is_supported(self):
        self.mem_sleep.write_text("[s2idle] deep\n")
        result, errors = self.run_command("sleep")
        self.assertEqual(result, 0, errors)
        self.assertEqual(self.state.read_text(), "mem\n")

    def test_grant_denied_does_not_suspend_and_releases_mcu(self):
        def reject(op):
            if op == "suspend_prepare":
                raise RuntimeError("BUSY")
        self.mcu.side_effect = reject
        result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertIn("BUSY", errors)
        self.assertEqual(self.state.read_text(), "freeze mem disk\n")
        self.assertEqual(self.mcu.call_args_list,
                         [mock.call("suspend_prepare"), mock.call("suspend_resume")])

    def test_lost_prepare_response_releases_unknown_grant(self):
        grant = [False]

        def request(op):
            if op == "suspend_prepare":
                self.assertTrue(json.loads(CLI.RECOVERY_STATE.read_text())["mcu"])
                grant[0] = True
                raise RuntimeError("PREPARE response timed out")
            self.assertEqual(op, "suspend_resume")
            grant[0] = False

        self.mcu.side_effect = request
        result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertIn("PREPARE response timed out", errors)
        self.assertEqual(self.state.read_text(), "freeze mem disk\n")
        self.assertFalse(grant[0])
        self.assertFalse(CLI.RECOVERY_STATE.exists())
        self.assertEqual(self.mcu.call_args_list,
                         [mock.call("suspend_prepare"), mock.call("suspend_resume")])

    def test_prepare_timeout_retains_cleanup_until_resume_succeeds(self):
        self.mcu.side_effect = [RuntimeError("PREPARE response timed out"),
                               RuntimeError("UART unavailable")]
        result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertIn("UART unavailable", errors)
        self.assertTrue(json.loads(CLI.RECOVERY_STATE.read_text())["mcu"])
        self.assertEqual(self.state.read_text(), "freeze mem disk\n")
        self.mcu.side_effect = None
        self.mcu.reset_mock()
        self.assertEqual(CLI.main(["--recover"]), 0)
        self.mcu.assert_called_once_with("suspend_resume")
        self.assertFalse(CLI.RECOVERY_STATE.exists())

    def test_wakeup_count_failure_rolls_back_mcu_grant(self):
        original = CLI.write_power
        def fail_count(path, value, *args, **kwargs):
            if path == self.wakeup_count:
                raise OSError("wake raced with suspend")
            return original(path, value, *args, **kwargs)
        with mock.patch.object(CLI, "write_power", fail_count):
            result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertIn("wake raced", errors)
        self.assertEqual(self.state.read_text(), "freeze mem disk\n")
        self.assertEqual(self.mcu.call_args_list[-1], mock.call("suspend_resume"))

    def test_wake_between_grant_and_kernel_suspend_is_rejected(self):
        def reply(op):
            return {"state": {"rv_state": "ready"}} if op == "get_state" else {}
        self.mcu.side_effect = reply
        result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertIn("wake or shutdown", errors)
        self.assertEqual(self.state.read_text(), "freeze mem disk\n")
        self.assertEqual(self.mcu.call_args_list[-1], mock.call("suspend_resume"))

    def test_blocked_wakeup_count_times_out_and_releases_mcu(self):
        self.wakeup_count.unlink()
        os.mkfifo(self.wakeup_count)
        with mock.patch.object(CLI, "WAKEUP_COUNT_TIMEOUT", 0.05):
            result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertIn("wakeup sources remain active", errors)
        self.assertEqual(self.state.read_text(), "freeze mem disk\n")
        self.assertEqual(self.mcu.call_args_list[-1], mock.call("suspend_resume"))

    def test_non_root_fails_before_mutation(self):
        with mock.patch.object(CLI.os, "geteuid", return_value=1000):
            result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertTrue(errors.strip())
        self.assert_untouched()

    def test_unsupported_mode_does_not_request_suspend(self):
        self.mem_sleep.write_text("[deep]\n")
        result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertIn("s2idle", errors)
        self.assertEqual(self.mem_sleep.read_text(), "[deep]\n")
        self.assertEqual(self.state.read_text(), "freeze mem disk\n")

    def test_mode_requires_exact_token(self):
        self.mem_sleep.write_text("[deep] nos2idle\n")
        result, _ = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertEqual(self.state.read_text(), "freeze mem disk\n")

    def test_mem_state_requires_exact_token(self):
        self.state.write_text("freeze nomem disk\n")
        result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertTrue(errors.strip())
        self.assertEqual(self.mem_sleep.read_text(), "[s2idle] deep\n")
        self.assertEqual(self.state.read_text(), "freeze nomem disk\n")

    def test_missing_power_controls_report_failure(self):
        self.mem_sleep.unlink()
        result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertTrue(errors.strip())
        self.assertEqual(self.state.read_text(), "freeze mem disk\n")

    def test_lock_contention_does_not_change_mode_or_suspend(self):
        with CLI.LOCK_PATH.open("a") as held:
            fcntl.flock(held, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertTrue(errors.strip())
        self.assert_untouched()

    def test_mode_selection_error_does_not_request_suspend(self):
        write_power = CLI.write_power

        def fail_selection(path, value, *args, **kwargs):
            if path == self.mem_sleep:
                raise OSError("mode selection failed")
            return write_power(path, value, *args, **kwargs)

        with mock.patch.object(CLI, "write_power", fail_selection):
            result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertIn("mode selection failed", errors)
        self.assertEqual(self.state.read_text(), "freeze mem disk\n")

    def test_kernel_suspend_error_is_nonzero_and_releases_lock(self):
        write_power = CLI.write_power

        def fail_suspend(path, value, *args, **kwargs):
            if path == self.state:
                raise OSError("suspend failed: Operation not supported")
            return write_power(path, value, *args, **kwargs)

        with mock.patch.object(CLI, "write_power", fail_suspend):
            result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertIn("suspend failed", errors)
        # A failed command must not block a later request indefinitely.
        self.mem_sleep.write_text("s2idle [deep]\n")
        result, errors = self.run_command("sleep")
        self.assertEqual(result, 0, errors)

    def test_kernel_busy_is_not_retried_by_buffered_close(self):
        class PowerControl(io.RawIOBase):
            writes = 0

            def writable(self):
                return True

            def write(self, payload):
                self.writes += 1
                if self.writes == 1:
                    raise OSError(errno.EBUSY, "Device or resource busy")
                return len(payload)

        control = PowerControl()
        path_open = Path.open

        def open_control(path, mode="r", buffering=-1, *args, **kwargs):
            if path != self.state or "w" not in mode:
                return path_open(path, mode, buffering, *args, **kwargs)
            if buffering == 0:
                return control
            buffered = io.BufferedWriter(control)
            return buffered if "b" in mode else io.TextIOWrapper(buffered)

        with mock.patch.object(Path, "open", open_control):
            result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertIn("Device or resource busy", errors)
        self.assertEqual(control.writes, 1)
        self.assertTrue(control.closed)
        self.assertEqual(self.mcu.call_args_list[-1], mock.call("suspend_resume"))

    def test_public_command_runs_under_systemd(self):
        with mock.patch.object(CLI.subprocess, "run") as run:
            run.return_value.returncode = 0
            self.assertEqual(CLI.main(["sleep"]), 0)
        self.assert_untouched()
        self.mcu.assert_not_called()
        run.assert_called_once_with(["systemctl", "start", "aico-suspend.service"], check=False)
        unit = (ROOT / "overlay-debian/etc/systemd/system/aico-suspend.service").read_text()
        self.assertIn("ExecStart=/usr/bin/aico-power sleep --execute", unit)
        self.assertIn("ExecStopPost=/usr/bin/aico-power --recover", unit)
        self.assertIn("TimeoutStartSec=infinity", unit)

    def test_systemd_failure_is_returned_to_caller(self):
        with mock.patch.object(CLI.subprocess, "run") as run, \
                contextlib.redirect_stderr(io.StringIO()) as errors:
            run.return_value.returncode = 1
            self.assertEqual(CLI.main(["sleep"]), 1)
        self.assertIn("journalctl", errors.getvalue())

    def test_requires_timekeeping_idle_driver(self):
        for path, value in ((CLI.CPU_IDLE_DRIVER, "none"), (CLI.CPU_IDLE_DISABLED, "1")):
            with self.subTest(path=path):
                previous = path.read_text()
                path.write_text(value)
                result, _ = self.run_command("sleep")
                self.assertEqual(result, 1)
                self.assert_untouched()
                self.mcu.assert_not_called()
                path.write_text(previous)

    def online_wifi(self, initial="COMPLETED"):
        CLI.WIFI_INTERFACE.mkdir()
        state = [initial]
        events = []
        def request(command):
            events.append(command)
            if command == "status":
                return state[0]
            if command == "disconnect":
                # Recovery must be on disk before disconnect can lose SSH.
                self.assertTrue(json.loads(CLI.RECOVERY_STATE.read_text())["wifi"])
                state[0] = "DISCONNECTED"
            elif command == "reconnect":
                state[0] = initial
            return "OK"
        self.wifi.side_effect = request
        return state, events

    def test_wifi_is_quiesced_before_mcu_grant_and_restored(self):
        state, events = self.online_wifi()
        def mcu(operation):
            self.assertEqual(state[0], "DISCONNECTED")
            if operation == "suspend_prepare":
                self.assertTrue(json.loads(CLI.RECOVERY_STATE.read_text())["mcu"])
            return {"state": {"rv_state": "sleeping"}}
        self.mcu.side_effect = mcu
        result, errors = self.run_command("sleep")
        self.assertEqual(result, 0, errors)
        self.assertEqual(state[0], "COMPLETED")
        self.assertEqual(events, ["status", "disconnect", "status", "reconnect"])
        self.assertFalse(CLI.RECOVERY_STATE.exists())

    def test_wifi_restored_when_kernel_rejects_suspend(self):
        state, events = self.online_wifi("SCANNING")
        self.wakeup_count.write_text("invalid")
        result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertIn("invalid kernel", errors)
        self.assertEqual(state[0], "SCANNING")
        self.assertEqual(events[-1], "reconnect")
        self.assertFalse(CLI.RECOVERY_STATE.exists())

    def test_offline_wifi_remains_offline(self):
        CLI.WIFI_INTERFACE.mkdir()
        for previous in ("DISCONNECTED", "INACTIVE", "INTERFACE_DISABLED"):
            with self.subTest(previous=previous):
                self.wifi.reset_mock()
                self.wifi.return_value = previous
                self.mem_sleep.write_text("[s2idle] deep")
                result, errors = self.run_command("sleep")
                self.assertEqual(result, 0, errors)
                self.wifi.assert_called_once_with("status")

    def test_unresponsive_supplicant_aborts_without_mcu_grant(self):
        CLI.WIFI_INTERFACE.mkdir()
        self.wifi.side_effect = subprocess.TimeoutExpired("wpa_cli", 10)
        result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertIn("timed out", errors)
        self.mcu.assert_not_called()
        self.assertEqual(self.state.read_text(), "freeze mem disk\n")

    def test_disconnect_without_ack_is_recovered(self):
        CLI.WIFI_INTERFACE.mkdir()
        self.wifi.side_effect = ["COMPLETED", RuntimeError("lost disconnect reply"), "OK"]
        result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertIn("lost disconnect", errors)
        self.assertEqual(self.wifi.call_args_list[-1], mock.call("reconnect"))
        self.mcu.assert_not_called()
        self.assertFalse(CLI.RECOVERY_STATE.exists())

    def test_wifi_timeout_cleans_up_without_sleep(self):
        CLI.WIFI_INTERFACE.mkdir()
        self.wifi.return_value = "COMPLETED"
        with mock.patch.object(CLI.time, "monotonic", side_effect=[0, 6]):
            result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertIn("did not quiesce", errors)
        self.assertEqual(self.wifi.call_args_list[-1], mock.call("reconnect"))
        self.mcu.assert_not_called()
        self.assertEqual(self.state.read_text(), "freeze mem disk\n")

    def test_stop_post_recovers_persisted_intents_after_worker_death(self):
        CLI.save_recovery({"wifi": True, "mcu": True})
        self.assertEqual(CLI.main(["--recover"]), 0)
        self.mcu.assert_called_once_with("suspend_resume")
        self.wifi.assert_called_once_with("reconnect")
        self.assertFalse(CLI.RECOVERY_STATE.exists())
        self.mcu.reset_mock()
        self.wifi.reset_mock()
        self.assertEqual(CLI.main(["--recover"]), 0)
        self.mcu.assert_not_called()
        self.wifi.assert_not_called()

    def test_mcu_cleanup_failure_still_restores_wifi_and_can_retry(self):
        CLI.save_recovery({"wifi": True, "mcu": True})
        self.mcu.side_effect = RuntimeError("UART unavailable")
        with contextlib.redirect_stderr(io.StringIO()) as errors:
            self.assertEqual(CLI.main(["--recover"]), 1)
        self.assertIn("UART unavailable", errors.getvalue())
        self.wifi.assert_called_once_with("reconnect")
        self.assertEqual(json.loads(CLI.RECOVERY_STATE.read_text()), {"wifi": False, "mcu": True})
        self.mcu.side_effect = None
        self.assertEqual(CLI.main(["--recover"]), 0)
        self.wifi.assert_called_once_with("reconnect")
        self.assertFalse(CLI.RECOVERY_STATE.exists())

    def test_wpa_cli_fail_reply_is_an_error_even_with_zero_exit(self):
        with mock.patch.object(CLI, "run_control", return_value="FAIL"):
            # Invoke the real function rather than the per-test Wi-Fi mock.
            other = importlib.util.module_from_spec(SPEC)
            LOADER.exec_module(other)
            with mock.patch.object(other, "run_control", return_value="FAIL"):
                with self.assertRaisesRegex(RuntimeError, "failed"):
                    other.wifi("disconnect")
                with self.assertRaisesRegex(RuntimeError, "supplicant state"):
                    other.wifi("status")

    def test_invalid_command_has_no_power_side_effects(self):
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as error:
            CLI.main(["hibernate"])
        self.assertNotEqual(error.exception.code, 0)
        self.assert_untouched()


if __name__ == "__main__":
    unittest.main()
