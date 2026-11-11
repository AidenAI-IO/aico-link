/**
 * OTA progress parser: the settings page shows the updater's stage and
 * percentage by reading its log, so the log shapes it relies on are pinned.
 */

import assert from 'node:assert/strict';
import path from 'node:path';
import {fileURLToPath, pathToFileURL} from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const {businessReleaseVersion, parseOtaLog} = await import(pathToFileURL(path.join(root, 'src/config_web/web/assets/js/app/ota-progress.js')).href);

assert.equal(businessReleaseVersion('0.0.78-1'), '0.0.78');
assert.equal(businessReleaseVersion('1:0.0.78-1'), '0.0.78');
assert.equal(businessReleaseVersion(''), '');

const line = (time, message) => `${time} [INFO][ota][updater] log_message message="${message}"`;
const run = [
  line('2026-09-23T09:15:04Z', 'ota check: start'),
  line('2026-09-23T09:15:05Z', 'ota manifest: verified version=debian-2 channel=debian-stable parts=2'),
  line('2026-09-23T09:15:10Z', 'ota download: boot_a.img.tar.gz progress 4.0 MiB/8.0 MiB (50%)'),
  line('2026-09-23T09:15:12Z', 'ota write: boot -> boot_a complete 8.0 MiB/8.0 MiB'),
  line('2026-09-23T09:16:00Z', 'ota download: rootfs.img.tar.gz progress 60.0 MiB/120.0 MiB (50%)'),
];

// Parts alternate between download and install; the newest step is shown.
let state = parseOtaLog(run.join('\n'));
assert.equal(state.state, 'running');
assert.equal(state.stage, 'download');
assert.equal(state.percent, 50);
assert.equal(state.item, 'rootfs.img.tar.gz');

run.push(line('2026-09-23T09:20:00Z', 'ota write: rootfs -> rootfs_a progress 300.0 MiB/549.6 MiB (55%)'));
state = parseOtaLog(run.join('\n'));
assert.equal(state.stage, 'install');
assert.equal(state.percent, 55);

run.push(line('2026-09-23T09:27:15Z', 'ota reboot: requested after switching to slot a'));
assert.equal(parseOtaLog(run.join('\n')).state, 'rebooting');

run.push('{"Updated":true,"NoUpdate":false,"Version":"debian-2","TargetSlot":0}');
run.push('2026-09-23T09:27:15Z [INFO][config_web][ota] update_exited exit_code=0');
state = parseOtaLog(run.join('\n'));
assert.equal(state.state, 'updated');
assert.equal(state.version, 'debian-2');

// Only the newest run counts, and a failure keeps its reason.
const failed = parseOtaLog([...run,
  line('2026-09-24T01:00:00Z', 'ota check: start'),
  'GitHub release status 403',
  '2026-09-24T01:00:03Z [ERROR][config_web][ota] update_exited exit_code=1'].join('\n'));
assert.equal(failed.state, 'failed');
assert.equal(failed.error, 'GitHub release status 403');

const current = parseOtaLog([line('2026-09-25T01:00:00Z', 'ota check: start'),
  '{"Updated":false,"NoUpdate":true,"Version":"debian-2","TargetSlot":0}',
  '2026-09-25T01:00:04Z [INFO][config_web][ota] update_exited exit_code=0'].join('\n'));
assert.equal(current.state, 'up_to_date');

// Pre-prefix logs (no leading "ota ") still parse.
assert.equal(parseOtaLog('x message="check: start"\nx message="download: a progress 1 MiB/2 MiB (50%)"').percent, 50);
assert.equal(parseOtaLog('').state, 'idle');

const hybrid = [line('2026-09-30T01:00:00Z', 'hybrid: start'),
  line('2026-09-30T01:00:01Z', 'hybrid: ota-check'),
  line('2026-09-30T01:00:02Z', 'hybrid: package-check'),
  line('2026-09-30T01:00:03Z', 'hybrid: package-install')];
assert.equal(parseOtaLog(hybrid.join('\n')).stage, 'package-install');
hybrid.push(line('2026-09-30T01:00:04Z', 'hybrid: completed'));
hybrid.push('2026-09-30T01:00:04Z [config_web][ota] update_exited exit_code=0');
assert.equal(parseOtaLog(hybrid.join('\n')).state, 'completed', 'package-only completion must not request a reboot');

const resumed = [line('2026-09-30T02:00:00Z', 'hybrid: start'),
  line('2026-09-30T02:00:01Z', 'ota check: start'),
  line('2026-09-30T02:01:00Z', 'ota reboot: requested after switching to slot b'),
  line('2026-09-30T02:02:00Z', 'hybrid: ota-committed'),
  line('2026-09-30T02:02:01Z', 'hybrid: package-check')];
assert.equal(parseOtaLog(resumed.join('\n')).state, 'running');
assert.equal(parseOtaLog(resumed.join('\n')).stage, 'package-check');
resumed.push(line('2026-09-30T02:02:02Z', 'hybrid: up-to-date'));
assert.equal(parseOtaLog(resumed.join('\n')).state, 'up_to_date');

const failedHybrid = [line('2026-09-30T03:00:00Z', 'hybrid: start'),
  '2026-09-30T03:00:01Z [ERROR][config_web][ota] message="APT index download failed"',
  '2026-09-30T03:00:01Z [config_web][ota] update_exited exit_code=1'];
assert.equal(parseOtaLog(failedHybrid.join('\n')).error, 'APT index download failed');
assert.equal(parseOtaLog(failedHybrid.join('\n')).state, 'failed');
assert.equal(parseOtaLog([...failedHybrid, ...hybrid].join('\n')).state, 'completed');
assert.equal(parseOtaLog(hybrid.slice(2).join('\n')).state, 'completed', 'truncated logs still show package completion');

process.stdout.write('config web OTA progress tests passed\n');
