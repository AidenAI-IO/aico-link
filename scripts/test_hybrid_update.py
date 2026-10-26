#!/usr/bin/env python3
"""Run the real hybrid worker with isolated OTA/APT processes and boot state."""

import fcntl
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
WORKER = ROOT / "overlay-debian/usr/lib/aico/aico-hybrid-update"
MOCK = '''#!/usr/bin/env python3
import json, os, sys, time
from pathlib import Path
root = Path(os.environ["HYBRID_TEST_ROOT"])
args = sys.argv[1:]
name = Path(sys.argv[0]).name
with (root / "events").open("a") as stream:
    stream.write(json.dumps([name, *args])+"\\n")
config = json.loads((root / "scenario").read_text())
if config.get("delay_command") in args:
    time.sleep(config.get("delay_seconds", 5))
if name == "apt-get" and args[-1] == "update":
    attempts = sum(1 for line in (root / "events").read_text().splitlines()
                   if json.loads(line)[0] == "apt-get" and json.loads(line)[-1] == "update")
    if attempts <= config.get("fail_update_attempts", 0):
        sys.exit(4)
if config.get("fail") == name or config.get("fail") in args:
    print("test command failure", file=sys.stderr)
    sys.exit(4)
if name == "ota":
    if "check" in args:
        print(json.dumps(config.get("response", {"available":config["ota"], "version":"dev-v0.0.2", "build_time":"2026-09-30T01:00:00Z"})))
    else:
        (root / "state.json").write_text(json.dumps({"phase":"pending-reboot"}))
        print('{"Updated":true}')
elif "--simulate" in args:
    print(config.get("plan", "Inst aico-business [0.0.1-1] (0.0.2-1 Aico [armhf])" if config["package"] else "0 upgraded"))
'''


class HybridUpdateTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for command in ("ota", "apt-get"):
            file = self.root / command
            file.write_text(MOCK)
            file.chmod(0o755)
        (self.root / "config.json").write_text("{}")
        (self.root / "business.sources").write_text("test source")
        self.marker = self.root / "pending"
        self.marker.write_text('{"phase":"requested"}')
        (self.root / "boot-id").write_text("boot-before")
        self.env = dict(os.environ, HYBRID_TEST_ROOT=str(self.root),
                        AICO_HYBRID_UPDATE_MARKER=str(self.marker),
                        AICO_CONFIG_WEB_OTA_UPDATE_LOCK=str(self.root / "lock"),
                        AICO_OTA_STATE_PATH=str(self.root / "state.json"),
                        AICO_BOOT_ID_PATH=str(self.root / "boot-id"),
                        AICO_OTA_CONFIG=str(self.root / "config.json"),
                        AICO_BUSINESS_APT_SOURCE=str(self.root / "business.sources"),
                        AICO_MAINTENANCE_LOCK=str(self.root / "maintenance.lock"),
                        AICO_OTA_BIN=str(self.root / "ota"),
                        AICO_APT_GET_BIN=str(self.root / "apt-get"))
        # Exercise real subprocess timeouts without waiting for the production
        # ten-minute budget or DHCP backoff in each failure test.
        self.command = [sys.executable, "-c", """
import runpy, sys
worker = runpy.run_path(sys.argv[1])
worker["main"].__globals__.update(METADATA_TIMEOUT=1, APT_UPDATE_RETRY_BACKOFF=0.01)
sys.argv = [sys.argv[1]]
sys.exit(worker["main"]())
""", str(WORKER)]
        self.scenario(ota=False, package=False)

    def scenario(self, **scenario):
        (self.root / "scenario").write_text(json.dumps(scenario))

    def run_worker(self, code=0):
        result = subprocess.run(self.command, env=self.env, text=True, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, code, result.stdout+result.stderr)
        return result.stdout

    def events(self):
        path = self.root / "events"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def test_up_to_date_never_installs_or_reboots(self):
        log = self.run_worker()
        self.assertIn("hybrid: up-to-date", log)
        self.assertEqual(len(self.events()), 3)  # metadata, apt index, apt simulation
        self.assertNotIn(["ota", "update"], self.events())
        self.assertFalse(any("-y" in event for event in self.events()))
        self.assertFalse(self.marker.exists())

    def test_package_only_refreshes_business_source_and_upgrades_only_business(self):
        self.scenario(ota=False, package=True)
        log = self.run_worker()
        self.assertIn("hybrid: completed", log)
        commands = self.events()
        self.assertEqual(sum(event[0] == "ota" for event in commands), 1)
        self.assertIn("Dir::Etc::sourcelist="+str(self.root / "business.sources"), commands[1])
        self.assertIn("Dir::Etc::sourceparts=-", commands[1])
        self.assertIn("APT::Get::List-Cleanup=0", commands[1])
        self.assertIn("--only-upgrade", commands[-1])
        self.assertEqual(commands[-1][-2:], ["install", "aico-business"])
        self.assertFalse(self.marker.exists())

    def test_unrelated_package_does_not_trigger_business_upgrade(self):
        self.scenario(ota=False, package=False, plan="Inst unrelated [1] (2 Debian)\n")
        log = self.run_worker(code=1)
        self.assertIn("would change other packages", log)
        self.assertFalse(any("-y" in event for event in self.events()))

    def test_ota_runs_before_apt_and_packages_resume_only_after_matching_commit(self):
        self.scenario(ota=True, package=True)
        self.assertIn("waiting-reboot", self.run_worker())
        self.assertTrue(all(event[0] == "ota" for event in self.events()))
        self.assertIn("--expected-version", self.events()[-1])
        self.assertTrue(self.marker.exists())
        # Starting the service again on the original boot must not run apt.
        self.run_worker()
        self.assertEqual(len(self.events()), 2)
        (self.root / "boot-id").write_text("boot-after")
        (self.root / "state.json").write_text(json.dumps({"phase":"committed", "current_version":"dev-v0.0.2", "current_build_time":"2026-09-30T01:00:00Z"}))
        log = self.run_worker()
        self.assertIn("hybrid: ota-committed", log)
        self.assertIn("hybrid: completed", log)
        self.assertEqual(sum(event[0] == "ota" for event in self.events()), 2)
        self.assertFalse(self.marker.exists())

    def test_rollback_or_wrong_commit_never_installs_packages(self):
        for phase in ("rolled-back", "pending-reboot", "committed"):
            with self.subTest(phase=phase):
                self.marker.write_text(json.dumps({"phase":"post-ota", "boot_id":"other-boot", "version":"dev-v0.0.2", "build_time":"2026-09-30T01:00:00Z"}))
                (self.root / "state.json").write_text(json.dumps({"phase":phase, "current_version":"dev-v0.0.1"}))
                self.assertIn("package upgrade skipped", self.run_worker(code=1))
                self.assertEqual(self.events(), [])
                self.assertFalse(self.marker.exists())

    def test_command_failures_are_not_reported_as_up_to_date(self):
        for failure in ("ota", "update", "--simulate", "-y"):
            with self.subTest(failure=failure):
                self.marker.write_text('{"phase":"requested"}')
                self.scenario(ota=False, package=True, fail=failure)
                log = self.run_worker(code=1)
                self.assertIn("update_exited exit_code=1", log)
                self.assertNotIn("hybrid: up-to-date", log)
                self.assertFalse(self.marker.exists())

    def test_metadata_timeouts_fail_and_release_all_locks(self):
        for command in ("check", "update", "--simulate"):
            with self.subTest(command=command):
                self.marker.write_text('{"phase":"requested"}')
                self.scenario(ota=False, package=True, delay_command=command)
                log = self.run_worker(code=1)
                self.assertIn("timed out", log)
                self.assertIn("update_exited exit_code=1", log)
                self.assertFalse(self.marker.exists())
                self.assertFalse(any("-y" in event for event in self.events()))
                for name in ("lock", "maintenance.lock", "update.lock"):
                    with (self.root / name).open("a") as lock:
                        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)

    def test_installation_commands_are_not_subject_to_metadata_timeout(self):
        self.scenario(ota=True, package=False, delay_command="update", delay_seconds=1.2)
        self.assertIn("waiting-reboot", self.run_worker())
        self.marker.write_text('{"phase":"requested"}')
        self.scenario(ota=False, package=True, delay_command="-y", delay_seconds=1.2)
        self.assertIn("hybrid: completed", self.run_worker())

    def test_post_ota_waits_for_apt_source_before_installing(self):
        self.marker.write_text(json.dumps({"phase":"post-ota", "boot_id":"other-boot",
                                          "version":"dev-v0.0.2", "build_time":"2026-09-30T01:00:00Z"}))
        (self.root / "state.json").write_text(json.dumps({"phase":"committed",
                "current_version":"dev-v0.0.2", "current_build_time":"2026-09-30T01:00:00Z"}))
        self.scenario(ota=False, package=True, fail_update_attempts=2)
        log = self.run_worker()
        self.assertEqual(log.count("apt metadata retry"), 2)
        self.assertEqual(sum(event[-1] == "update" for event in self.events()), 3)
        self.assertEqual(self.events()[-1][-2:], ["install", "aico-business"])
        self.assertIn("hybrid: completed", log)

    def test_apt_retries_are_bounded_and_never_install_after_failure(self):
        self.scenario(ota=False, package=True, fail_update_attempts=100)
        self.run_worker(code=1)
        self.assertEqual(sum(event[-1] == "update" for event in self.events()), 3)
        self.assertFalse(any("--simulate" in event or "-y" in event for event in self.events()))
        self.assertFalse(self.marker.exists())

    def test_invalid_marker_fields_fail_with_context_before_commands(self):
        for marker, error in [(None, "expected an object"),
                              ({}, "phase must be a non-empty string"),
                              ({"phase":"post-ota", "boot_id":"other-boot"}, "version must be a non-empty string"),
                              ({"phase":"post-ota", "boot_id":"other-boot", "version":"v2", "build_time":42}, "build_time must be a non-empty string")]:
            with self.subTest(marker=marker):
                self.marker.write_text(json.dumps(marker))
                self.assertIn(error, self.run_worker(code=1))
                self.assertEqual(self.events(), [])
                self.assertFalse(self.marker.exists())

    def test_invalid_ota_release_fields_never_reach_install(self):
        for response in (None, {"available":True}, {"available":True, "version":"v2", "build_time":False}):
            with self.subTest(response=response):
                self.marker.write_text('{"phase":"requested"}')
                self.scenario(ota=True, package=True, response=response)
                self.assertIn("Invalid OTA check response", self.run_worker(code=1))
                self.assertTrue(all(event[-1] == "check" for event in self.events()))
                self.assertFalse(self.marker.exists())

    def test_no_marker_is_idle_and_interrupted_package_does_not_auto_retry(self):
        self.marker.unlink()
        self.run_worker()
        self.assertEqual(self.events(), [])
        self.marker.write_text('{"phase":"packages"}')
        self.assertIn("Package upgrade was interrupted", self.run_worker(code=1))
        self.assertEqual(self.events(), [])

    def test_worker_waits_for_api_lock_then_consumes_marker(self):
        with (self.root / "lock").open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            worker = subprocess.Popen(self.command, env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                with self.assertRaises(subprocess.TimeoutExpired):
                    worker.wait(timeout=0.15)
                self.assertEqual(self.events(), [])
                fcntl.flock(lock, fcntl.LOCK_UN)
                stdout, stderr = worker.communicate(timeout=10)
                self.assertEqual(worker.returncode, 0, stdout+stderr)
            finally:
                if worker.poll() is None: worker.kill()
                worker.communicate()


if __name__ == "__main__":
    unittest.main()
