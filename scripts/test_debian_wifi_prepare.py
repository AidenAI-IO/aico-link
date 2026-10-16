#!/usr/bin/env python3
"""Exercise retained Wi-Fi startup with isolated sysfs and command fixtures."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

REPO = Path(__file__).resolve().parents[1]
HELPERS = REPO / "overlay-debian/usr/lib/aiden"


class StartupFixture(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.host = self.root / "sys/bus/platform/devices/ff9a0000.mmc"
        self.driver = self.root / "sys/bus/platform/drivers/dwmmc_rockchip"
        self.control = self.host / "power/control"
        self.state = self.root / "sys/devices/platform/aic-bsp/aicbsp_info/fw_state"
        self.model = self.root / "proc/device-tree/model"
        self.optin = self.root / "sys/firmware/devicetree/base/mmc@ff9a0000/aiden,aic8800d80-retained-sdio"
        self.lib = self.root / "usr/lib/aiden"
        self.modules = self.lib / "platform/modules"
        self.bin = self.root / "bin"
        for path in (self.driver, self.control.parent, self.model.parent,
                     self.optin.parent, self.modules, self.bin):
            path.mkdir(parents=True, exist_ok=True)
        self.model.write_bytes(b"Aiden SCH v1\0")
        self.optin.touch()
        self.control.write_text("auto\n")
        (self.driver / "ff9a0000.mmc").touch()
        # Sentinel files catch any reset/rebind writes even in failure cleanup.
        for path in (self.driver / "bind", self.driver / "unbind",
                     self.root / "sys/class/gpio/export",
                     self.root / "sys/class/gpio/gpio117/direction",
                     self.root / "sys/class/gpio/gpio117/value"):
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("untouched\n")
        self.add_function("390b", 1, "0x0082")
        self.add_function("390b", 2, "0x0182")
        for name in ("cfg80211", "aic8800_bsp", "aic8800_fdrv"):
            (self.modules / (name + ".ko")).touch()
        for name in ("aiden-wifi-prepare", "aiden-wifi-driver", "aiden-bluetooth-prepare"):
            script = (HELPERS / name).read_text()
            for prefix in ("/sys/", "/proc/", "/usr/lib/aiden/", "/userdata/"):
                script = script.replace(prefix, str(self.root) + prefix)
            script = script.replace("/dev/ttyS0", "/dev/null")
            path = self.lib / name
            path.write_text(script)
            path.chmod(0o755)
        for path in (self.lib / "aiden-boot-timeline", self.lib / "agent",
                     self.bin / "logger"):
            path.write_text("#!/bin/sh\nexit 0\n")
            path.chmod(0o755)
        (self.bin / "sleep").write_text('#!/bin/sh\nprintf "%s\\n" "$1" >> "$FIXTURE/sleeps"\n')
        (self.bin / "sleep").chmod(0o755)
        (self.bin / "insmod").write_text("""#!/usr/bin/env python3
import os, sys
from pathlib import Path
root = Path(os.environ["FIXTURE"])
module = Path(sys.argv[1]).stem
with (root / "loads").open("a") as log:
    log.write(module + "\\n")
if module in ("aic8800_bsp", "aic8800_fdrv"):
    assert (root / "sys/bus/platform/devices/ff9a0000.mmc/power/control").read_text().strip() == "on"
if module == os.environ.get("FAIL_MODULE"):
    sys.exit(1)
(root / "sys/module" / module).mkdir(parents=True, exist_ok=True)
if module == "aic8800_bsp":
    state = root / "sys/devices/platform/aic-bsp/aicbsp_info/fw_state"
    state.parent.mkdir(parents=True, exist_ok=True)
    value = os.environ.get("BSP_STATE", "provisioned")
    if value != "missing":
        state.write_text(value + "\\n")
if module == "aic8800_fdrv":
    params = root / "sys/module/aic8800_fdrv/parameters"
    params.mkdir(parents=True, exist_ok=True)
    (params / "custregd").write_text("Y\\n")
    wlan = root / "sys/class/net/wlan0"
    wlan.mkdir(parents=True, exist_ok=True)
    (wlan / "operstate").write_text("down\\n")
""")
        (self.bin / "insmod").chmod(0o755)
        (self.root / "proc/crypto").write_text("name : aes\nname : ecdh\n")

    def add_function(self, card, function, device, vendor="0xc8a1"):
        name = "mmc2:" + card
        node = self.host / "mmc_host/mmc2" / name / (name + ":" + str(function))
        node.mkdir(parents=True, exist_ok=True)
        (node / "vendor").write_text(vendor + "\n")
        (node / "device").write_text(device + "\n")

    def run_helper(self, name="aiden-wifi-prepare", **extra_env):
        return subprocess.run(["sh", str(self.lib / name)], text=True, capture_output=True,
                              env={**os.environ, "PATH": str(self.bin) + ":" + os.environ["PATH"],
                                   "FIXTURE": str(self.root), **extra_env}, timeout=15)

    def sysfs_snapshot(self):
        return {str(p.relative_to(self.root)): p.read_bytes()
                for p in (self.root / "sys").rglob("*") if p.is_file()}

    def assert_no_reset(self):
        for path in (self.driver / "bind", self.driver / "unbind",
                     self.root / "sys/class/gpio/export",
                     self.root / "sys/class/gpio/gpio117/direction",
                     self.root / "sys/class/gpio/gpio117/value"):
            self.assertEqual(path.read_text(), "untouched\n", str(path))

    def load_names(self):
        path = self.root / "loads"
        return path.read_text().splitlines() if path.exists() else []


class WifiPrepareTest(StartupFixture):
    def assert_read_only_result(self, success, reason=None):
        before = self.sysfs_snapshot()
        result = self.run_helper()
        self.assertEqual(result.returncode == 0, success, result.stderr)
        if reason:
            self.assertIn(reason, result.stderr)
        self.assertEqual(self.sysfs_snapshot(), before)
        return result

    def test_same_card_functions_ready_without_sysfs_writes(self):
        self.assert_read_only_result(True, "card=mmc2:390b")
        self.assertFalse((self.root / "sleeps").exists())

    def test_loaded_drivers_and_live_interface_are_preserved(self):
        for name in ("aic8800_bsp", "aic8800_fdrv", "aic8800_btlpm"):
            (self.root / "sys/module" / name).mkdir(parents=True)
        (self.root / "sys/class/net/wlan0").mkdir(parents=True)
        self.assert_read_only_result(True)

    def test_other_board_untouched(self):
        self.model.write_text("Luckfox Pico Zero")
        self.assert_read_only_result(True)

    def test_requires_kernel_optin(self):
        self.optin.unlink()
        self.assert_read_only_result(False, "retained-sdio-not-configured")

    def test_requires_bound_host(self):
        (self.driver / "ff9a0000.mmc").unlink()
        self.assert_read_only_result(False, "sdio-host-unbound")

    def test_absent_partial_wrong_identity_and_split_cards_fail(self):
        cases = [[], [("390b", 1, "0x0082")],
                 [("390b", 1, "0x0082", "0xffff"), ("390b", 2, "0x0182")],
                 [("390b", 1, "0x0182"), ("390b", 2, "0x0082")],
                 [("390b", 1, "0x0082"), ("4000", 2, "0x0182")]]
        for functions in cases:
            with self.subTest(functions=functions):
                shutil.rmtree(self.host / "mmc_host", ignore_errors=True)
                (self.root / "sleeps").unlink(missing_ok=True)
                for function in functions:
                    self.add_function(*function)
                self.assert_read_only_result(False, "sdio-enumeration-timeout")
                self.assertEqual((self.root / "sleeps").read_text().splitlines(), ["0.1"] * 50)


class WifiDriverTest(StartupFixture):
    def run_driver(self, **env):
        result = self.run_helper("aiden-wifi-driver", **env)
        self.assert_no_reset()
        return result

    def test_provisioned_probe_order_power_hold_and_idempotent_restart(self):
        result = self.run_driver()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.load_names(), ["cfg80211", "aic8800_bsp", "aic8800_fdrv"])
        self.assertEqual(self.control.read_text(), "auto\n")
        self.assertIn("fw_state=provisioned", result.stderr)
        self.assertFalse((self.root / "sleeps").exists())
        result = self.run_driver()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.load_names()), 3)

    def test_reused_probe_preserves_original_on_policy(self):
        self.control.write_text("on\n")
        result = self.run_driver(BSP_STATE="reused")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("fw_state=reused", result.stderr)
        self.assertEqual(self.control.read_text(), "on\n")

    def test_unknown_state_does_not_load_fdrv_or_reprovision_loaded_bsp(self):
        result = self.run_driver(BSP_STATE="unknown")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("firmware-not-ready", result.stderr)
        self.assertNotIn("aic8800_fdrv", self.load_names())
        self.assertEqual(self.control.read_text(), "auto\n")
        before = self.load_names()
        result = self.run_driver()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.load_names(), before)

    def test_missing_state_is_not_module_readiness(self):
        result = self.run_driver(BSP_STATE="missing")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("fw_state=missing", result.stderr)
        self.assertNotIn("aic8800_fdrv", self.load_names())
        self.assertEqual(self.control.read_text(), "auto\n")

    def test_bsp_probe_failure_restores_host_policy(self):
        result = self.run_driver(FAIL_MODULE="aic8800_bsp")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("aic8800_fdrv", self.load_names())
        self.assertEqual(self.control.read_text(), "auto\n")

    def test_fdrv_probe_failure_restores_host_policy(self):
        result = self.run_driver(FAIL_MODULE="aic8800_fdrv")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.control.read_text(), "auto\n")

    def test_missing_mandatory_bsp_fails_without_fdrv(self):
        (self.modules / "aic8800_bsp.ko").unlink()
        result = self.run_driver()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("reason=missing", result.stderr)
        self.assertNotIn("aic8800_fdrv", self.load_names())
        self.assertEqual(self.control.read_text(), "auto\n")

    def test_invalid_host_policy_is_not_overwritten(self):
        self.control.write_text("invalid\n")
        result = self.run_driver()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("invalid-host-power-control", result.stderr)
        self.assertEqual(self.control.read_text(), "invalid\n")
        self.assertNotIn("aic8800_bsp", self.load_names())

    def test_invalid_he_does_not_touch_hardware(self):
        before = self.sysfs_snapshot()
        result = self.run_driver(AIDEN_WIFI_HE="invalid")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.sysfs_snapshot(), before)
        self.assertEqual(self.load_names(), [])


class BluetoothPrepareTest(StartupFixture):
    def test_shared_firmware_ready_without_fdrv_presence(self):
        self.state.parent.mkdir(parents=True)
        for state in ("provisioned", "reused"):
            with self.subTest(state=state):
                self.state.write_text(state + "\n")
                result = self.run_helper("aiden-bluetooth-prepare")
                self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.root / "sys/module/aic8800_fdrv").exists())

    def test_unknown_or_missing_state_fails_even_with_fdrv(self):
        (self.root / "sys/module/aic8800_fdrv").mkdir(parents=True)
        self.state.parent.mkdir(parents=True)
        for state in (None, "unknown", "failed", "provisioned\nunknown"):
            with self.subTest(state=state):
                if state is not None:
                    self.state.write_text(state + "\n")
                result = self.run_helper("aiden-bluetooth-prepare")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("firmware is not ready", result.stderr)


class BluetoothClearAdvertisingTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        (self.bin / "hciconfig").write_text(
            '#!/bin/sh\n[ -f "$FIXTURE/up" ] && echo "\tUP RUNNING " || echo "\tDOWN "\n')
        # Reply like hcitool: a Command Complete line ending in the status byte.
        (self.bin / "hcitool").write_text(
            '#!/bin/sh\nprintf "%s\\n" "$*" >> "$FIXTURE/commands"\n'
            # The command echo ends in its own parameters, which look like a status.
            'printf "< HCI Command: ogf 0x08 plen 2\\n  00 00\\n"\n'
            '[ -n "$HCI_NO_EVENT" ] && exit 1\n'
            'printf "> HCI Event: 0x0e plen 4\\n  05 39 20 %s\\n" "${HCI_STATUS:-00}"\n')
        (self.bin / "sleep").write_text('#!/bin/sh\nprintf "%s\\n" "$1" >> "$FIXTURE/sleeps"\n')
        for name in ("hciconfig", "hcitool", "sleep"):
            (self.bin / name).chmod(0o755)

    def run_helper(self, **extra_env):
        return subprocess.run(["sh", str(HELPERS / "aiden-bluetooth-clear-adv")],
                              text=True, capture_output=True,
                              env={**os.environ, "PATH": str(self.bin) + ":" + os.environ["PATH"],
                                   "FIXTURE": str(self.root), **extra_env}, timeout=15)

    def commands(self):
        path = self.root / "commands"
        return path.read_text().splitlines() if path.exists() else []

    def test_disables_then_clears_every_set(self):
        (self.root / "up").touch()
        result = self.run_helper()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.commands(), ["-i hci0 cmd 0x08 0x0039 0x00 0x00",
                                           "-i hci0 cmd 0x08 0x003d"])
        self.assertEqual(result.stderr, "")

    def test_controller_rejection_does_not_block_startup(self):
        (self.root / "up").touch()
        result = self.run_helper(HCI_STATUS="0c")
        self.assertEqual(result.returncode, 0)
        self.assertEqual(len(self.commands()), 2)
        self.assertIn("returned status 0c", result.stderr)

    def test_missing_event_is_not_read_as_success(self):
        (self.root / "up").touch()
        result = self.run_helper(HCI_NO_EVENT="1")
        self.assertEqual(result.returncode, 0)
        self.assertIn("disabling advertising sets returned status none", result.stderr)
        self.assertIn("clearing advertising sets returned status none", result.stderr)

    def test_down_adapter_is_skipped_after_timeout(self):
        result = self.run_helper()
        self.assertEqual(result.returncode, 0)
        self.assertEqual(self.commands(), [])
        self.assertEqual(len((self.root / "sleeps").read_text().splitlines()), 10)
        self.assertIn("hci0 is not up", result.stderr)


if __name__ == "__main__":
    unittest.main()
