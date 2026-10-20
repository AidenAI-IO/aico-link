#!/usr/bin/env python3
"""Real single-package config upgrades in a disposable Debian container."""
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts/debian-package"))
import system_config
import test_debian_package_lifecycle as lifecycle
from test_debian_package_lifecycle import UNITS, RESTART, TTYD

spec = importlib.util.spec_from_file_location("standalone_release", ROOT / "scripts/debian-package/release.py")
standalone_release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(standalone_release)


@unittest.skipUnless(shutil.which("dpkg-deb") and shutil.which("dpkg"), "Requires Linux dpkg")
class RuntimeConfigTests(unittest.TestCase):
    def setUp(self):
        self.fixture = lifecycle.LifecycleTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.root, self.device = self.fixture.root, self.fixture.device
        self.env = {**self.fixture.env, "AIDEN_BUSINESS_REVISION": "1", "AIDEN_RELEASE_CHANNEL": "",
                    "AIDEN_PLATFORM_BASE": "", "AIDEN_SYSTEM_FINGERPRINT": ""}
        self.source = self.root / "source"
        for path in system_config.inventory():
            target = self.source / "overlay-debian" / path
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(ROOT / "overlay-debian" / path, target)
        # Debian 13 systemd creates this link before aiden-business is installed.
        # A conffile at the legacy path would be left as locale.dpkg-new.
        locale = self.device / "etc/default/locale"
        locale.parent.mkdir(parents=True, exist_ok=True)
        locale.symlink_to("../locale.conf")
        self.fixture.set_states({unit: "inactive" for unit in UNITS})

    def package(self, version, *, fail_preinst=False):
        package = self.root / ("package-" + version)
        control = package / "DEBIAN"
        control.mkdir(parents=True)
        (control / "control").write_text(
            f"Package: aiden-business\nVersion: {version}-1\nArchitecture: all\n"
            "Maintainer: Test <test@example.test>\nDescription: runtime config fixture\n")
        with patch.object(system_config, "ROOT", self.source):
            system_config.stage(package)
            inventory = system_config.inventory()
        for phase in ("preinst", "postinst", "prerm", "postrm"):
            text = (self.fixture.scripts / phase).read_text()
            text = re.sub(r"^NEW_FILES = .*", lambda _: "NEW_FILES = " + repr(inventory), text, flags=re.M)
            text = text.replace("set -eu\n", "set -eu\nunset DPKG_ROOT\n", 1)
            if fail_preinst and phase == "preinst":
                text = text.removesuffix("exit 0\n") + "exit 42\n"
            (control / phase).write_text(text)
            (control / phase).chmod(0o755)
        archive = self.root / f"aiden-business_{version}-1_all.deb"
        subprocess.run(["dpkg-deb", "--build", "--root-owner-group", str(package), str(archive)],
                       check=True, stdout=subprocess.DEVNULL)
        standalone_release.verify_runtime_config(archive, 1)
        return archive

    def dpkg(self, *args, success=True):
        return self.fixture.dpkg("--force-confold", *args, success=success)

    def boot(self):
        for name in ("reboot-required", "reboot-required.pkgs", "aiden-business-reboot-required.json"):
            (self.device / "run" / name).unlink(missing_ok=True)
        self.fixture.set_states({unit: "inactive" if unit in (RESTART, TTYD) else "active" for unit in UNITS})

    def change(self, path, content):
        target = self.source / "overlay-debian" / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content)

    def changes(self):
        path = self.device / "run/aiden-business-reboot-required.json"
        return json.loads(path.read_text()) if path.exists() else []

    def test_ownership_modes_conffiles_and_rootfs_exclusion(self):
        package = self.package("0.0.2")
        self.dpkg("-i", package)
        system_config.audit(self.device)
        profile = "etc/profile.d/aiden-env.sh"
        (self.device / profile).write_text((self.device / profile).read_text() + "\n# administrator setting\n")
        original = (self.source / "overlay-debian" / profile).read_text()
        self.change(profile, original + "\n# upstream change\n")
        self.dpkg("-i", self.package("0.0.3"))
        self.assertIn("administrator setting", (self.device / profile).read_text())
        self.assertIn("upstream change", (self.device / (profile + ".dpkg-dist")).read_text())
        self.assertEqual((self.device / "etc/sudoers.d/20-aiden-proxy").stat().st_mode & 0o777, 0o440)
        subprocess.run([shutil.which("visudo"), "-cf", str(self.device / "etc/sudoers.d/20-aiden-proxy")], check=True)
        overlay = self.root / "overlay"
        exclude = self.root / "exclude"
        subprocess.run([sys.executable, str(ROOT / "scripts/debian-package/system_config.py"), "exclude", str(exclude)], check=True)
        subprocess.run(["rsync", "-a", "--exclude-from=" + str(exclude), str(ROOT / "overlay-debian") + "/", str(overlay)], check=True)
        for path in system_config.inventory():
            self.assertFalse((overlay / path).exists(), path)
        self.assertTrue((overlay / "usr/lib/aiden/platform/contract.json").exists())

    def test_locale_conffile_installs_behind_debian_compatibility_link(self):
        self.dpkg("-i", self.package("0.0.2"))
        locale = self.device / "etc/default/locale"
        self.assertEqual(str(locale.readlink()), "../locale.conf")
        self.assertEqual(locale.read_text(), "LANG=C.UTF-8\n")
        self.assertFalse(locale.with_name("locale.dpkg-new").exists())
        self.assertFalse((self.device / "etc/locale.conf").is_symlink())
        inventory = json.loads((self.device / "usr/lib/aiden/runtime-config.json").read_text())["files"]
        self.assertIn("etc/locale.conf", inventory)
        self.assertNotIn("etc/default/locale", inventory)
        system_config.audit(self.device)

    def test_hdmi_profiles_verify_with_a_narrow_namespace(self):
        archive = self.package("0.0.2")
        staged = self.root / "package-0.0.2"
        manifest = staged / "usr/lib/aiden/runtime-config.json"
        document = json.loads(manifest.read_text())
        for mode in ("1080p30", "1080p60"):
            path = f"usr/share/aiden/hdmi/hdmi_{mode}_cta.hex"
            self.assertEqual(document["files"][path]["activation"], "live")
            self.assertEqual((staged / path).read_bytes(), (ROOT / "overlay-debian" / path).read_bytes())

        record = document["files"]["usr/share/aiden/hdmi/hdmi_1080p60_cta.hex"]
        for path in (
            "/usr/share/aiden/hdmi/profile.hex",
            "usr/share/aiden/hdmi/../profile.hex",
            "usr/share/aiden/hdmi-extra/profile.hex",
            "usr/share/aiden/hdmi/nested/profile.hex",
            "usr/share/aiden/hdmi/profile.sh",
            "usr/share/elsewhere/profile.hex",
        ):
            with self.subTest(path=path):
                manifest.write_text(json.dumps({"format": 1, "files": {path: record}}))
                subprocess.run(["dpkg-deb", "--build", "--root-owner-group", str(staged), str(archive)],
                               check=True, stdout=subprocess.DEVNULL)
                with self.assertRaisesRegex(ValueError, "Unsafe runtime configuration path"):
                    standalone_release.verify_runtime_config(archive, 1)

    def test_upgrade_downgrade_and_reinstall_restart_only_previous_services(self):
        old = self.package("0.0.2")
        self.dpkg("-i", old)
        self.boot()
        before = self.fixture.states()
        new = self.package("0.0.3")
        for package in (new, old, old):
            self.dpkg("-i", package)
            self.assertEqual(self.fixture.states(), before)
            self.assertFalse(self.fixture.transaction.exists())
            self.assertFalse(self.fixture.config_transaction.exists())
            self.assertEqual(self.changes(), [])
        self.assertFalse(any(any(x in op for x in ("reboot", "ssh.service", "systemd-networkd.service"))
                             for op in self.fixture.operations()))

    def test_new_deferred_config_marks_reboot_but_live_config_does_not(self):
        self.dpkg("-i", self.package("0.0.2"))
        self.boot()
        self.change("etc/profile.d/aiden-new.sh", "# new login setting\n")
        self.dpkg("-i", self.package("0.0.3"))
        self.assertEqual(self.changes(), [])
        path = "etc/ssh/sshd_config.d/30-aiden.conf"
        self.change(path, "# deferred SSH setting\n")
        # This path was unknown to old prerm; new preinst must extend its snapshot.
        target = self.device / path
        target.write_text("# already supplied locally\n")
        self.dpkg("-i", self.package("0.0.4"))
        self.assertEqual(self.changes(), [])  # dpkg kept the administrator's file.
        self.change("etc/aiden/new-feature.conf", "enabled=true\n")
        self.dpkg("-i", self.package("0.0.5"))
        self.assertEqual(self.changes(), ["etc/aiden/new-feature.conf"])
        self.assertEqual((self.device / "run/reboot-required.pkgs").read_text(), "aiden-business\n")
        self.boot()
        self.dpkg("--configure", "aiden-business", success=False)  # already configured
        self.fixture.run_phase("postinst", "configure")
        self.assertEqual(self.changes(), [])

    def test_changed_network_config_marks_reboot_and_local_conffile_is_preserved(self):
        path = "etc/systemd/network/20-wlan0.network"
        self.dpkg("-i", self.package("0.0.2"))
        self.boot()
        original = (self.source / "overlay-debian" / path).read_text()
        self.change(path, original + "\n# new upstream setting\n")
        self.dpkg("-i", self.package("0.0.3"))
        self.assertEqual(self.changes(), [path])
        self.boot()
        local = self.device / path
        local.write_text(local.read_text() + "\n# local override\n")
        self.change(path, original + "\n# next upstream setting\n")
        self.dpkg("-i", self.package("0.0.4"))
        self.assertEqual(self.changes(), [])
        self.assertIn("local override", local.read_text())

    def test_deferred_helper_permission_change_requires_reboot(self):
        path = "usr/lib/aiden/aiden-new-helper"
        self.change(path, "#!/bin/sh\nexit 0\n")
        self.dpkg("-i", self.package("0.0.2"))
        self.boot()
        (self.source / "overlay-debian" / path).chmod(0o755)
        self.dpkg("-i", self.package("0.0.3"))
        self.assertEqual(self.changes(), [path])

    def test_configure_failure_keeps_services_stopped_until_retry(self):
        self.dpkg("-i", self.package("0.0.2"))
        self.boot()
        before = self.fixture.states()
        self.fixture.env["MOCK_INVALID_SUDOERS"] = "1"
        self.dpkg("-i", self.package("0.0.3"), success=False)
        self.assertTrue(self.fixture.transaction.exists())
        self.assertTrue(all(state == "inactive" for state in self.fixture.states().values()))
        del self.fixture.env["MOCK_INVALID_SUDOERS"]
        self.dpkg("--configure", "aiden-business")
        self.assertEqual(self.fixture.states(), before)

    def test_failed_preinst_restores_services_and_preserves_old_files(self):
        self.dpkg("-i", self.package("0.0.2"))
        self.boot()
        before = self.fixture.states()
        path = "etc/aiden_boot.conf"
        original = (self.device / path).read_text()
        self.change(path, "# update\n")
        self.dpkg("-i", self.package("0.0.3", fail_preinst=True), success=False)
        self.assertEqual(self.fixture.states(), before)
        self.assertEqual((self.device / path).read_text(), original)
        self.assertFalse(self.fixture.config_transaction.exists())

    def test_policy_denial_keeps_services_and_still_records_deferred_changes(self):
        self.dpkg("-i", self.package("0.0.2"))
        self.boot()
        before = self.fixture.states()
        self.fixture.policy.write_text("#!/bin/sh\nexit 101\n")
        self.fixture.policy.chmod(0o755)
        self.change("etc/aiden/new.conf", "new=true\n")
        self.dpkg("-i", self.package("0.0.3"))
        self.assertEqual(self.fixture.states(), before)
        self.assertEqual(self.changes(), ["etc/aiden/new.conf"])

    def test_production_packager_builds_one_verified_package(self):
        apps = self.root / "apps"
        (apps / "bin").mkdir(parents=True)
        for name in "agent audio_service audio_service_cli ble_service cpu_vad frame_service frame_service_cli rknn_vad ota abctl aiden-environment ttyd".split():
            binary = apps / "bin" / name
            binary.write_text("#!/bin/sh\nexit 0\n")
            binary.chmod(0o755)
        output = self.root / "production-output"
        output.mkdir()
        script = (ROOT / "scripts/debian-package/container-build.sh").read_text()
        script = script.replace("REPO_ROOT=/work APPS_DIR=/apps OUTPUT_DIR=/out",
                                f'REPO_ROOT="{ROOT}" APPS_DIR="{apps}" OUTPUT_DIR="{output}"')
        env = {**self.env, "AIDEN_BUSINESS_VERSION": "0.0.9"}
        subprocess.run(["bash", "-c", script], env=env, check=True, stdout=subprocess.DEVNULL)
        package = output / "aiden-business_0.0.9-1_armhf.deb"
        self.assertEqual(list(output.glob("*.deb")), [package])
        standalone_release.verify_runtime_config(package, 1)
        # Only binary provenance is mocked: the test binaries are explicit fixtures.
        with patch.object(standalone_release, "check_source", return_value=("a" * 40, "b" * 40)), patch.dict(os.environ, env):
            standalone_release.stage(ROOT, apps, output)
        standalone_release.verify(output / "release")
        self.assertEqual(json.loads(standalone_release.package_manifest(package))["runtime_config"], 1)
        # Tampering with the manifest cannot conceal a changed file or conffile list.
        staged = output / "package-root"
        config = staged / "etc/aiden_boot.conf"
        config.write_text(config.read_text() + "\n# tampered\n")
        subprocess.run(["dpkg-deb", "--build", "--root-owner-group", str(staged), str(package)], check=True, stdout=subprocess.DEVNULL)
        with self.assertRaisesRegex(ValueError, "differs from inventory"):
            standalone_release.verify_runtime_config(package, 1)


if __name__ == "__main__":
    if shutil.which("dpkg") and os.geteuid() != 0:
        raise SystemExit("Run as root inside the disposable test container; dpkg must replace 0440 conffiles.")
    unittest.main()
