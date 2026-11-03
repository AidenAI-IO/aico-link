#!/usr/bin/env python3
"""Exercise late SSH terminal registration without a system bus."""

import errno
import os
from pathlib import Path
import pty
import runpy
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch


ROOT = Path(__file__).resolve().parents[1]
HELPER = ROOT / "overlay-debian/usr/lib/aico/aico-ssh-session-tty"
HOOK = ROOT / "overlay-debian/etc/ssh/sshrc"
register_tty = runpy.run_path(str(HELPER))["register_tty"]


class SSHSessionTests(unittest.TestCase):
    def setUp(self):
        self.master, self.slave = pty.openpty()
        self.addCleanup(os.close, self.master)
        self.addCleanup(os.close, self.slave)
        self.env = {"SSH_TTY": os.ttyname(self.slave), "XDG_SESSION_ID": "c42"}
        self.lib = Mock()
        self.lib.sd_bus_open_system.return_value = 0
        self.lib.sd_bus_set_method_call_timeout.return_value = 0
        self.lib.sd_bus_call_method.return_value = 1
        self.methods = []

    def call(self, *args):
        self.assertEqual(args[2], b"/org/freedesktop/login1/session/self")
        method = args[4]
        self.methods.append(method)
        if method == b"TakeControl":
            self.assertEqual(args[8].value, 0, "must not force out another controller")
        if method == b"SetTTY":
            self.tty_fd = args[8].value
            self.assertEqual(os.ttyname(self.tty_fd), self.env["SSH_TTY"])
        return 1

    def run_helper(self):
        with patch.dict(os.environ, self.env, clear=True), \
                patch("ctypes.CDLL", return_value=self.lib):
            register_tty()

    def test_registers_pty_and_releases_resources(self):
        self.lib.sd_bus_call_method.side_effect = self.call
        self.run_helper()
        self.assertEqual(self.methods, [b"TakeControl", b"SetTTY", b"ReleaseControl"])
        self.lib.sd_bus_unref.assert_called_once()
        with self.assertRaises(OSError):
            os.fstat(self.tty_fd)

    def test_releases_control_when_setting_tty_fails(self):
        def reject_tty(*args):
            self.call(*args)
            return -errno.EACCES if args[4] == b"SetTTY" else 1

        self.lib.sd_bus_call_method.side_effect = reject_tty
        with self.assertRaises(OSError):
            self.run_helper()
        self.assertEqual(self.methods[-1], b"ReleaseControl")
        self.lib.sd_bus_unref.assert_called_once()
        with self.assertRaises(OSError):
            os.fstat(self.tty_fd)

    def test_existing_controller_is_left_alone(self):
        self.lib.sd_bus_call_method.return_value = -errno.EBUSY
        with self.assertRaises(OSError):
            self.run_helper()
        self.lib.sd_bus_call_method.assert_called_once()
        self.lib.sd_bus_unref.assert_called_once()

    def test_non_tty_and_missing_session_do_not_connect(self):
        for env in ({}, {"SSH_TTY": self.env["SSH_TTY"]},
                    {"XDG_SESSION_ID": "c42"},
                    {"SSH_TTY": "/dev/null", "XDG_SESSION_ID": "c42"}):
            with self.subTest(env=env), patch.dict(os.environ, env, clear=True), \
                    patch("ctypes.CDLL") as load:
                register_tty()
                load.assert_not_called()

    def test_hook_preserves_command_output_and_x11_cookie_handling(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            helper = directory / "helper"
            helper.write_text("#!/bin/sh\nexit 1\n")
            helper.chmod(0o755)
            xauth = directory / "xauth"
            xauth.write_text('#!/bin/sh\ncat >"$XAUTH_TEST_LOG"\n')
            xauth.chmod(0o755)
            hook = directory / "sshrc"
            hook.write_text(HOOK.read_text().replace(
                "/usr/lib/aico/aico-ssh-session-tty", str(helper)
            ).replace("/usr/bin/xauth", str(xauth)))
            for display, expected in (("localhost:10.0", "unix:10.0"),
                                      ("other:10.0", "other:10.0")):
                log = directory / "xauth.log"
                result = subprocess.run(
                    ["sh", str(hook)], input="MIT-MAGIC-COOKIE-1 abc123\n",
                    text=True, capture_output=True,
                    env={**self.env, "PATH": os.environ["PATH"],
                         "DISPLAY": display, "XAUTH_TEST_LOG": str(log)},
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, "")
                self.assertEqual(log.read_text(),
                                 f"remove {expected}\nadd {expected} MIT-MAGIC-COOKIE-1 abc123\n")


if __name__ == "__main__":
    unittest.main()
