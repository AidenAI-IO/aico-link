#!/usr/bin/env python3
"""Exercise the installed suspend command without accessing host power controls."""

import contextlib
import fcntl
import importlib.machinery
import importlib.util
import io
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
        self.state.write_text("freeze mem disk\n")
        self.mem_sleep.write_text("[s2idle] deep\n")
        self.patch("POWER_DIR", self.power)
        self.patch("LOCK_PATH", self.root / "aico-power.lock")
        self.patch("os.geteuid", return_value=0)
        self.sync = self.patch("os.sync")

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
            result = CLI.main([argument])
        return result, errors.getvalue()

    def assert_untouched(self):
        self.assertEqual(self.state.read_text(), "freeze mem disk\n")
        self.assertEqual(self.mem_sleep.read_text(), "[s2idle] deep\n")
        self.sync.assert_not_called()

    def test_installed_command_is_executable(self):
        self.assertTrue(COMMAND.stat().st_mode & 0o111)

    def test_both_commands_select_mode_before_requesting_mem(self):
        for argument, selected in (("sleep", "s2idle"), ("deep-sleep", "deep")):
            with self.subTest(argument=argument):
                self.state.write_text("freeze mem disk\n")
                self.mem_sleep.write_text("[s2idle] deep\n")
                events = []
                write_text = Path.write_text

                def record_write(path, value, *args, **kwargs):
                    events.append((path.name, value))
                    return write_text(path, value, *args, **kwargs)

                with mock.patch.object(Path, "write_text", record_write), \
                        mock.patch.object(CLI.os, "sync", side_effect=lambda: events.append("sync")):
                    result, errors = self.run_command(argument)
                self.assertEqual(result, 0, errors)
                self.assertEqual(self.mem_sleep.read_text(), selected + "\n")
                self.assertEqual(self.state.read_text(), "mem\n")
                self.assertLess(events.index(("mem_sleep", selected + "\n")),
                                events.index(("state", "mem\n")))
                self.assertLess(events.index("sync"), events.index(("state", "mem\n")))

    def test_mode_currently_selected_in_brackets_is_supported(self):
        self.mem_sleep.write_text("s2idle [deep]\n")
        result, errors = self.run_command("deep-sleep")
        self.assertEqual(result, 0, errors)
        self.assertEqual(self.state.read_text(), "mem\n")

    def test_non_root_fails_before_mutation(self):
        with mock.patch.object(CLI.os, "geteuid", return_value=1000):
            result, errors = self.run_command("sleep")
        self.assertEqual(result, 1)
        self.assertTrue(errors.strip())
        self.assert_untouched()

    def test_unsupported_mode_does_not_request_suspend(self):
        self.mem_sleep.write_text("[s2idle]\n")
        result, errors = self.run_command("deep-sleep")
        self.assertEqual(result, 1)
        self.assertIn("deep", errors)
        self.assertEqual(self.mem_sleep.read_text(), "[s2idle]\n")
        self.assertEqual(self.state.read_text(), "freeze mem disk\n")

    def test_mode_requires_exact_token(self):
        self.mem_sleep.write_text("[s2idle] nodeep\n")
        result, _ = self.run_command("deep-sleep")
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
        write_text = Path.write_text

        def fail_selection(path, value, *args, **kwargs):
            if path == self.mem_sleep:
                raise OSError("mode selection failed")
            return write_text(path, value, *args, **kwargs)

        with mock.patch.object(Path, "write_text", fail_selection):
            result, errors = self.run_command("deep-sleep")
        self.assertEqual(result, 1)
        self.assertIn("mode selection failed", errors)
        self.assertEqual(self.state.read_text(), "freeze mem disk\n")

    def test_kernel_suspend_error_is_nonzero_and_releases_lock(self):
        write_text = Path.write_text

        def fail_suspend(path, value, *args, **kwargs):
            if path == self.state:
                raise OSError("suspend failed: Operation not supported")
            return write_text(path, value, *args, **kwargs)

        with mock.patch.object(Path, "write_text", fail_suspend):
            result, errors = self.run_command("deep-sleep")
        self.assertEqual(result, 1)
        self.assertIn("suspend failed", errors)
        # A failed command must not block a later request indefinitely.
        self.mem_sleep.write_text("s2idle [deep]\n")
        result, errors = self.run_command("sleep")
        self.assertEqual(result, 0, errors)

    def test_invalid_command_has_no_power_side_effects(self):
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as error:
            CLI.main(["hibernate"])
        self.assertNotEqual(error.exception.code, 0)
        self.assert_untouched()


if __name__ == "__main__":
    unittest.main()
