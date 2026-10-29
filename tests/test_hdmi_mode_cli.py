#!/usr/bin/env python3
"""Persistence and failure handling for the installed HDMI mode command."""

import contextlib
import importlib.machinery
import importlib.util
import io
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
LOADER = importlib.machinery.SourceFileLoader(
    "hdmi_mode", str(ROOT / "overlay-debian/usr/lib/aico/aico-hdmi-mode"))
SPEC = importlib.util.spec_from_loader(LOADER.name, LOADER)
CLI = importlib.util.module_from_spec(SPEC)
LOADER.exec_module(CLI)


class HdmiModeTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.patch("MODE_FILE", self.root / "system/hdmi-mode")
        self.patch("EDID_DIR", ROOT / "overlay-debian/usr/share/aico/hdmi")

    def patch(self, name, value):
        patch = mock.patch.object(CLI, name, value)
        patch.start()
        self.addCleanup(patch.stop)

    def test_mode_assets_and_command_ship_in_business_updates(self):
        loader = importlib.machinery.SourceFileLoader(
            "system_config", str(ROOT / "scripts/debian-package/system_config.py"))
        spec = importlib.util.spec_from_loader(loader.name, loader)
        config = importlib.util.module_from_spec(spec)
        loader.exec_module(config)
        inventory = config.inventory()
        for name in ("usr/lib/aico/aico-hdmi-mode",
                     "usr/share/aico/hdmi/hdmi_1080p30_cta.hex",
                     "usr/share/aico/hdmi/hdmi_1080p60_cta.hex"):
            self.assertEqual(inventory[name]["activation"], "live")
        self.assertEqual(inventory["usr/lib/aico/aico-hdmi-mode"]["mode"], "0755")

    def test_default_and_persistent_switches(self):
        self.assertEqual(CLI.read_mode(), ("1080p60", "default"))
        for mode in ("1080p30", "1080p60"):
            CLI.save_mode(mode)
            self.assertEqual(CLI.read_mode(), (mode, "saved"))
            self.assertEqual(CLI.MODE_FILE.read_text(), mode + "\n")
            self.assertEqual(CLI.MODE_FILE.stat().st_mode & 0o777, 0o644)
            self.assertEqual(list(CLI.MODE_FILE.parent.glob(".hdmi-mode-*")), [])

    def test_failed_atomic_replace_preserves_previous_mode(self):
        CLI.save_mode("1080p30")
        with mock.patch.object(CLI.os, "replace", side_effect=OSError("write failed")):
            with self.assertRaises(OSError):
                CLI.save_mode("1080p60")
        self.assertEqual(CLI.read_mode(), ("1080p30", "saved"))
        self.assertEqual(list(CLI.MODE_FILE.parent.glob(".hdmi-mode-*")), [])

    def test_missing_asset_does_not_change_selection(self):
        CLI.save_mode("1080p30")
        self.patch("EDID_DIR", self.root)
        with self.assertRaises(FileNotFoundError):
            CLI.save_mode("1080p60")
        self.assertEqual(CLI.read_mode(), ("1080p30", "saved"))

    def test_corrupt_state_is_not_silently_defaulted(self):
        CLI.MODE_FILE.parent.mkdir()
        CLI.MODE_FILE.write_text("1080p60\nextra\n")
        with self.assertRaises(ValueError):
            CLI.read_mode()

    def test_restart_failure_keeps_saved_choice_and_reports_retry(self):
        with tempfile.TemporaryFile() as lock, \
                mock.patch("builtins.open", return_value=lock), \
                mock.patch.object(CLI.os, "geteuid", return_value=0), \
                mock.patch.object(CLI.sys, "argv", ["aico-hdmi-mode", "1080p30"]), \
                mock.patch.object(CLI.subprocess, "run", return_value=subprocess.CompletedProcess([], 1)) as run, \
                contextlib.redirect_stderr(io.StringIO()) as errors:
            self.assertEqual(CLI.main(), 1)
        self.assertEqual(CLI.read_mode(), ("1080p30", "saved"))
        run.assert_called_once_with(["systemctl", "restart", "aico-frame.service"], check=False)
        self.assertIn("capture restart failed", errors.getvalue())


if __name__ == "__main__":
    unittest.main()
