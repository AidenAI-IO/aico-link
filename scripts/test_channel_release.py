#!/usr/bin/env python3
"""Exercise release decisions with Git histories and real Debian package fixtures."""

import copy
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts/release"))
import contract
import plan
import release
sys.path.insert(0, str(ROOT / "scripts/debian-package"))
import system_config

REPO = "AidenAI-IO/aiden-firmware"


class GitFixture(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.git("init", "-q")
        self.git("config", "user.name", "Release Test")
        self.git("config", "user.email", "release@example.test")
        self.commit({"src/agent/main.go": "initial", "overlay-debian/etc/config": "initial"})
        self.history = []

    def git(self, *args):
        return plan.command("git", *args, cwd=self.root)

    def commit(self, files):
        for name, value in files.items():
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            if value is None:
                path.unlink()
            else:
                path.write_text(value)
        self.git("add", "-A")
        self.git("commit", "-qm", "test: update source")

    def make(self, channel="dev", **kwargs):
        return plan.make_plan(self.root, REPO, channel, self.history, **kwargs)

    def publish(self, channel="dev", **kwargs):
        record = self.make(channel, **kwargs)
        self.history.append(record)
        return record


class PlannerTests(GitFixture):
    def test_first_release_establishes_contract(self):
        record = self.make()
        self.assertEqual((record["kind"], record["version"], record["platform"]["contract"]), ("ota", "0.0.2", 1))
        self.assertIs(type(record["platform"]["contract"]), int)
        self.assertEqual(plan.environment(record)["AIDEN_PLATFORM_CONTRACT"], "1")
        self.assertEqual(record["platform"]["base_release"], record["tag"])

    def test_release_contract_rejects_non_positive_or_non_integer_values(self):
        for value in (0, -1, True, False, 1.0, "1", "1.0.0", None):
            with self.subTest(value=value):
                record = self.make()
                record["platform"]["contract"] = value
                with self.assertRaisesRegex(ValueError, "positive integer"):
                    plan.validate_record(record)

    def test_business_after_system_inherits_latest_base(self):
        self.publish()
        self.commit({"overlay-debian/etc/config": "new kernel services"})
        system = self.publish()
        self.commit({"src/agent/main.go": "new business"})
        business = self.publish()
        self.assertEqual(system["platform"]["contract"], 2)
        self.assertEqual(business["kind"], "business")
        self.assertEqual(business["platform"], system["platform"])
        self.commit({"src/agent/main.go": "next business"})
        self.assertEqual(self.make()["platform"], system["platform"])

    def test_new_namespaced_configs_ship_without_changing_contract(self):
        base = self.publish()
        for path in ("etc/aiden/new-feature.conf", "etc/aiden_new.conf",
                     "etc/profile.d/aiden-extra.sh", "usr/lib/aiden/aiden-new-helper",
                     "etc/systemd/system/aiden-new.service", "etc/ssh/sshd_config.d/30-aiden.conf"):
            with self.subTest(path=path):
                self.commit({"overlay-debian/" + path: "new config"})
                record = self.publish()
                self.assertEqual(record["kind"], "business")
                self.assertEqual(record["platform"], base["platform"])
                self.assertEqual(record["changes"]["config"], ["overlay-debian/" + path])
        self.commit({"overlay-debian/etc/aiden/new-feature.conf": None})
        self.assertEqual(self.make()["kind"], "business")

    def test_platform_exclusions_and_boundary_changes_require_ota(self):
        self.publish()
        for path in ("usr/lib/aiden/aiden-rootfs-grow", "usr/lib/aiden/aiden-ota-health-aggregate",
                     "usr/lib/aiden/platform/lib/new.so", "etc/systemd/system/aiden-environment.service",
                     "etc/bluetooth/main.conf", "etc/apt/sources.list.d/other.sources"):
            self.commit({"overlay-debian/" + path: "platform change"})
            self.assertEqual(self.publish()["kind"], "ota")
        self.commit({"scripts/debian-system/config-package.json": "boundary"})
        self.assertEqual(self.make()["kind"], "ota")

    def test_legacy_ownership_requires_one_new_base(self):
        old = self.publish()
        del old["runtime_config"]
        updated = self.publish()
        self.assertEqual((updated["kind"], updated["platform"]["contract"]), ("ota", 2))
        self.commit({"overlay-debian/etc/aiden_new.conf": "config"})
        self.assertEqual(self.make()["kind"], "business")

    def test_channels_have_independent_baselines_and_global_numbers(self):
        dev = self.publish()
        staging = self.publish("staging")
        prod = self.publish("prod")
        self.assertEqual([r["platform"]["contract"] for r in self.history], [1, 2, 3])
        self.assertEqual(prod["version"], "0.0.4")
        self.commit({"src/agent/main.go": "changed"})
        updated = self.publish("staging")
        self.assertEqual(updated["platform"], staging["platform"])
        self.assertEqual(self.make()["previous_tag"], dev["tag"])
        self.assertEqual(self.make()["platform"], dev["platform"])

    def test_docs_tests_only_or_identical_source_do_not_publish(self):
        self.publish()
        self.assertEqual(self.make()["kind"], "none")
        self.commit({"docs/readme.md": "docs", "src/agent/foo_test.go": "test", "scripts/test_x.py": "test"})
        self.assertEqual(self.make()["kind"], "none")

    def test_repository_markdown_and_gitignore_do_not_change_release(self):
        base = self.publish()
        paths = ("scripts/UPDATE_SUMMARY.md", "src/README.md", "src/agent/README.md",
                 "CLAUDE.md", ".gitignore", "src/.gitignore")
        self.commit({path: "documentation" for path in paths})
        record = self.make()
        self.assertEqual(record["kind"], "none")
        self.assertEqual(record["fingerprints"], base["fingerprints"])
        self.assertEqual(record["changes"]["ignore"], sorted(paths))

    def test_old_fingerprint_policy_does_not_force_ota(self):
        base = self.publish()
        self.commit({".gitignore": "output/", "scripts/UPDATE_SUMMARY.md": "notes"})
        legacy = self.publish(force_ota=True)
        legacy["fingerprints"]["system"] = "a" * 64
        legacy["fingerprints"]["business"] = "b" * 64
        self.assertEqual(self.make()["kind"], "none")
        self.commit({"src/agent/main.go": "business change"})
        record = self.make()
        self.assertEqual(record["kind"], "business")
        self.assertEqual(record["platform"], legacy["platform"])
        self.assertEqual(record["fingerprints"]["system"], legacy["fingerprints"]["system"])
        plan.validate_history([base, legacy, record], REPO)

    def test_overlay_markdown_remains_system(self):
        self.publish()
        self.commit({"overlay-debian/usr/share/aiden/platform/README.md": "platform content"})
        self.assertEqual(self.make()["kind"], "ota")

    def test_overlay_gitignore_is_shipped(self):
        self.publish()
        self.commit({"overlay-debian/etc/aiden/.gitignore": "managed config"})
        self.assertEqual(self.publish()["kind"], "business")
        self.commit({"overlay-debian/usr/share/aiden/platform/.gitignore": "platform content"})
        self.assertEqual(self.make()["kind"], "ota")

    def test_skill_markdown_is_business(self):
        self.publish()
        self.commit({"src/agent/config/skills/new/SKILL.md": "runtime skill"})
        self.assertEqual(self.make()["kind"], "business")

    def test_other_shipped_markdown_is_business(self):
        self.publish()
        self.commit({"src/config_web/web/help.md": "web content",
                     "assets/business/audio/guide.md": "audio content",
                     "src/agent/internal/agent/web_ui/help.md": "embedded web content",
                     "src/agent/config/skills/new/.gitignore": "skill payload"})
        self.assertEqual(self.make()["kind"], "business")

    def test_mixed_unknown_ota_and_dependency_changes_are_system(self):
        self.publish()
        for filename in ("new-runtime-file", "src/agent/internal/ota/github.go", "src/agent/go.mod"):
            with self.subTest(filename=filename):
                self.commit({filename: "changed", "src/agent/main.go": filename})
                self.assertEqual(self.publish()["kind"], "ota")

    def test_renaming_or_deleting_system_file_still_requires_ota(self):
        self.publish()
        self.git("mv", "overlay-debian/etc/config", "src/moved")
        self.git("commit", "-qm", "test: move system file")
        self.assertEqual(self.make()["kind"], "ota")
        self.assertIn("overlay-debian/etc/config", self.make()["changes"]["system"])

    def test_force_ota_refreshes_unchanged_external_dependencies(self):
        self.publish()
        self.assertEqual(self.make(force_ota=True)["kind"], "ota")
        self.assertEqual(self.make(force_ota=True)["platform"]["contract"], 2)

    def test_cannot_release_older_ancestor(self):
        old = self.git("rev-parse", "HEAD")
        self.commit({"src/agent/main.go": "next"})
        self.publish()
        for force_ota in (False, True):
            with self.subTest(force_ota=force_ota), self.assertRaisesRegex(ValueError, "older than"):
                self.make(ref=old, force_ota=force_ota)

    def squash_published_branch(self):
        base = self.git("rev-parse", "HEAD")
        self.git("checkout", "-qb", "release-candidate")
        self.commit({"overlay-debian/etc/config": "released system", "src/agent/main.go": "released business"})
        previous = self.publish()
        self.git("checkout", "-qb", "squashed-main", base)
        self.git("merge", "--squash", "release-candidate")
        self.git("commit", "-qm", "feat: squash released changes")
        self.assertNotEqual(self.git("rev-parse", "HEAD"), previous["source_commit"])
        return previous

    def test_squash_with_identical_tree_does_not_release(self):
        previous = self.squash_published_branch()
        record = self.make()
        self.assertEqual(record["kind"], "none")
        self.assertEqual(record["source_tree"], previous["source_tree"])
        self.assertEqual(record["platform"], previous["platform"])
        self.assertEqual(record["previous_commit"], previous["source_commit"])
        self.assertTrue(all(not paths for paths in record["changes"].values()))
        self.commit({"docs/readme.md": "documentation after squash"})
        self.assertEqual(self.make()["kind"], "none")

    def test_business_after_squash_compares_published_tree(self):
        previous = self.squash_published_branch()
        self.commit({"src/agent/main.go": "business after squash"})
        record = self.make()
        self.assertEqual(record["kind"], "business")
        self.assertEqual(record["platform"], previous["platform"])
        self.assertEqual(record["previous_tag"], previous["tag"])
        self.assertEqual(record["changes"]["business"], ["src/agent/main.go"])
        self.assertEqual(record["changes"]["system"], [])
        self.assertIn(f"/compare/{previous['tag']}..{record['tag']})", release.notes(record))
        release.assert_current(record, self.history)

    def test_system_removal_after_squash_requires_new_contract(self):
        previous = self.squash_published_branch()
        self.commit({"overlay-debian/etc/config": None})
        record = self.make()
        self.assertEqual(record["kind"], "ota")
        self.assertEqual(record["previous_commit"], previous["source_commit"])
        self.assertEqual(record["platform"]["contract"], 2)
        self.assertEqual(record["changes"]["system"], ["overlay-debian/etc/config"])
        self.assertEqual(record["ota_requirement"], {"commit": record["source_commit"], "more": False})
        release.assert_current(record, self.history)

    def test_rebased_release_with_only_docs_changes_does_not_release(self):
        base = self.git("rev-parse", "HEAD")
        self.git("checkout", "-qb", "release-candidate")
        self.commit({"src/agent/main.go": "released business"})
        previous = self.publish()
        self.git("checkout", "-qb", "new-base", base)
        self.commit({"docs/readme.md": "new main documentation"})
        self.git("checkout", "release-candidate")
        self.git("rebase", "new-base")
        self.assertNotEqual(self.git("rev-parse", "HEAD"), previous["source_commit"])
        record = self.make()
        self.assertEqual(record["kind"], "none")
        self.assertEqual(record["changes"]["ignore"], ["docs/readme.md"])

    def shallow_clone(self, previous):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        clone = Path(temporary.name) / "clone"
        self.git("clone", "--quiet", "--depth=1", "--no-tags", self.root.as_uri(), str(clone))
        self.root = clone
        self.assertEqual(self.git("rev-parse", "--is-shallow-repository"), "true")
        published = subprocess.run(["git", "cat-file", "-e", previous["source_commit"] + "^{commit}"],
                                   cwd=self.root, capture_output=True)
        self.assertNotEqual(published.returncode, 0)

    def test_shallow_squash_fetches_history_before_comparing_released_tree(self):
        previous = self.squash_published_branch()
        self.commit({"src/agent/main.go": "business after squash"})
        self.shallow_clone(previous)
        commit = self.git("rev-parse", "HEAD")
        record = self.make()
        self.assertEqual(self.git("cat-file", "-t", previous["source_commit"]), "commit")
        self.assertEqual(self.git("rev-parse", "--is-shallow-repository"), "false")
        self.assertEqual(record["source_commit"], commit)
        self.assertEqual(record["kind"], "business")
        self.assertEqual(record["platform"], previous["platform"])
        self.assertEqual(record["changes"]["business"], ["src/agent/main.go"])
        self.assertEqual(record["changes"]["system"], [])

    def test_shallow_fetch_failure_is_not_reported_as_unrelated_history(self):
        previous = self.squash_published_branch()
        self.shallow_clone(previous)
        self.git("remote", "set-url", "origin", str(self.root / "missing-remote"))
        with self.assertRaises(subprocess.CalledProcessError) as raised:
            self.make()
        self.assertEqual(raised.exception.cmd[:3], ("git", "fetch", "--unshallow"))
        self.assertEqual(self.git("rev-parse", "--is-shallow-repository"), "true")

    def test_shallow_unrelated_history_is_rejected_after_fetch(self):
        previous = self.publish()
        self.git("checkout", "--orphan", "unrelated")
        self.commit({"src/agent/main.go": "unrelated business"})
        self.shallow_clone(previous)
        with self.assertRaisesRegex(ValueError, "unrelated"):
            self.make()
        self.assertEqual(self.git("rev-parse", "--is-shallow-repository"), "false")

    def test_unrelated_history_is_rejected(self):
        self.publish()
        self.git("checkout", "--orphan", "unrelated")
        self.commit({"src/agent/main.go": "unrelated business"})
        for force_ota in (False, True):
            with self.subTest(force_ota=force_ota), self.assertRaisesRegex(ValueError, "unrelated"):
                self.make(force_ota=force_ota)

    def test_versions_must_increase_across_channels(self):
        self.publish(version="1.2.3")
        with self.assertRaises(ValueError):
            self.make("prod", version="1.2.3")
        self.assertEqual(self.make("prod")["version"], "1.2.4")

    def test_invalid_or_missing_business_base_fails_closed(self):
        self.publish()
        self.commit({"src/agent/main.go": "next"})
        business = self.publish()
        with self.assertRaises(ValueError):
            plan.validate_history([business], REPO)
        bad = copy.deepcopy(self.history)
        bad[-1]["platform"]["base_release"] = "prod-v0.0.2"
        with self.assertRaises(ValueError):
            plan.validate_history(bad, REPO)

    def test_stale_plan_cannot_publish(self):
        old_plan = self.make()
        self.publish("prod")
        with self.assertRaisesRegex(ValueError, "history changed"):
            release.assert_current(old_plan, self.history)

    def test_first_release_cannot_be_changed_to_business_before_publication(self):
        record = self.make()
        record["kind"] = "business"
        with self.assertRaises(ValueError):
            release.assert_current(record, [])

    def test_new_ota_time_is_strictly_increasing(self):
        first = self.publish()
        second = self.make(force_ota=True)
        self.assertGreater(second["build_time"], first["build_time"])

    def test_github_history_ignores_drafts_and_legacy_but_rejects_missing_record(self):
        entries = [{"draft": False, "tag_name": "debian-old"},
                   {"draft": True, "tag_name": "dev-v0.0.2"}]
        with patch.object(plan, "github_releases", return_value=entries):
            self.assertEqual(plan.github_history(REPO), [])
        entries.append({"draft": False, "tag_name": "prod-v0.0.3", "assets": []})
        with patch.object(plan, "github_releases", return_value=entries):
            with self.assertRaisesRegex(ValueError, "release.json"):
                plan.github_history(REPO)

    def test_platform_change_does_not_leak_into_other_channel(self):
        self.publish()
        self.publish("staging")
        self.commit({"overlay-debian/etc/config": "v2"})
        self.publish("dev")
        stage = self.make("staging")
        self.assertEqual(stage["kind"], "ota")
        self.assertEqual(stage["platform"]["contract"], 4)


class ReleaseNotesTests(GitFixture):
    def assert_notice(self, record, commit, more=False):
        suffix = " (and more)" if more else ""
        text = release.notes(record)
        self.assertIn(f"Commit [`{commit[:8]}`](https://github.com/{REPO}/commit/{commit})"
                      f"{suffix} requires OTA update.\n", text)
        self.assertEqual(text.count("requires OTA update."), 1)
        self.assertEqual("(and more)" in text, more)

    def test_notes_keep_commits_and_comparison_without_file_lists(self):
        previous = self.publish()
        self.commit({"src/agent/main.go": "business", "overlay-debian/etc/aiden/new.conf": "config",
                     "docs/readme.md": "docs"})
        business = self.publish()
        self.commit({"overlay-debian/etc/config": "system"})
        ota = self.publish()
        for record in (business, ota, self.make()):
            with self.subTest(kind=record["kind"]):
                text = release.notes(record)
                for paths in record["changes"].values():
                    for path in paths:
                        self.assertNotIn(path, text)
                for kind in record["changes"]:
                    self.assertNotIn(f"## {kind.title()}", text)
                for commit in record["commits"]:
                    self.assertIn(f"- {commit}\n", text)
                if record["kind"] != "ota":
                    self.assertNotIn("requires OTA update", text)
        self.assertIn(f"/compare/{previous['tag']}..{business['tag']})", release.notes(business))
        self.assert_notice(ota, ota["source_commit"])

    def test_first_system_commit_with_more_ignores_business_config_and_docs(self):
        self.publish()
        self.commit({"src/agent/main.go": "business", "overlay-debian/etc/aiden/new.conf": "config"})
        self.commit({"overlay-debian/etc/config": "system one"})
        first = self.git("rev-parse", "HEAD")
        self.commit({"overlay-debian/etc/config": "system two"})
        self.commit({"docs/readme.md": "docs"})
        self.assert_notice(self.make(), first, more=True)

    def test_system_commit_outside_displayed_log_is_still_reported(self):
        self.publish()
        self.commit({"overlay-debian/etc/config": "system"})
        first = self.git("rev-parse", "HEAD")
        for _ in range(101):
            self.git("commit", "--allow-empty", "-qm", "docs: no runtime changes")
        record = self.make()
        self.assertEqual(len(record["commits"]), 100)
        self.assertFalse(any(c.startswith(first[:7]) for c in record["commits"]))
        self.assert_notice(record, first)

    def test_rename_from_system_to_business_is_reported(self):
        self.publish()
        self.git("mv", "overlay-debian/etc/config", "src/moved")
        self.git("commit", "-qm", "refactor: move system file")
        first = self.git("rev-parse", "HEAD")
        self.commit({"src/agent/main.go": "business"})
        self.assert_notice(self.make(), first)

    def test_reverted_system_path_does_not_count_as_an_ota_reason(self):
        self.publish()
        self.commit({"temporary-system-file": "temporary"})
        self.commit({"temporary-system-file": None})
        self.commit({"overlay-debian/etc/config": "system"})
        self.assert_notice(self.make(), self.git("rev-parse", "HEAD"))

    def test_merge_commit_with_system_resolution_is_reported(self):
        self.publish()
        main = self.git("branch", "--show-current")
        self.git("checkout", "-qb", "business-branch")
        self.commit({"src/agent/main.go": "business"})
        self.git("checkout", main)
        self.commit({"docs/readme.md": "docs"})
        self.git("merge", "--no-ff", "--no-commit", "business-branch")
        self.commit({"overlay-debian/etc/config": "merge system change"})
        self.assert_notice(self.make(), self.git("rev-parse", "HEAD"))

    def test_first_release_and_forced_or_migration_ota_have_a_notice(self):
        first = self.publish()
        self.assert_notice(first, first["source_commit"])
        self.assert_notice(self.make(force_ota=True), first["source_commit"])
        del first["runtime_config"]
        self.assert_notice(self.make(), first["source_commit"])

    def test_older_plan_without_ota_attribution_can_still_render(self):
        record = self.make()
        del record["ota_requirement"]
        self.assert_notice(record, record["source_commit"])


class ContractTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.env = {**os.environ, "AIDEN_PLATFORM_CONTRACT": "7", "AIDEN_PLATFORM_BASE": "dev-v0.0.9",
                    "AIDEN_RELEASE_CHANNEL": "dev", "AIDEN_SYSTEM_FINGERPRINT": "a" * 64,
                    "AIDEN_BUSINESS_VERSION": "0.0.10", "AIDEN_BUSINESS_REVISION": "1"}

    def test_package_and_rootfs_share_the_contract(self):
        with patch.dict(os.environ, self.env):
            platform = contract.declaration()
            manifest = contract.package_manifest()
        self.assertEqual(platform, manifest["platform"])
        self.assertEqual(manifest["required_platform_contract"], {"min": 7, "max_exclusive": 8})
        self.assertIs(type(platform["platform_contract"]), int)

    def test_default_contract_is_integer_one(self):
        with patch.dict(os.environ, {**self.env, "AIDEN_PLATFORM_CONTRACT": ""}):
            platform = contract.declaration()
            manifest = contract.package_manifest()
        self.assertIs(type(platform["platform_contract"]), int)
        self.assertEqual(platform["platform_contract"], 1)
        self.assertEqual(manifest["required_platform_contract"], {"min": 1, "max_exclusive": 2})

    def test_contract_environment_requires_canonical_positive_integer(self):
        for value in ("0", "-1", "1.0.0", "1.0", "01", "+1", "true", " 1"):
            with self.subTest(value=value), patch.dict(os.environ, {**self.env, "AIDEN_PLATFORM_CONTRACT": value}):
                with self.assertRaisesRegex(ValueError, "positive integer"):
                    contract.declaration()

    def test_preinst_checks_contract_before_any_service_operation(self):
        subprocess.run(["bash", str(ROOT / "scripts/debian-package/write-maintainer-scripts.sh"),
                        str(self.root / "DEBIAN")], env=self.env, check=True)
        contract_path = self.root / "usr/lib/aiden/platform/contract.json"
        with patch.dict(os.environ, self.env):
            platform = contract.declaration()
        release.write_json(contract_path, platform)
        env = {**self.env, "DPKG_ROOT": str(self.root), "SYSTEMD_OFFLINE": "1"}
        script = str(self.root / "DEBIAN/preinst")
        subprocess.run([script, "install"], check=True, env=env)
        for field, value in (("platform_contract", 6), ("platform_contract", 7.0),
                             ("platform_contract", "7"), ("platform_contract", "7.0.0"),
                             ("runtime_config", 0), ("channel", "prod"),
                             ("base_release", "dev-v0.0.8"), ("system_fingerprint", "b" * 64)):
            with self.subTest(field=field):
                release.write_json(contract_path, {**platform, field: value})
                result = subprocess.run([script, "upgrade"], env=env, capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("incompatible platform", result.stderr)
        contract_path.unlink()
        self.assertNotEqual(subprocess.run([script, "install"], env=env, capture_output=True).returncode, 0)


@unittest.skipUnless(shutil.which("dpkg-deb"), "real package checks require Linux dpkg-deb")
class AssetTests(GitFixture):
    def setUp(self):
        super().setUp()
        self.record = self.make()
        self.env = {**os.environ, **plan.environment(self.record)}
        self.assets = self.root / "assets"
        self.assets.mkdir()

    def create_assets(self, kind="business"):
        if kind == "business":
            self.history.append(self.record)
            self.commit({"src/agent/main.go": "business change"})
        record = self.make()
        self.env.update(plan.environment(record))
        package_root = self.root / "package"
        control = package_root / "DEBIAN/control"
        control.parent.mkdir(parents=True)
        control.write_text(f"Package: aiden-business\nVersion: {record['version']}-1\nArchitecture: armhf\n"
                           "Maintainer: Test <test@example.test>\nDescription: test package\n")
        manifest_path = package_root / "usr/share/doc/aiden-business/release-manifest.json"
        with patch.dict(os.environ, self.env):
            manifest = contract.package_manifest()
        release.write_json(manifest_path, manifest)
        system_config.stage(package_root)
        name = f"aiden-business_{record['version']}-1_armhf.deb"
        subprocess.run(["dpkg-deb", "--build", "--root-owner-group", str(package_root), str(self.assets / name)],
                       check=True, stdout=subprocess.DEVNULL)
        shutil.copyfile(manifest_path, self.assets / "release-manifest.json")
        release.write_json(self.assets / "platform-contract.json", manifest["platform"])
        (self.assets / "RELEASE-NOTES.md").write_text(release.notes(record))
        if kind == "ota":
            images = self.root / "images"
            images.mkdir()
            for name in ("boot_a.img", "boot_b.img", "rootfs.img", "update.img"):
                (images / name).write_bytes((name * 20).encode())
            subprocess.run(["bash", str(ROOT / "scripts/compress_release_images.sh"), "--image-dir", str(images),
                            "--assets", "boot_a.img boot_b.img rootfs.img update.img", "--output", "/dev/null"], check=True, stdout=subprocess.DEVNULL)
            key = self.root / "key.pem"
            subprocess.run(["openssl", "genpkey", "-algorithm", "ED25519", "-out", str(key)], check=True)
            subprocess.run(["openssl", "pkey", "-in", str(key), "-pubout", "-out", str(self.assets / "ota-public-key.pem")], check=True)
            subprocess.run(["bash", str(ROOT / "scripts/generate_ota_manifest.sh"), "--version", record["tag"],
                            "--channel", record["channel"], "--build-time", record["build_time"],
                            "--sign-key", str(key), "--image-dir", str(images), "--base-url", self.env["OTA_BASE_URL"],
                            "--output", str(self.assets / "manifest.json")], check=True, stdout=subprocess.DEVNULL)
            for name in release.IMAGE_ASSETS - {"manifest.json", "ota-public-key.pem", "update.img.sha256"}:
                shutil.copyfile(images / name, self.assets / name)
            (self.assets / "update.img.sha256").write_text(release.file_hash(images / "update.img") + "  update.img\n")
        self.record = record
        self.seal()
        return record

    def seal(self):
        self.record["assets"] = {p.name: {"size": p.stat().st_size, "sha256": release.file_hash(p)}
                                  for p in self.assets.iterdir() if p.name not in ("release.json", "SHA256SUMS")}
        release.write_json(self.assets / "release.json", self.record)
        (self.assets / "SHA256SUMS").write_text("".join(
            f"{release.file_hash(p)}  {p.name}\n" for p in sorted(self.assets.iterdir()) if p.name != "SHA256SUMS"))

    def test_business_asset_verification_and_tampering(self):
        self.create_assets()
        release.verify(self.assets)
        (self.assets / "RELEASE-NOTES.md").write_text("corrupt")
        with self.assertRaisesRegex(ValueError, "modified"):
            release.verify(self.assets)

    def test_package_binding_cannot_be_changed_by_rehashing_assets(self):
        self.create_assets()
        platform = release.read_json(self.assets / "platform-contract.json")
        for value in (999, True, 1.0, "1", "1.0.0"):
            with self.subTest(contract=value):
                release.write_json(self.assets / "platform-contract.json", {**platform, "platform_contract": value})
                self.seal()
                with self.assertRaisesRegex(ValueError, "binding"):
                    release.verify(self.assets)

    def test_signed_ota_and_corrupted_signature(self):
        self.create_assets("ota")
        release.verify(self.assets)
        manifest = release.read_json(self.assets / "manifest.json")
        manifest["signature"]["value"] = "00" * 64
        release.write_json(self.assets / "manifest.json", manifest)
        self.seal()
        with self.assertRaises(subprocess.CalledProcessError):
            release.verify(self.assets)

    def exercise_publish(self, corrupt=False, stale=False, draft=False, conflicting_draft=False, wrong_tag=False):
        self.create_assets()
        calls = []
        original_command = release.command
        def command(*args, **kwargs):
            if args[0] != "gh":
                return original_command(*args, **kwargs)
            calls.append(args)
            request = " ".join(args)
            if "matching-refs" in request:
                return json.dumps([{"ref": "refs/tags/" + self.record["tag"]}]) if draft or wrong_tag else "[]"
            if "/commits/" in request:
                return "0" * 40 if wrong_tag else self.record["source_commit"]
            if "/releases/assets/" in request:
                record = {**self.record, "build_time": "2000-01-01T00:00:00Z"} if conflicting_draft else self.record
                return json.dumps(record)
            return ""
        def transport(*args):
            calls.append(("gh", *args))
            if args[1] == "download":
                directory = Path(args[args.index("--dir") + 1])
                for asset in self.assets.iterdir():
                    shutil.copyfile(asset, directory / asset.name)
                if corrupt:
                    (directory / "RELEASE-NOTES.md").write_text("corrupt")
        histories = [self.history, [*self.history, self.record]] if stale else [self.history, self.history]
        existing = [{"tag_name": self.record["tag"], "draft": True,
                     "assets": [{"name": "release.json", "id": 1}]}] if draft else []
        fails = corrupt or stale or conflicting_draft or wrong_tag
        with patch.object(release, "command", side_effect=command), patch.object(release, "gh_retry", side_effect=transport), \
             patch.object(release, "github_history", side_effect=histories), patch.object(release, "github_releases", return_value=existing):
            if fails:
                with self.assertRaises(ValueError):
                    release.publish(self.assets)
            else:
                release.publish(self.assets)
        edits = [call for call in calls if call[:3] == ("gh", "release", "edit")]
        self.assertEqual(len(edits), 0 if fails else 1)
        if draft:
            self.assertFalse(any(call[:3] == ("gh", "release", "create") for call in calls))
        if edits:
            self.assertIn("--prerelease=true", edits[0])
            self.assertIn("--latest=false", edits[0])

    def test_publish_verifies_uploaded_assets_before_finalizing(self):
        self.exercise_publish()

    def test_corrupt_download_keeps_draft(self):
        self.exercise_publish(corrupt=True)

    def test_new_release_during_upload_keeps_draft(self):
        self.exercise_publish(stale=True)

    def test_identical_draft_can_resume(self):
        self.exercise_publish(draft=True)

    def test_draft_from_different_build_cannot_be_overwritten(self):
        self.exercise_publish(draft=True, conflicting_draft=True)

    def test_tag_from_different_commit_cannot_be_overwritten(self):
        self.exercise_publish(wrong_tag=True)


if __name__ == "__main__":
    unittest.main()
