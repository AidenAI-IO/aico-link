#!/usr/bin/env python3
"""Exercise the real guard loop with deterministic network/clock commands."""
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
GUARD = ROOT / 'overlay-debian/usr/lib/aico/aico-wlan-guard'
MOCK = r'''#!/bin/sh
step=$(cat "$CASE_ROOT/step")
name=${0##*/}
if [ "$SCENARIO" = invalid_limit ]; then
    printf '%s\n' "$name $*" >> "$CASE_ROOT/actions"
fi
has_arg() {
    needle=$1
    shift
    for arg do
        [ "$arg" = "$needle" ] && return 0
    done
    return 1
}
has_option_value() {
    option=$1
    expected=$2
    shift 2
    while [ "$#" -gt 0 ]; do
        if [ "$1" = "$option" ] && [ "${2:-}" = "$expected" ]; then
            return 0
        fi
        shift
    done
    return 1
}
case "$name" in
    sleep)
        [ "$1" = 1 ] || exit 0
        step=$((step + 1))
        echo "$step" > "$CASE_ROOT/step"
        [ "$step" -lt "$STEPS" ] || kill -TERM "$PPID"
        ;;
    logger) echo "$step $*" >> "$CASE_ROOT/log" ;;
    wpa_cli)
        case "$*" in
            *status) [ "$SCENARIO" = disconnected ] || echo wpa_state=COMPLETED ;;
            *) echo "$step $*" >> "$CASE_ROOT/actions" ;;
        esac ;;
    ip)
        case "$*" in
            'route show default'*) echo 'default via 192.168.50.1 dev wlan0' ;;
            '-4 -o address'*) echo '5: wlan0 inet 192.168.50.50/24 scope global' ;;
            *) echo "$step $*" >> "$CASE_ROOT/actions" ;;
        esac ;;
    ping)
        case "$SCENARIO" in
            healthy) exit 0 ;;
            stable_reset) [ "$step" -ge 5 ] && [ "$step" -le 7 ] && exit 0 ;;
            brief_recovery) [ "$step" = 5 ] && exit 0 ;;
        esac
        exit 1 ;;
    arping)
        # Gratuitous announcements do not establish a fresh reply.
        has_arg -U "$@" && exit 1
        if [ "$SCENARIO" = icmp_filtered ] && has_arg -f "$@" \
            && has_option_value -c 2 "$@" \
            && has_option_value -w 3 "$@"; then
            exit 0
        fi
        exit 1 ;;
    systemctl|networkctl) echo "$step $name $*" >> "$CASE_ROOT/actions" ;;
    *) exit 1 ;;
esac
'''


class GuardTest(unittest.TestCase):
    def run_guard(self, scenario, steps=15, *, limit='2',
                  expected_returncode=-signal.SIGTERM):
        """Run a bounded guard simulation and return its actions and log.

        Mock sleeps advance the scenario without waiting and terminate only
        the guard process after the requested number of checks. Invalid-limit
        scenarios log every mocked command and expect an early validation exit.
        """
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'step').write_text('0\n')
            for name in ('actions', 'log'):
                (root / name).touch()
            for name in ('ip', 'ping', 'arping', 'wpa_cli', 'systemctl', 'networkctl', 'logger', 'sleep'):
                command = root / name
                command.write_text(MOCK)
                command.chmod(0o755)
            env = dict(os.environ, PATH=f'{root}:{os.environ["PATH"]}',
                       CASE_ROOT=str(root), SCENARIO=scenario, STEPS=str(steps),
                       WLAN_GUARD_INTERVAL='1', WLAN_GUARD_FAIL_THRESHOLD='2',
                       WLAN_GUARD_RECOVER_COOLDOWN='7', WLAN_GUARD_MAX_RECOVERIES=limit,
                       WLAN_GUARD_HEALTHY_THRESHOLD='3')
            result = subprocess.run(['sh', str(GUARD)], env=env, capture_output=True, text=True, timeout=15)
            self.assertEqual(result.returncode, expected_returncode, result.stderr)
            return (root / 'actions').read_text(), (root / 'log').read_text()

    def test_healthy_or_disconnected_links_are_not_reset(self):
        """Healthy, disconnected, and ICMP-filtering links avoid recovery actions."""
        for scenario in ('healthy', 'disconnected', 'icmp_filtered'):
            with self.subTest(scenario=scenario):
                actions, _ = self.run_guard(scenario)
                self.assertEqual(actions, '')

    def test_persistent_failure_has_a_bounded_recovery_budget(self):
        """Persistent failures exhaust the budget without further interface resets."""
        actions, log = self.run_guard('down')
        self.assertEqual(actions.count('reassociate'), 2)
        self.assertEqual(actions.count('systemctl restart'), 2)
        self.assertEqual(actions.count('networkctl reconfigure'), 2)
        self.assertTrue(actions.startswith('1 -i wlan0 reassociate\n'), actions)
        self.assertEqual(log.count('recovery limit reached'), 1)

    def test_brief_success_does_not_restart_recovery_loop(self):
        """One healthy check cannot replenish an exhausted recovery budget."""
        actions, log = self.run_guard('brief_recovery')
        self.assertEqual(actions.count('reassociate'), 2)
        self.assertNotIn('budget reset', log)

    def test_sustained_success_rearms_recovery(self):
        """Consecutive healthy checks restore the budget for a subsequent failure."""
        actions, log = self.run_guard('stable_reset')
        self.assertEqual(actions.count('reassociate'), 4)
        self.assertEqual(log.count('budget reset'), 1)

    def test_invalid_limit_exits_before_network_mutation(self):
        """Invalid limits fail before invoking any mocked network or helper command."""
        for value in ('0', '-1', 'bad'):
            with self.subTest(limit=value):
                actions, _ = self.run_guard('invalid_limit', limit=value,
                                            expected_returncode=1)
                self.assertEqual(actions, '')


if __name__ == '__main__':
    unittest.main()
