/**
 * Apply levels: what a saved change needs before it takes effect.
 *
 * The levels are defined once, by the Agent (`agent.ApplyLevel`), and reported
 * by `POST /api/config/plan` before saving and by `PATCH /api/config` after:
 *
 *   - `live`           hot-reloaded by the running Agent;
 *   - `agent_restart`  read only at Agent start; this page and the USB link stay up;
 *   - `reboot`         changes the USB gadget the phone enumerated; the device
 *                      restarts and every connection to it drops for a while.
 *
 * Both restart levels share one save label, "保存并重启"; the confirmation
 * says which restart it is, because their cost to the user is very different.
 */

import {request} from './data.js';
import {postToHost} from './host.js';
import {button} from '../ui/button.js';
import {el} from '../ui/dom.js';
import {msg, resolve, text} from '../ui/text.js';
import {confirmSheet} from '../ui/confirm.js';

export const APPLY_LIVE = 'live';
export const APPLY_AGENT_RESTART = 'agent_restart';
export const APPLY_REBOOT = 'reboot';

/** How long a reboot may take before the page gives up waiting, ms. */
const REBOOT_TIMEOUT_MS = 180000;
/** A reboot that has not taken the device down by now did not happen, ms. */
const REBOOT_START_TIMEOUT_MS = 45000;
const AGENT_RESTART_TIMEOUT_MS = 60000;
/**
 * Without a `runtime_id` from before the restart there is nothing to compare
 * against, and a quick restart can fall between two polls. After this long an
 * Agent that answers is taken to be the restarted one, ms.
 */
const AGENT_RESTART_BLIND_GRACE_MS = 10000;
const POLL_INTERVAL_MS = 2000;
const PROBE_TIMEOUT_MS = 3000;
/** Answered by Config Web, and carries the Agent's `runtime_id` once it is up. */
const PROBE_URL = '/api/config/application';

/** @param {string} level */
export function needsRestart(level) {
  return level === APPLY_REBOOT || level === APPLY_AGENT_RESTART;
}

/** The save button's label for a level. */
export function saveLabel(level) {
  return needsRestart(level) ? msg('apply.save_restart', '保存并重启') : msg('action.save', '保存');
}

/**
 * Ask the Agent what saving `patch` would need, without saving it.
 *
 * @param {object} patch - the same `config` object `PATCH /api/config` takes.
 * @returns {Promise<string>} an apply level.
 */
export async function planApply(patch) {
  const payload = await request('/api/config/plan', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({config: patch}),
  });
  return payload.apply || APPLY_LIVE;
}

const COPY = {
  [APPLY_REBOOT]: {
    title: msg('apply.reboot_title', '需要重启设备'),
    body: msg('apply.reboot_body', '设备将重启以应用这项设置。重启期间与手机的连接会断开，大约 1 分钟后恢复。'),
    progress: msg('apply.rebooting', '设备正在重启…'),
    progressBody: msg('apply.rebooting_body', '连接恢复后页面会自动刷新。'),
  },
  [APPLY_AGENT_RESTART]: {
    title: msg('apply.agent_restart_title', '需要重启 Agent'),
    body: msg('apply.agent_restart_body', 'Agent 将重启以应用这项设置，当前对话会中断几秒。'),
    progress: msg('apply.agent_restarting', 'Agent 正在重启…'),
    progressBody: msg('apply.agent_restarting_body', '通常几秒内完成。'),
  },
};

/**
 * Confirm a save that restarts something.
 *
 * @param {string} level
 * @returns {Promise<boolean>} true to save and restart.
 */
export function confirmRestart(level) {
  const copy = COPY[level];
  if (!copy) return Promise.resolve(true);
  return confirmSheet({title: copy.title, body: copy.body, confirmLabel: saveLabel(level), action: 'confirm-restart'});
}

/** GET the probe URL, resolving to `{ok, body}` or null when unreachable. */
async function probe() {
  const controller = typeof AbortController === 'undefined' ? null : new AbortController();
  const timer = controller ? setTimeout(() => controller.abort(), PROBE_TIMEOUT_MS) : null;
  try {
    const response = await fetch(PROBE_URL, {cache: 'no-store', signal: controller?.signal});
    let body = {};
    try {
      body = await response.json();
    } catch (_error) {
      // A non-JSON answer still proves the server is up.
    }
    return {ok: response.ok, body: body || {}};
  } catch (_error) {
    return null;
  } finally {
    if (timer) clearTimeout(timer);
  }
}

const sleep = ms => new Promise(done => setTimeout(done, ms));

/**
 * Wait for the device to go down and come back. Exported for the OTA page,
 * whose reboot the updater starts itself.
 */
export async function waitForReboot() {
  const started = Date.now();
  let sawDown = false;
  while (Date.now() - started < REBOOT_TIMEOUT_MS) {
    await sleep(POLL_INTERVAL_MS);
    const result = await probe();
    if (!result) {
      sawDown = true;
    } else if (sawDown && result.ok) {
      return;
    } else if (!sawDown && Date.now() - started > REBOOT_START_TIMEOUT_MS) {
      throw new Error(resolve(msg('apply.reboot_not_started', '设备没有重启，请稍后重试。')));
    }
  }
  throw new Error(resolve(msg('apply.restart_timeout', '等待重启超时，请检查设备。')));
}

/** Wait for a new Agent process, recognised by a new `runtime_id`. */
async function waitForAgentRestart(previousRuntime) {
  const started = Date.now();
  let sawDown = false;
  while (Date.now() - started < AGENT_RESTART_TIMEOUT_MS) {
    await sleep(POLL_INTERVAL_MS / 2);
    const result = await probe();
    const runtime = result && result.ok ? result.body.runtime_id : '';
    if (!runtime) {
      sawDown = true;
    } else if (previousRuntime) {
      if (runtime !== previousRuntime) return;
    } else if (sawDown || Date.now() - started >= AGENT_RESTART_BLIND_GRACE_MS) {
      return;
    }
  }
  throw new Error(resolve(msg('apply.restart_timeout', '等待重启超时，请检查设备。')));
}

/** Full-screen progress overlay; `fail()` turns it into a dismissible error. */
function progressOverlay(copy) {
  const title = text(copy.progress, 'ds-restart__title');
  const body = text(copy.progressBody, 'ds-restart__body');
  const panel = el('div', {class: 'ds-restart__panel'}, [el('div', {class: 'ds-restart__spinner'}), title, body]);
  const root = el('div', {class: 'ds-restart', attrs: {role: 'alertdialog', 'aria-live': 'polite'}}, [panel]);
  document.body.appendChild(root);
  const remove = () => root.parentNode && root.parentNode.removeChild(root);
  return {
    remove,
    fail(message) {
      panel.replaceChildren(
        text(msg('apply.restart_failed', '重启未完成'), 'ds-restart__title'),
        el('span', {class: 'ds-restart__body', text: message}),
        button({label: msg('action.close', '关闭'), variant: 'secondary', block: true, action: 'close-restart', onPress: remove}),
      );
    },
  };
}

/**
 * Carry out the restart a saved change needs, and wait until it is over.
 *
 * A reboot takes this page's server down too, so the page reloads itself once
 * the device answers again; an Agent restart leaves the page in place.
 *
 * @param {string} level
 * @returns {Promise<boolean>} true once the restart completed.
 */
export async function performRestart(level) {
  const copy = COPY[level];
  if (!copy) return true;
  postToHost({type: 'aico_config_restart', apply: level, phase: 'started'});
  const overlay = progressOverlay(copy);
  try {
    if (level === APPLY_REBOOT) {
      await request('/api/device/reboot', {method: 'POST'});
      await waitForReboot();
      postToHost({type: 'aico_config_restart', apply: level, phase: 'finished'});
      window.location.reload();
      return true;
    }
    const before = await probe();
    await request('/api/agent/restart', {method: 'POST'});
    await waitForAgentRestart(before && before.ok ? before.body.runtime_id : '');
    overlay.remove();
    postToHost({type: 'aico_config_restart', apply: level, phase: 'finished'});
    return true;
  } catch (error) {
    overlay.fail(error && error.message ? error.message : resolve(msg('apply.restart_timeout', '等待重启超时，请检查设备。')));
    postToHost({type: 'aico_config_restart', apply: level, phase: 'failed'});
    return false;
  }
}
