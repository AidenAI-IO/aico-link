#!/usr/bin/env python3
"""Exercise package service transitions with a stateful systemd stand-in.

Only filesystem roots are relocated in generated maintainer scripts. The dpkg
tests also exercise real unpack/configure/abort ordering in an isolated root.
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
WATCHER = 'aiden-wifi-proxy-agent-restart.path'
RESTART = 'aiden-wifi-proxy-agent-restart.service'
AGENT = 'aiden-agent.service'
FRAME = 'aiden-frame.service'
TTYD = 'aiden-ttyd.service'
UNITS = [WATCHER, RESTART, AGENT, FRAME, TTYD, 'aiden-audio.service',
         'aiden-ble.service', 'aiden-config-web.service', 'aiden-wifi-proxy.service',
         'aiden-mcu-event.service']


class LifecycleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.scripts = self.root / 'scripts'
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.marker = self.root / 'systemd'
        self.marker.mkdir()
        self.state = self.root / 'service-state.json'
        self.calls = self.root / 'calls.jsonl'
        self.transaction = self.root / 'package-state/service-transition'
        self.policy = self.root / 'policy-rc.d'
        self.device = self.root / 'dpkg-root'
        self.config_transaction = self.root / 'package-state/config-transition'
        contract = self.device / 'usr/lib/aiden/platform/contract.json'
        subprocess.run(['python3', str(ROOT / 'scripts/release/contract.py'), 'platform', str(contract)], check=True)
        self.set_states({unit: ('inactive' if unit == RESTART else 'active') for unit in UNITS})
        self.env = {**os.environ, 'PATH': str(self.bin) + os.pathsep + os.environ['PATH'],
                    'MOCK_STATE': str(self.state), 'MOCK_CALLS': str(self.calls),
                    'DPKG_ROOT': '', 'SYSTEMD_OFFLINE': '0'}
        subprocess.run(['bash', str(ROOT / 'scripts/debian-package/write-maintainer-scripts.sh'), str(self.scripts)], check=True)
        for script in self.scripts.iterdir():
            script.write_text(self.relocate(script.read_text()))
        mock = self.bin / 'systemctl'
        mock.write_text('''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
args = sys.argv[1:]
path = Path(os.environ['MOCK_STATE'])
state = json.loads(path.read_text())
with open(os.environ['MOCK_CALLS'], 'a') as log:
    log.write(json.dumps(args) + '\\n')
action = args[0]
if action == 'show':
    unit = args[-1]
    if '--property=LoadState' in args:
        print('loaded' if unit in state else 'not-found')
    else:
        print(state.get(unit, 'inactive'))
elif action in ('start', 'stop'):
    for unit in args[1:]:
        if os.environ.get('MOCK_FAIL') == action + ':' + unit:
            sys.exit(1)
        state[unit] = 'active' if action == 'start' else 'inactive'
        path.write_text(json.dumps(state))
elif action != 'daemon-reload':
    sys.exit('unexpected systemctl operation: ' + repr(args))
''')
        mock.chmod(0o755)
        detector = self.bin / 'systemd-detect-virt'
        detector.write_text('#!/bin/sh\n[ "${MOCK_CHROOT:-0}" = 1 ]\n')
        detector.chmod(0o755)
        for name in ('systemd-tmpfiles', 'visudo'):
            helper = self.bin / name
            helper.write_text('#!/bin/sh\n[ "${MOCK_INVALID_SUDOERS:-0}" != 1 ]\n' if name == 'visudo' else '#!/bin/sh\nexit 0\n')
            helper.chmod(0o755)

    def relocate(self, text):
        text = text.replace('/var/lib/aiden-business', str(self.root / 'package-state'))
        text = text.replace('/run/systemd/system', str(self.marker))
        text = text.replace('/usr/sbin/policy-rc.d', str(self.policy))
        text = text.replace('ROOT = Path(os.environ.get("DPKG_ROOT") or "/")', 'ROOT = Path(' + repr(str(self.device)) + ')')
        text = text.replace("path = os.environ.get('DPKG_ROOT', '') + '/usr/lib/aiden/platform/contract.json'",
                            'path = ' + repr(str(self.device / 'usr/lib/aiden/platform/contract.json')))
        return text

    def set_states(self, states):
        self.state.write_text(json.dumps(states))

    def states(self):
        return json.loads(self.state.read_text())

    def operations(self):
        if not self.calls.exists():
            return []
        return [json.loads(line) for line in self.calls.read_text().splitlines()
                if json.loads(line)[0] != 'show']

    def run_phase(self, phase, action, success=True):
        result = subprocess.run([str(self.scripts / phase), action, '0.0.1-1', '0.0.1-2'],
                                env=self.env, capture_output=True, text=True)
        if success:
            self.assertEqual(result.returncode, 0, result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0)
        return result

    def test_upgrade_from_old_package_restores_only_running_services(self):
        before = self.states()
        before[TTYD] = 'inactive'
        self.set_states(before)
        self.run_phase('preinst', 'upgrade')  # Legacy package has no prerm.
        self.assertTrue(all(value == 'inactive' for value in self.states().values()))
        self.assertEqual(self.operations()[0], ['stop', WATCHER, RESTART])
        self.run_phase('postinst', 'configure')
        self.assertEqual(self.states(), before)
        self.assertEqual(self.operations()[-1], ['start', WATCHER])
        self.assertFalse(self.transaction.exists())

    def test_repeated_hooks_preserve_original_snapshot(self):
        before = self.states()
        for phase, action in [('prerm', 'upgrade'), ('preinst', 'upgrade'), ('postrm', 'upgrade')]:
            self.run_phase(phase, action)
        self.run_phase('postinst', 'configure')
        self.assertEqual(self.states(), before)
        count = len(self.operations())
        self.run_phase('postinst', 'configure')
        self.assertEqual(len(self.operations()), count)

    def test_abort_upgrade_restores_services(self):
        before = self.states()
        self.run_phase('prerm', 'upgrade')
        self.run_phase('postrm', 'abort-upgrade')
        self.assertEqual(self.states(), before)

    def test_remove_keeps_services_stopped_and_clears_state(self):
        self.run_phase('prerm', 'remove')
        self.run_phase('postrm', 'remove')
        self.assertTrue(all(value == 'inactive' for value in self.states().values()))
        self.assertFalse(self.transaction.exists())

    def test_abort_remove_restores_services(self):
        before = self.states()
        self.run_phase('prerm', 'remove')
        self.run_phase('postinst', 'abort-remove')
        self.assertEqual(self.states(), before)

    def test_first_install_does_not_start_previously_stopped_units(self):
        self.set_states({unit: 'inactive' for unit in UNITS})
        self.run_phase('preinst', 'install')
        self.run_phase('postinst', 'configure')
        self.assertTrue(all(value == 'inactive' for value in self.states().values()))

    def test_missing_units_are_ignored(self):
        self.set_states({AGENT: 'active'})
        self.run_phase('preinst', 'upgrade')
        self.run_phase('postinst', 'configure')
        self.assertEqual(self.states(), {AGENT: 'active'})

    def test_stop_failure_aborts_and_recovers_services(self):
        before = self.states()
        self.env['MOCK_FAIL'] = 'stop:' + FRAME
        self.run_phase('preinst', 'upgrade', success=False)
        self.assertEqual(self.states(), before)
        self.assertFalse(self.transaction.exists())

    def test_start_failure_retains_state_for_configure_retry(self):
        before = self.states()
        self.run_phase('preinst', 'upgrade')
        self.env['MOCK_FAIL'] = 'start:' + AGENT
        self.run_phase('postinst', 'configure', success=False)
        self.assertTrue(self.transaction.exists())
        self.assertEqual(self.states()[WATCHER], 'inactive')
        del self.env['MOCK_FAIL']
        self.run_phase('postinst', 'configure')
        self.assertEqual(self.states(), before)

    def test_policy_denial_does_not_stop_any_service(self):
        self.policy.write_text('#!/bin/sh\n[ "$2" != start ] || exit 101\n')
        self.policy.chmod(0o755)
        before = self.states()
        self.run_phase('preinst', 'upgrade')
        self.run_phase('postinst', 'configure')
        self.assertEqual(self.states(), before)
        self.assertEqual(self.operations(), [])

    def test_offline_chroot_and_alternate_root_skip_systemd(self):
        for key in ('SYSTEMD_OFFLINE', 'DPKG_ROOT', 'MOCK_CHROOT'):
            with self.subTest(key=key):
                self.env[key] = '1'
                self.run_phase('preinst', 'install')
                self.run_phase('postinst', 'configure')
                self.assertEqual(self.operations(), [])
                self.env[key] = '' if key == 'DPKG_ROOT' else '0'
        self.marker.rmdir()
        self.run_phase('preinst', 'install')
        self.assertEqual(self.operations(), [])

    def make_package(self, version, *, scripts=True, fail_preinst=False):
        package = self.root / ('package-' + version)
        control = package / 'DEBIAN'
        control.mkdir(parents=True)
        (control / 'control').write_text(f'Package: aiden-business\nVersion: {version}\nArchitecture: all\nMaintainer: Test <test@example.invalid>\nDescription: lifecycle fixture\n')
        (package / 'usr/share/aiden-test').mkdir(parents=True)
        (package / 'usr/share/aiden-test/version').write_text(version)
        if scripts:
            for source in self.scripts.iterdir():
                # dpkg --root sets DPKG_ROOT. Only the test fixture clears it
                # to exercise live-service behavior with its isolated mock.
                text = source.read_text().replace('set -eu\n', 'set -eu\nunset DPKG_ROOT\n', 1)
                if fail_preinst and source.name == 'preinst':
                    text = text.removesuffix('exit 0\n') + 'exit 42\n'
                target = control / source.name
                target.write_text(text)
                target.chmod(0o755)
        archive = self.root / ('aiden-business_' + version + '_all.deb')
        subprocess.run(['dpkg-deb', '--build', '--root-owner-group', str(package), str(archive)], check=True, stdout=subprocess.DEVNULL)
        return archive

    def dpkg(self, *args, success=True):
        result = subprocess.run(['dpkg', '--force-not-root', '--force-script-chrootless',
                                 '--root=' + str(self.root / 'dpkg-root'), *map(str, args)],
                                env=self.env, capture_output=True, text=True)
        if success:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0)
        return result

    @unittest.skipUnless(shutil.which('dpkg-deb') and shutil.which('dpkg'), 'requires Linux dpkg')
    def test_real_dpkg_legacy_upgrade_reinstall_and_remove(self):
        before = self.states()
        self.dpkg('-i', self.make_package('0.0.1-1', scripts=False))
        package = self.make_package('0.0.1-2')
        self.dpkg('-i', package)
        self.assertEqual(self.states(), before)
        self.dpkg('-i', package)
        self.assertEqual(self.states(), before)
        self.dpkg('--remove', 'aiden-business')
        self.assertTrue(all(value == 'inactive' for value in self.states().values()))
        self.assertFalse(self.transaction.exists())

    @unittest.skipUnless(shutil.which('dpkg-deb') and shutil.which('dpkg'), 'requires Linux dpkg')
    def test_real_dpkg_aborted_preinst_restores_old_service_state(self):
        before = self.states()
        self.dpkg('-i', self.make_package('0.0.1-1', scripts=False))
        self.dpkg('-i', self.make_package('0.0.1-2', fail_preinst=True), success=False)
        self.assertEqual(self.states(), before)
        self.assertEqual((self.root / 'dpkg-root/usr/share/aiden-test/version').read_text(), '0.0.1-1')

    @unittest.skipUnless(shutil.which('dpkg-deb') and shutil.which('dpkg'), 'requires Linux dpkg')
    def test_real_dpkg_configure_retry_restores_services(self):
        before = self.states()
        self.dpkg('-i', self.make_package('0.0.1-1', scripts=False))
        self.env['MOCK_FAIL'] = 'start:' + AGENT
        self.dpkg('-i', self.make_package('0.0.1-2'), success=False)
        self.assertTrue(self.transaction.exists())
        del self.env['MOCK_FAIL']
        self.dpkg('--configure', '-a')
        self.assertEqual(self.states(), before)
        self.assertFalse(self.transaction.exists())


if __name__ == '__main__':
    unittest.main()
