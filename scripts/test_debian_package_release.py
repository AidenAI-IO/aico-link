#!/usr/bin/env python3
"""Exercise real .deb metadata and the standalone release validation boundary."""
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("business_release", ROOT / "scripts/debian-package/release.py")
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "repo"
        self.root.mkdir()
        self.apps = self.root / "apps"
        self.output = self.root / "output"
        self.output.mkdir()
        self.git("init", "-q")
        (self.root / ".gitignore").write_text("apps/\noutput/\n")
        (self.root / "pico-sdk").mkdir()
        (self.root / "pico-sdk/source.c").write_text("vendored SDK\n")
        self.git("add", ".gitignore", "pico-sdk")
        self.git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "test: fixture")
        self.commit = self.git("rev-parse", "HEAD")
        metadata = self.apps / "apps/metadata"
        metadata.mkdir(parents=True)
        for name, data in {
            "hardware-demo-commit.txt": self.commit,
            "pico-sdk-commit.txt": self.commit,
            "hardware-demo-status.txt": "",
            "pico-sdk-status.txt": "",
        }.items():
            (metadata / name).write_text(data)
        (self.apps / "apps-audit").mkdir()
        (self.apps / "apps-audit/summary.txt").write_text("status=pass\n")
        pkg = self.output / "fixture"
        (pkg / "DEBIAN").mkdir(parents=True)
        (pkg / "DEBIAN/control").write_text(
            "Package: aiden-business\nVersion: 0.0.1-1\nArchitecture: armhf\n"
            "Maintainer: Test <test@example.invalid>\nDescription: Test package\n"
        )
        manifest = pkg / "usr/share/doc/aiden-business/release-manifest.json"
        manifest.parent.mkdir(parents=True)
        manifest.write_text(json.dumps({"business_release": "0.0.1", "package_revision": "1",
                                        "required_platform_contract": {"min": 1, "max_exclusive": 2}}))
        subprocess.run(["dpkg-deb", "--build", "--root-owner-group", str(pkg),
                        str(self.output / "aiden-business_0.0.1-1_armhf.deb")], check=True, stdout=subprocess.DEVNULL)
        with patch.dict(os.environ, AIDEN_BUSINESS_VERSION="0.0.1", AIDEN_BUSINESS_REVISION="1"):
            release.stage(self.root, self.apps, self.output)
        self.assets = self.output / "release"

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.root), *args], text=True).strip()

    def test_valid_release(self):
        self.assertEqual(release.verify(self.assets)["tag"], "business-v0.0.1-1")

    def test_corrupted_asset_is_rejected(self):
        with (self.assets / "aiden-business_0.0.1-1_armhf.deb").open("ab") as f:
            f.write(b"corrupt")
        with self.assertRaisesRegex(ValueError, "checksum"):
            release.verify(self.assets)

    def test_unexpected_firmware_asset_is_rejected(self):
        (self.assets / "manifest.json").write_text("{}")
        with self.assertRaisesRegex(ValueError, "unexpected"):
            release.verify(self.assets)

    def test_wrong_package_metadata_is_rejected(self):
        metadata_path = self.assets / "build-metadata.json"
        metadata = json.loads(metadata_path.read_text())
        metadata["architecture"] = "amd64"
        metadata_path.write_text(json.dumps(metadata))
        with self.assertRaisesRegex(ValueError, "architecture"):
            release.verify(self.assets)

    def test_stale_binary_is_rejected(self):
        (self.apps / "apps/metadata/hardware-demo-commit.txt").write_text("0" * 40)
        with self.assertRaisesRegex(ValueError, "another commit"):
            release.check_source(self.root, self.apps)

    def test_sdk_subtree_id_is_not_a_source_commit(self):
        (self.apps / "apps/metadata/pico-sdk-commit.txt").write_text(self.git("rev-parse", "HEAD:pico-sdk"))
        with self.assertRaisesRegex(ValueError, "SDK commit"):
            release.check_source(self.root, self.apps)

    def test_dirty_vendored_sdk_is_rejected(self):
        (self.root / "pico-sdk/source.c").write_text("modified SDK\n")
        with self.assertRaisesRegex(ValueError, "Commit source"):
            release.check_source(self.root, self.apps)

    def test_dirty_source_is_rejected(self):
        (self.root / "uncommitted").write_text("dirty")
        with self.assertRaisesRegex(ValueError, "Commit source"):
            release.check_source(self.root, self.apps)

    def test_legacy_publisher_requires_channel_workflow(self):
        result = subprocess.run([str(ROOT / "scripts/debian-package/release.sh"), "publish"],
                                text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("scripts/release/release.py publish", result.stderr)


class VersionTests(unittest.TestCase):
    def test_defaults(self):
        env = {k: v for k, v in os.environ.items() if k not in ("AIDEN_BUSINESS_VERSION", "AIDEN_BUSINESS_REVISION")}
        result = subprocess.check_output(["bash", "-c", 'source "$1"; echo "$AIDEN_BUSINESS_VERSION-$AIDEN_BUSINESS_REVISION"',
                                          "bash", str(ROOT / "scripts/debian-package/version.sh")], env=env, text=True)
        self.assertEqual(result.strip(), "0.0.1-2")

    def test_unsafe_version_is_rejected(self):
        for version in ("../escape", "1.0.0\nInjected: value", "01.0.0", "1.0.0;echo bad"):
            result = subprocess.run(["bash", "-c", 'source "$1"', "bash", str(ROOT / "scripts/debian-package/version.sh")],
                                    env={**os.environ, "AIDEN_BUSINESS_VERSION": version}, capture_output=True)
            self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    if not shutil.which("dpkg-deb"):
        raise SystemExit("Run release tests on Linux with dpkg-deb installed")
    unittest.main()
