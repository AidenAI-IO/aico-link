#!/usr/bin/env python3
"""Exercise signed indexes, contract isolation and upgrade policy with real APT."""

import contextlib
import copy
from datetime import datetime, timezone
import functools
import getpass
import hashlib
import http.server
import importlib.machinery
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts/apt"))
import repository
sys.path.insert(0, str(ROOT / "scripts/debian-package"))
import system_config

loader = importlib.machinery.SourceFileLoader("apt_source", str(ROOT / "overlay-debian/usr/lib/aico/aico-apt-source"))
spec = importlib.util.spec_from_loader(loader.name, loader)
apt_source = importlib.util.module_from_spec(spec)
loader.exec_module(apt_source)
REPO = "AidenAI-IO/aico-link"


def run(*args, **kwargs):
    return subprocess.run(args, text=True, capture_output=True, check=True, **kwargs)


class QuietHandler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *args):
        pass


@unittest.skipUnless(all(shutil.which(x) for x in ("dpkg-deb", "apt-get", "apt-cache", "gpg", "gpgv")),
                     "Requires Linux APT, dpkg-deb and GnuPG")
class RepositoryTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="aico-apt-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.gpg = self.root / "gnupg"
        self.gpg.mkdir(mode=0o700)
        self.old_gpg = os.environ.get("GNUPGHOME")
        os.environ["GNUPGHOME"] = str(self.gpg)
        self.addCleanup(self.restore_gpg)
        run("gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "",
            "--quick-generate-key", "APT Test <apt@example.test>", "ed25519", "sign", "0")
        self.key = self.root / "public.asc"
        self.key.write_text(run("gpg", "--batch", "--armor", "--export", "apt@example.test").stdout)
        self.cache = self.root / "cache"
        self.site = self.root / "site"
        self.history = []

    def restore_gpg(self):
        subprocess.run(["gpgconf", "--kill", "all"], capture_output=True)
        if self.old_gpg is None:
            os.environ.pop("GNUPGHOME", None)
        else:
            os.environ["GNUPGHOME"] = self.old_gpg

    def release(self, version, channel="dev", kind="business", contract=None, runtime_config=0):
        previous = next((r for r in reversed(self.history) if r["channel"] == channel), None)
        commit = hashlib.sha1(version.encode()).hexdigest()
        tag = f"{channel}-v{version}"
        platform = ({"contract": contract, "base_release": tag, "source_commit": commit} if kind == "ota"
                    else copy.deepcopy(previous["platform"]))
        record = {
            "format": 1, "repo": REPO, "channel": channel, "version": version, "tag": tag, "kind": kind,
            "source_commit": commit, "source_tree": commit, "build_time": "2026-01-01T00:00:00Z",
            "platform": platform, "previous_tag": previous["tag"] if previous else None,
            "previous_commit": previous["source_commit"] if previous else None,
            "fingerprints": {"business": hashlib.sha256(version.encode()).hexdigest(),
                             "system": hashlib.sha256(str(platform["contract"]).encode()).hexdigest()},
        }
        declaration = {"format": 1, "product": "aico", "platform_id": "luckfox-rv1106", "architecture": "armhf",
                       "os_release": "debian-13", "libc": "glibc", "channel": channel,
                       "platform_contract": platform["contract"], "base_release": platform["base_release"],
                       "system_fingerprint": record["fingerprints"]["system"]}
        number = platform["contract"]
        manifest = {"business_release": version, "package_revision": "1", "platform": declaration,
                    "required_platform_contract": {"min": number, "max_exclusive": number + 1}}
        if runtime_config:
            record["runtime_config"] = manifest["runtime_config"] = declaration["runtime_config"] = runtime_config
        package_root = self.root / tag
        (package_root / "DEBIAN").mkdir(parents=True)
        (package_root / "DEBIAN/control").write_text(
            f"Package: aico-business\nVersion: {version}-1\nArchitecture: armhf\n"
            "Maintainer: Test <apt@example.test>\nDescription: business fixture\n")
        document = package_root / "usr/share/doc/aico-business/release-manifest.json"
        document.parent.mkdir(parents=True)
        document.write_text(json.dumps(manifest))
        if runtime_config:
            system_config.stage(package_root)
        directory = self.cache / tag
        directory.mkdir(parents=True)
        path = directory / f"aico-business_{version}-1_armhf.deb"
        run("dpkg-deb", "--build", "--root-owner-group", str(package_root), str(path))
        record["assets"] = {path.name: {"size": path.stat().st_size, "sha256": repository.sha256(path)}}
        self.history.append(record)
        return declaration

    def build(self, now=None):
        return repository.build_site(self.history, REPO, self.site, self.cache, self.key, now=now)

    @contextlib.contextmanager
    def serve(self):
        handler = functools.partial(QuietHandler, directory=str(self.site))
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            yield f"http://127.0.0.1:{server.server_port}"
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def apt_fixture(self, platform, url):
        root = self.root / "device"
        declaration = root / "usr/lib/aico/platform/contract.json"
        declaration.parent.mkdir(parents=True)
        declaration.write_text(json.dumps(platform))
        apt_source.configure(root, self.key)
        source = root / "etc/apt/sources.list.d/aico-business.sources"
        source.write_text(source.read_text().replace(apt_source.DEFAULT_URL, url + "/apt")
                          .replace("Signed-By: /usr/", f"Signed-By: {root}/usr/"))
        for path in ("var/lib/apt/lists/partial", "var/cache/apt/archives/partial", "var/lib/dpkg", "etc/apt/apt.conf.d"):
            (root / path).mkdir(parents=True, exist_ok=True)
        (root / "var/lib/dpkg/status").write_text(
            "Package: aico-business\nStatus: install ok installed\nArchitecture: armhf\nVersion: 0.0.2-1\nDescription: old business\n\n"
            "Package: system-fixture\nStatus: install ok installed\nArchitecture: armhf\nVersion: 1\nDescription: old system\n")
        try:
            sandbox_user = getpass.getuser()
        except OSError:
            # Docker can run with the host's numeric UID absent from passwd.
            # APT already runs unprivileged in that case and cannot drop IDs.
            sandbox_user = "root"
        self.apt_options = ["-o", f"Dir={root}", "-o", f"Dir::State::status={root}/var/lib/dpkg/status",
                            "-o", "Dir::Etc::sourcelist=-", "-o", "APT::Architecture=armhf",
                            "-o", "Acquire::Languages=none", "-o", "APT::Update::Error-Mode=any",
                            "-o", f"APT::Sandbox::User={sandbox_user}", "-o", "Debug::NoLocking=true",
                            "-o", "Dir::Cache::pkgcache=", "-o", "Dir::Cache::srcpkgcache="]
        return root

    def apt(self, program, *args, check=True):
        return subprocess.run([program, *self.apt_options, *args], capture_output=True, text=True, check=check)

    def test_integrated_runtime_configs_upgrade_through_signed_apt(self):
        self.release("0.0.2", kind="ota", contract=1)
        device = self.release("0.0.3", kind="ota", contract=2, runtime_config=1)
        self.release("0.0.4", runtime_config=1)
        self.build()
        with self.serve() as url:
            self.apt_fixture(device, url)
            self.apt("apt-get", "update")
            upgrade = self.apt("apt-get", "--simulate", "upgrade").stdout
            self.assertIn("Inst aico-business", upgrade)
            self.assertIn("0.0.4-1", upgrade)

    def test_contract_isolation_and_only_business_upgrades(self):
        device = self.release("0.0.2", kind="ota", contract=1)
        self.release("0.0.3")
        self.release("0.0.4", "staging", "ota", 2)
        self.release("0.0.5", kind="ota", contract=3)
        self.release("0.0.6")
        suites = self.build()
        self.assertEqual(set(suites), {"dev-c1", "staging-c2", "dev-c3"})
        self.assertEqual(suites["dev-c1"], ["dev-v0.0.2", "dev-v0.0.3"])
        with self.serve() as url:
            root = self.apt_fixture(device, url)
            system = self.site / "system"
            system.mkdir()
            payload = "Package: system-fixture\nVersion: 2\nArchitecture: armhf\nFilename: unused.deb\nSize: 1\nDescription: system update\n\n"
            (system / "Packages").write_text(payload)
            (system / "Release").write_text("Origin: Debian\nLabel: Debian\nSuite: test\nCodename: test\nArchitectures: armhf\n")
            (root / "etc/apt/sources.list.d/system.list").write_text(f"deb [trusted=yes] {url}/system ./\n")
            self.apt("apt-get", "update")
            policy = self.apt("apt-cache", "policy", "aico-business", "system-fixture").stdout
            self.assertIn("Candidate: 0.0.3-1", policy)
            self.assertIn("Candidate: 1\n", policy)
            result = self.apt("apt-get", "--simulate", "upgrade").stdout
            self.assertIn("Inst aico-business", result)
            self.assertNotIn("Inst system-fixture", result)
            self.assertNotIn("0.0.6", policy)
            downloaded = self.apt("apt-get", "--print-uris", "--download-only", "--assume-yes", "install", "aico-business").stdout
            self.assertIn("pool/dev-c1/dev-v0.0.3/aico-business_0.0.3-1_armhf.deb", downloaded)

    def test_hybrid_check_refreshes_only_business_source_and_targets_business(self):
        device = self.release("0.0.2", kind="ota", contract=1)
        self.release("0.0.3")
        self.build()
        with self.serve() as url:
            root = self.apt_fixture(device, url)
            # An unrelated broken source must not participate in this check.
            (root / "etc/apt/sources.list.d/unrelated.list").write_text("deb http://127.0.0.1:1/unrelated ./\n")
            self.apt("apt-get", "-o", f"Dir::Etc::sourcelist={root}/etc/apt/sources.list.d/aico-business.sources",
                     "-o", "Dir::Etc::sourceparts=-", "-o", "APT::Get::List-Cleanup=0", "update")
            plan = self.apt("apt-get", "--simulate", "--only-upgrade", "install", "aico-business").stdout
            changed = [line.split()[1] for line in plan.splitlines() if line.startswith("Inst ")]
            self.assertEqual(changed, ["aico-business"])
            self.assertIn("0.0.3-1", plan)

    def test_signed_metadata_has_no_expiry(self):
        device = self.release("0.0.2", kind="ota", contract=1)
        self.build(now=datetime(2020, 1, 1, tzinfo=timezone.utc))
        for name in ("Release", "InRelease"):
            self.assertNotIn("Valid-Until:", (self.site / "apt/dists/dev-c1" / name).read_text())
        with self.serve() as url:
            self.apt_fixture(device, url)
            self.apt("apt-get", "update")
            self.assertIn("Candidate: 0.0.2-1", self.apt("apt-cache", "policy", "aico-business").stdout)

    def test_tampered_indexes_are_rejected(self):
        device = self.release("0.0.2", kind="ota", contract=1)
        self.build()
        for path in (self.site / "apt/dists/dev-c1/main/binary-armhf").rglob("*"):
            if path.is_file():
                data = path.read_bytes()
                path.write_bytes(bytes([data[0] ^ 1]) + data[1:])
        with self.serve() as url:
            self.apt_fixture(device, url)
            result = self.apt("apt-get", "update", check=False)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("mismatch", (result.stdout + result.stderr).lower())

    def test_untrusted_signature_is_rejected(self):
        device = self.release("0.0.2", kind="ota", contract=1)
        self.build()
        run("gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "", "--quick-generate-key",
            "Other <other@example.test>", "ed25519", "sign", "0")
        self.key.write_text(run("gpg", "--batch", "--armor", "--export", "other@example.test").stdout)
        with self.serve() as url:
            self.apt_fixture(device, url)
            result = self.apt("apt-get", "update", check=False)
            self.assertNotEqual(result.returncode, 0)
            self.assertTrue(any(word in result.stderr for word in ("NO_PUBKEY", "Missing key", "signature")))

    def test_corrupt_published_asset_is_rejected_before_signing(self):
        self.release("0.0.2", kind="ota", contract=1)
        package = next(self.cache.rglob("*.deb"))
        package.write_bytes(package.read_bytes() + b"modified")
        with self.assertRaisesRegex(ValueError, "checksum/size"):
            self.build()
        self.assertFalse(self.site.exists())

    def test_manifest_contract_mismatch_is_rejected(self):
        self.release("0.0.2", kind="ota", contract=1)
        self.history[0]["platform"]["contract"] = 2
        with self.assertRaisesRegex(ValueError, "manifest"):
            self.build()
        self.assertFalse(self.site.exists())

    def test_legacy_contract_is_excluded_without_skipping_aico_contracts(self):
        self.release("0.0.2", kind="ota", contract=1)
        self.release("0.0.3")
        for record in self.history:
            name = f"aico-business_{record['version']}-1_armhf.deb"
            record["assets"] = {name.replace("aico-", "aiden-"): record["assets"].pop(name)}
        self.release("0.0.4", kind="ota", contract=2)
        self.release("0.0.5")

        suites = self.build()
        self.assertEqual(suites, {"dev-c2": ["dev-v0.0.4", "dev-v0.0.5"]})
        self.assertFalse((self.site / "apt/dists/dev-c1").exists())
        self.assertFalse((self.site / "apt/pool/dev-c1").exists())

    def test_missing_aico_package_in_supported_contract_is_rejected(self):
        self.release("0.0.2", kind="ota", contract=1)
        self.release("0.0.3")
        for record in self.history:
            with self.subTest(tag=record["tag"]):
                assets = record["assets"]
                record["assets"] = {}
                with self.assertRaises(KeyError):
                    self.build()
                self.assertFalse(self.site.exists())
                record["assets"] = assets

    def test_legacy_only_history_cannot_publish_an_empty_site(self):
        self.release("0.0.2", kind="ota", contract=1)
        assets = self.history[0]["assets"]
        assets["aiden-business_0.0.2-1_armhf.deb"] = assets.pop("aico-business_0.0.2-1_armhf.deb")
        with self.assertRaisesRegex(ValueError, "No supported Aico releases"):
            self.build()
        self.assertFalse(self.site.exists())

    def test_legacy_history_is_validated_before_exclusion(self):
        self.release("0.0.2", kind="ota", contract=1)
        assets = self.history[0]["assets"]
        assets["aiden-business_0.0.2-1_armhf.deb"] = assets.pop("aico-business_0.0.2-1_armhf.deb")
        self.release("0.0.3", kind="ota", contract=2)
        self.history[0]["previous_tag"] = "dev-v0.0.1"
        with self.assertRaisesRegex(ValueError, "history is incomplete or forked"):
            self.build()
        self.assertFalse(self.site.exists())

    def test_retention_preserves_old_contract_suites(self):
        self.release("0.0.2", kind="ota", contract=1)
        for version in ("0.0.3", "0.0.4", "0.0.5"):
            self.release(version)
        self.release("0.0.6", kind="ota", contract=2)
        selected = repository.select_records(self.history, REPO)
        self.assertEqual([r["version"] for r in selected["dev-c1"]], ["0.0.3", "0.0.4", "0.0.5"])
        self.assertEqual([r["version"] for r in selected["dev-c2"]], ["0.0.6"])


class ConfigurationTests(unittest.TestCase):
    def test_contract_is_a_positive_json_integer(self):
        platform = {"product": "aico", "platform_id": "luckfox-rv1106", "architecture": "armhf",
                    "channel": "dev", "base_release": "dev-v0.0.2", "system_fingerprint": "a" * 64}
        self.assertEqual(apt_source.suite_for({**platform, "platform_contract": 1}), "dev-c1")
        for value in (0, -1, True, False, 1.0, "1", "1.0.0", None):
            with self.subTest(value=value), self.assertRaisesRegex(ValueError, "positive integer"):
                apt_source.suite_for({**platform, "platform_contract": value})

    def test_unmanaged_or_invalid_platform_does_not_write_sources(self):
        for platform in ({}, {"channel": "unknown"}, {"channel": "dev", "platform_contract": 0}):
            with self.subTest(platform=platform), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                path = root / "usr/lib/aico/platform/contract.json"
                path.parent.mkdir(parents=True)
                path.write_text(json.dumps(platform))
                with self.assertRaises(ValueError):
                    apt_source.configure(root, repository.PUBLIC_KEY)
                self.assertFalse((root / "etc/apt").exists())


if __name__ == "__main__":
    unittest.main()
