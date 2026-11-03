/**
 * OTA progress, read from the updater's log.
 *
 * `GET /api/ota/status` returns the running flag and the tail of
 * `config_web_ota_update.log`; the updater (`/usr/lib/aico/ota update`)
 * writes one `message="..."` line per step. This module turns the newest run
 * in that log into a stage and a percentage, so the page can show progress
 * rather than raw log text. Lines look like:
 *
 *   ota check: start
 *   ota download: rootfs.img.tar.gz progress 321.5 MiB/549.6 MiB (58%)
 *   ota write: rootfs -> rootfs_a progress 345.0 MiB/549.6 MiB (62%)
 *   ota reboot: requested after switching to slot a
 *   {"Updated":true,"NoUpdate":false,"Version":"debian-...","TargetSlot":0}
 *   ... [config_web][ota] update_exited exit_code=0
 *
 * Older logs drop the `ota ` prefix; both spellings are read.
 */

/** `debian-20260922-064044-201a2d08155d` -> `20260922-064044`, the readable part. */
export function shortVersion(version) {
  const match = /(\d{8}-\d{6})/.exec(String(version || ''));
  return match ? match[1] : String(version || '');
}

/** Stages in the order the updater runs them. */
export const STAGES = ['check', 'download', 'install', 'reboot'];

const STAGE_OF = {
  check: 'check', release: 'check', manifest: 'check', space: 'check', partition: 'check', asset: 'check',
  download: 'download', verify: 'download',
  write: 'install', readback: 'install', personalization: 'install', cleanup: 'install', misc: 'install',
  reboot: 'reboot',
};

const MESSAGE = /message="(?:ota )?([a-z]+):\s*([^"]*)"/;
const PROGRESS = /(\S+)(?:\s+->\s+\S+)?\s+(?:progress|complete)\s+([\d.]+\s*[KMG]iB)\/([\d.]+\s*[KMG]iB)(?:\s+\((\d+)%\))?/;
const EXIT = /update_exited exit_code=(-?\d+)/;

/**
 * @param {string} log - the log tail.
 * @returns {{
 *   state: 'idle'|'running'|'rebooting'|'updated'|'completed'|'up_to_date'|'failed',
 *   stage: string|null, percent: number|null, item: string, done: string, total: string,
 *   version: string, error: string, at: string,
 * }}
 */
export function parseOtaLog(log) {
  const lines = String(log || '').split('\n');
  // Hybrid runs span firmware checks and a reboot. Nested OTA check lines
  // belong to that same run; legacy logs still start at "check: start".
  let start = -1;
  let hybrid = false;
  lines.forEach((line, index) => {
    if (/message="hybrid: start"/.test(line)) { start = index; hybrid = true; }
    else if (!hybrid && /message="(?:ota )?check: start"/.test(line)) start = index;
  });
  if (start < 0) {
    start = lines.findIndex(line => /message="hybrid:/.test(line));
    hybrid = start >= 0;
  }
  const result = {state: 'idle', stage: null, percent: null, item: '', done: '', total: '', version: '', error: '', at: ''};
  if (start < 0) {
    // A run can fail before its first step (for example a GitHub 403).
    const exit = [...lines].reverse().find(line => EXIT.test(line));
    if (exit && Number(EXIT.exec(exit)[1]) !== 0) {
      result.state = 'failed';
      result.error = lastPlainLine(lines) || '';
      result.at = timestamp(exit);
    }
    return result;
  }
  const run = lines.slice(start);
  result.state = 'running';
  result.at = timestamp(run[0]);
  let lastPlain = '';
  for (const line of run) {
    const message = MESSAGE.exec(line);
    if (message) {
      if (message[1] === 'hybrid') {
        const phase = message[2];
        const stages = {'start': 'check', 'ota-check': 'check', 'ota-install': 'install',
          'waiting-reboot': 'reboot', 'ota-committed': 'package-check',
          'package-check': 'package-check', 'package-install': 'package-install'};
        if (stages[phase]) {
          result.stage = stages[phase];
          result.state = phase === 'waiting-reboot' ? 'rebooting' : 'running';
          result.percent = null;
          result.item = '';
        }
        if (phase === 'completed') result.state = 'completed';
        if (phase === 'up-to-date') result.state = 'up_to_date';
        continue;
      }
      if (/\[ERROR\]/.test(line)) lastPlain = message[2];
      const stage = STAGE_OF[message[1]];
      // The newest step wins: an image has several parts, and each part is
      // downloaded and then written, so the stage moves back and forth.
      if (stage) {
        if (stage !== result.stage) {
          result.percent = null;
          result.item = '';
        }
        result.stage = stage;
      }
      const progress = PROGRESS.exec(message[2]);
      if (progress && (message[1] === 'download' || message[1] === 'write')) {
        result.item = progress[1];
        result.done = progress[2];
        result.total = progress[3];
        result.percent = progress[4] != null ? Number(progress[4]) : 100;
      }
      if (stage === 'reboot') result.state = 'rebooting';
      continue;
    }
    const trimmed = line.trim();
    if (trimmed.startsWith('{')) {
      try {
        const summary = JSON.parse(trimmed);
        if (!hybrid && summary.NoUpdate) result.state = 'up_to_date';
        if (summary.Version) result.version = summary.Version;
      } catch (_error) {
        // Not the summary line.
      }
      continue;
    }
    const exit = EXIT.exec(line);
    if (exit) {
      const code = Number(exit[1]);
      result.at = timestamp(line) || result.at;
      if (code !== 0) {
        result.state = 'failed';
        result.error = lastPlain;
      } else if (result.state === 'running' || result.state === 'rebooting') {
        result.state = result.stage === 'reboot' ? 'updated' : result.state === 'rebooting' ? 'updated' : 'up_to_date';
      }
      continue;
    }
    if (trimmed && !/^\d{4}-\d{2}-\d{2}T/.test(trimmed)) lastPlain = trimmed;
    else if (/\[(ERROR|WARN)\]/.test(trimmed)) lastPlain = (/message="([^"]*)"/.exec(trimmed) || [])[1] || lastPlain;
  }
  return result;
}

function timestamp(line) {
  const match = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z)/.exec(String(line || ''));
  return match ? match[1] : '';
}

function lastPlainLine(lines) {
  for (let i = lines.length - 1; i >= 0; i -= 1) {
    const line = lines[i].trim();
    if (line && !/^\d{4}-\d{2}-\d{2}T/.test(line) && !line.startsWith('{')) return line;
  }
  return '';
}
