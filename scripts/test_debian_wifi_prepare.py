#!/usr/bin/env python3
"""Exercise Wi-Fi module reset and driver startup with isolated sysfs and command fixtures."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

REPO = Path(__file__).resolve().parents[1]
HELPERS = REPO / "overlay-debian/usr/lib/aico"
HOST = "ff9a0000.mmc"
RESET = ["1 low 0 %s -" % HOST, "2 low 1 %s -" % HOST]
ENUMERATED = "0.1 low 1 %s %s" % (HOST, HOST)
BOTH_FUNCTIONS = [("390b", 1, "0x0082"), ("390b", 2, "0x0182")]


class StartupFixture(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.host = self.root / "sys/bus/platform/devices" / HOST
        self.driver = self.root / "sys/bus/platform/drivers/dwmmc_rockchip"
        self.gpio = self.root / "sys/class/gpio/gpio117"
        self.model = self.root / "proc/device-tree/model"
        self.lib = self.root / "usr/lib/aico"
        self.modules = self.lib / "platform/modules"
        self.bin = self.root / "bin"
        for path in (self.host, self.driver, self.gpio, self.model.parent,
                     self.modules, self.bin):
            path.mkdir(parents=True, exist_ok=True)
        self.model.write_bytes(b"Aico Link SCH v1\0")
        (self.driver / HOST).touch()
        (self.gpio / "direction").write_text("in\n")
        (self.gpio / "value").write_text("0\n")
        self.stage_functions(BOTH_FUNCTIONS)
        for name in ("cfg80211", "aic8800_bsp", "aic8800_fdrv"):
            (self.modules / (name + ".ko")).touch()
        for name in ("aico-wifi-prepare", "aico-wifi-driver", "aico-bluetooth-prepare"):
            script = (HELPERS / name).read_text()
            for prefix in ("/sys/", "/proc/", "/usr/lib/aico/", "/userdata/"):
                script = script.replace(prefix, str(self.root) + prefix)
            script = script.replace("/dev/ttyS0", "/dev/null")
            path = self.lib / name
            path.write_text(script)
            path.chmod(0o755)
        for path in (self.lib / "aico-boot-timeline", self.lib / "agent",
                     self.bin / "logger"):
            path.write_text("#!/bin/sh\nexit 0\n")
            path.chmod(0o755)
        # Record the reset state at every delay. The staged SDIO functions
        # appear only once the host has been rebound, as on the board.
        (self.bin / "sleep").write_text("""#!/bin/sh
driver="$FIXTURE/sys/bus/platform/drivers/dwmmc_rockchip"
gpio="$FIXTURE/sys/class/gpio/gpio117"
host="$FIXTURE/sys/bus/platform/devices/ff9a0000.mmc"
show() { if [ -f "$1" ]; then cat "$1"; else echo -; fi; }
echo "$1 $(show "$gpio/direction") $(show "$gpio/value") $(show "$driver/unbind") $(show "$driver/bind")" >> "$FIXTURE/sleeps"
if [ -n "${FAIL_WHILE_LOW:-}" ] && [ ! -f "$driver/bind" ]; then
    exit 1
fi
if [ -f "$driver/bind" ] && [ -d "$FIXTURE/staged" ]; then
    mv "$FIXTURE/staged" "$host/mmc_host"
fi
exit 0
""")
        (self.bin / "sleep").chmod(0o755)
        (self.bin / "insmod").write_text("""#!/usr/bin/env python3
import os, sys
from pathlib import Path
root = Path(os.environ["FIXTURE"])
module = Path(sys.argv[1]).stem
with (root / "loads").open("a") as log:
    log.write(module + "\\n")
if module.startswith("aic8800"):
    # The module must be reset and re-enumerated before any AIC driver loads.
    bind = root / "sys/bus/platform/drivers/dwmmc_rockchip/bind"
    assert bind.read_text().strip() == "ff9a0000.mmc"
if module == os.environ.get("FAIL_MODULE"):
    sys.exit(1)
(root / "sys/module" / module).mkdir(parents=True, exist_ok=True)
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

    def stage_functions(self, functions):
        """Replace the SDIO functions the host reports after it is rebound."""
        staged = self.root / "staged"
        shutil.rmtree(staged, ignore_errors=True)
        staged.mkdir()
        for card, function, device, *vendor in functions:
            name = "mmc2:" + card
            node = staged / "mmc2" / name / (name + ":" + str(function))
            node.mkdir(parents=True)
            (node / "vendor").write_text((vendor[0] if vendor else "0xc8a1") + "\n")
            (node / "device").write_text(device + "\n")

    def run_helper(self, name="aico-wifi-prepare", **extra_env):
        return subprocess.run(["sh", str(self.lib / name)], text=True, capture_output=True,
                              env={**os.environ, "PATH": str(self.bin) + ":" + os.environ["PATH"],
                                   "FIXTURE": str(self.root), **extra_env}, timeout=15)

    def sleeps(self):
        path = self.root / "sleeps"
        return path.read_text().splitlines() if path.exists() else []

    def sysfs_snapshot(self):
        return {str(p.relative_to(self.root)): p.read_bytes()
                for p in (self.root / "sys").rglob("*") if p.is_file()}

    def load_names(self):
        path = self.root / "loads"
        return path.read_text().splitlines() if path.exists() else []


class WifiPrepareTest(StartupFixture):
    def assert_untouched(self, success, reason=None):
        before = self.sysfs_snapshot()
        result = self.run_helper()
        self.assertEqual(result.returncode == 0, success, result.stderr)
        if reason:
            self.assertIn(reason, result.stderr)
        self.assertEqual(self.sysfs_snapshot(), before)
        self.assertEqual(self.sleeps(), [])

    def test_reset_pulse_precedes_rebind_and_same_card_enumeration(self):
        result = self.run_helper()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("result=success", result.stderr)
        self.assertIn("card=mmc2:390b", result.stderr)
        self.assertEqual(self.sleeps(), RESET + [ENUMERATED])

    def test_other_board_untouched(self):
        self.model.write_text("Luckfox Pico Zero")
        self.assert_untouched(True)

    def test_live_interface_untouched(self):
        (self.root / "sys/class/net/wlan0").mkdir(parents=True)
        self.assert_untouched(True, "wlan0-present")

    def test_loaded_driver_without_interface_refuses_reset(self):
        (self.root / "sys/module/aic8800_bsp").mkdir(parents=True)
        self.assert_untouched(False, "driver-already-loaded")

    def test_unbound_host_refuses_reset(self):
        (self.driver / HOST).unlink()
        self.assert_untouched(False, "sdio-host-unbound")

    def test_absent_partial_wrong_identity_and_split_cards_fail(self):
        cases = [[], [("390b", 1, "0x0082")],
                 [("390b", 1, "0x0082", "0xffff"), ("390b", 2, "0x0182")],
                 [("390b", 1, "0x0182"), ("390b", 2, "0x0082")],
                 [("390b", 1, "0x0082"), ("4000", 2, "0x0182")]]
        for functions in cases:
            with self.subTest(functions=functions):
                shutil.rmtree(self.host / "mmc_host", ignore_errors=True)
                for path in (self.root / "sleeps", self.driver / "bind", self.driver / "unbind"):
                    path.unlink(missing_ok=True)
                (self.gpio / "value").write_text("0\n")
                self.stage_functions(functions)
                result = self.run_helper()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("sdio-enumeration-timeout", result.stderr)
                self.assertEqual(self.sleeps(), RESET + [ENUMERATED] * 50)
                # A failed start must not leave the module held in reset.
                self.assertEqual((self.gpio / "direction").read_text().strip(), "high")

    def test_failure_while_low_restores_enable_and_host(self):
        result = self.run_helper(FAIL_WHILE_LOW="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.gpio / "direction").read_text().strip(), "high")
        self.assertEqual((self.driver / "bind").read_text().strip(), HOST)


class WifiDriverTest(StartupFixture):
    def test_reset_precedes_module_load_and_restart_skips_reset(self):
        result = self.run_helper("aico-wifi-driver")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.load_names(), ["cfg80211", "aic8800_bsp", "aic8800_fdrv"])
        self.assertEqual(self.sleeps()[:3], RESET + [ENUMERATED])
        (self.driver / "unbind").unlink()
        result = self.run_helper("aico-wifi-driver")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("wlan0-present", result.stderr)
        self.assertFalse((self.driver / "unbind").exists())
        self.assertEqual(len(self.load_names()), 3)

    def test_enumeration_failure_loads_no_module(self):
        self.stage_functions([])
        result = self.run_helper("aico-wifi-driver")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.load_names(), [])

    def test_bsp_probe_failure_skips_fdrv(self):
        result = self.run_helper("aico-wifi-driver", FAIL_MODULE="aic8800_bsp")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("aic8800_fdrv", self.load_names())

    def test_missing_mandatory_bsp_fails_without_fdrv(self):
        (self.modules / "aic8800_bsp.ko").unlink()
        result = self.run_helper("aico-wifi-driver")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("reason=missing", result.stderr)
        self.assertNotIn("aic8800_fdrv", self.load_names())

    def test_invalid_options_do_not_touch_hardware(self):
        for option, reason in (("AICO_WIFI_HE", "invalid-he_on"),
                               ("AICO_WIFI_LOW_POWER", "invalid-lp_on")):
            with self.subTest(option=option):
                before = self.sysfs_snapshot()
                result = self.run_helper("aico-wifi-driver", **{option: "invalid"})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(reason, result.stderr)
                self.assertEqual(self.sysfs_snapshot(), before)
                self.assertEqual(self.load_names(), [])

    def test_low_power_option_reaches_module_loader(self):
        result = self.run_helper("aico-wifi-driver", AICO_WIFI_LOW_POWER="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("params=he_on=1 custregd=1 lp_on=1", result.stderr)


class BluetoothPrepareTest(StartupFixture):
    def test_requires_loaded_firmware_driver(self):
        result = self.run_helper("aico-bluetooth-prepare")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("firmware driver is not loaded", result.stderr)
        (self.root / "sys/module/aic8800_fdrv").mkdir(parents=True)
        result = self.run_helper("aico-bluetooth-prepare")
        self.assertEqual(result.returncode, 0, result.stderr)


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
        return subprocess.run(["sh", str(HELPERS / "aico-bluetooth-clear-adv")],
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
