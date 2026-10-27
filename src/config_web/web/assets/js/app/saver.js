/**
 * Per-page save controller.
 *
 * The rule every settings page follows: a change the Agent applies live is
 * saved the moment it is made, so there is no save button for it; a change
 * that needs a restart is staged, and the top bar (or the app's native bar)
 * offers "保存并重启" for everything staged. The apply level of each change
 * comes from `POST /api/config/plan`, never from a list kept in the page.
 *
 * Changes are keyed by the setting they touch, so changing a staged value back
 * — which plans as `live` — drops it from the staged set again.
 */

import {
  APPLY_LIVE,
  confirmRestart,
  needsRestart,
  performRestart,
  planApply,
  saveLabel,
} from './apply.js';
import {request} from './data.js';
import {postToHost} from './host.js';
import {button} from '../ui/button.js';
import {msg, resolve} from '../ui/text.js';
import {toast} from '../ui/toast.js';

const RANK = {live: 0, agent_restart: 1, reboot: 2};

/**
 * The app's native "保存" arrives as one window event. A single listener hands
 * it to the saver of the page on screen (the newest one whose root is still in
 * the document), so pages that come and go leave no listener behind.
 */
let saveRequestHandler = null;
let listening = false;
function handleSaveRequests(handler) {
  saveRequestHandler = handler;
  if (listening) return;
  listening = true;
  const onSaveRequest = () => { if (saveRequestHandler) saveRequestHandler(); };
  window.addEventListener('aico:save-request', onSaveRequest);
}

/** Merge JSON-merge-patch objects, later keys winning. */
function mergePatch(target, source) {
  for (const [key, value] of Object.entries(source)) {
    if (value && typeof value === 'object' && !Array.isArray(value)) {
      target[key] = mergePatch(target[key] && typeof target[key] === 'object' ? target[key] : {}, value);
    } else {
      target[key] = value;
    }
  }
  return target;
}

async function patchConfig(patch) {
  return request('/api/config', {
    method: 'PATCH',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({config: patch}),
  });
}

/**
 * @param {object} context - the route context (`header`, `navigate`).
 * @param {object} options
 * @param {import('../ui/text.js').TextSpec} options.title
 * @param {string} options.back - route the back button returns to.
 * @param {Node} options.root - the page's node; the controller goes quiet once
 *   it is detached, so a stale page cannot answer the app's save request.
 * @param {(payload: object) => void} [options.onSaved] - receives each save response.
 * @param {{ready: () => boolean, submit: () => void}} [options.form] - a page
 *   that saves a whole form at once (a provider's credentials) rather than
 *   value by value. Its 保存 sits where staged restarts would, in the top bar
 *   or the app's native bar, and shows while `ready()` holds.
 */
export function createSaver(context, options) {
  /** @type {Map<string, {patch: object, level: string}>} */
  const staged = new Map();
  const sequence = new Map();
  let committing = false;
  let confirming = false;

  const level = () =>
    [...staged.values()].reduce((max, entry) => (RANK[entry.level] > RANK[max] ? entry.level : max), APPLY_LIVE);

  const formReady = () => Boolean(options.form && staged.size === 0 && !committing && options.form.ready());

  function refreshChrome() {
    const pending = staged.size > 0 && !committing;
    const form = formReady();
    const action = pending
      ? button({label: saveLabel(level()), variant: 'quiet', action: 'save-restart', onPress: commit})
      : form
        ? button({label: saveLabel(APPLY_LIVE), variant: 'quiet', action: 'save-form', onPress: () => options.form.submit()})
        : null;
    if (context.header) context.header({title: options.title, onBack: () => context.navigate(options.back), action});
    const apply = pending ? level() : APPLY_LIVE;
    postToHost({
      type: 'aico_config_save_state',
      title: resolve(options.title),
      dirty: pending || form,
      apply,
      // Translated here, in the device's language, because the app has no
      // catalogue of its own for the native bar.
      action_label: resolve(saveLabel(apply)),
    });
  }

  function saved(payload) {
    if (options.onSaved) options.onSaved(payload);
  }

  /**
   * Apply one setting change.
   *
   * @param {string} key - identifies the setting, e.g. `agent.max_iterations`.
   * @param {object} patch - the `config` merge patch for it.
   * @param {{quietInvalid?: boolean}} [opts] - leave a rejected value to the
   *   caller instead of showing the Agent's validation message.
   * @returns {Promise<'saved'|'staged'|'invalid'|'failed'|'superseded'>}
   *   `invalid` means the Agent rejected the value before anything was written;
   *   `superseded` means a newer change to the same setting took over.
   */
  async function change(key, patch, opts = {}) {
    const current = (sequence.get(key) || 0) + 1;
    sequence.set(key, current);
    let planned;
    try {
      planned = await planApply(patch);
    } catch (error) {
      if (error && error.status === 400) {
        if (!opts.quietInvalid) toastError(error);
        return 'invalid';
      }
      // Planning is advisory; the save response still carries the real level.
      console.warn('[settings] apply plan unavailable:', error && error.message);
      planned = APPLY_LIVE;
    }
    if (sequence.get(key) !== current) return 'superseded';

    if (needsRestart(planned)) {
      staged.set(key, {patch, level: planned});
      refreshChrome();
      return 'staged';
    }
    staged.delete(key);
    refreshChrome();
    try {
      const payload = await patchConfig(patch);
      // A newer change to the same key may have finished first; do not let this
      // older response overwrite UI state or toast through onSaved.
      if (sequence.get(key) !== current) return 'superseded';
      saved(payload);
      if (needsRestart(payload.apply)) {
        // The plan was unavailable and the change turned out to need a
        // restart. It is already saved; ask before restarting.
        if (await confirmRestart(payload.apply)) await performRestart(payload.apply);
        else toast(msg('apply.saved_pending_restart', '已保存，重启后生效'));
        return 'saved';
      }
      toast(msg('basic.saved', '已保存'));
      return 'saved';
    } catch (error) {
      if (sequence.get(key) !== current) return 'superseded';
      toastError(error);
      return 'failed';
    }
  }

  function toastError(error) {
    toast(error && error.message ? error.message : resolve(msg('basic.save_failed', '保存失败')));
  }

  /** Save everything staged, after the user confirms the restart. */
  async function commit() {
    // `confirming` covers the sheet: the app's native bar stays tappable
    // behind it, and a second tap must not open a second confirmation.
    if (staged.size === 0 || committing || confirming) return;
    const restart = level();
    confirming = true;
    let confirmed = false;
    try {
      confirmed = await confirmRestart(restart);
    } finally {
      confirming = false;
    }
    if (!confirmed) {
      refreshChrome();
      return;
    }
    const patch = [...staged.values()].reduce((merged, entry) => mergePatch(merged, entry.patch), {});
    committing = true;
    refreshChrome();
    let restartLevel = null;
    try {
      const payload = await patchConfig(patch);
      staged.clear();
      saved(payload);
      restartLevel = needsRestart(payload.apply) ? payload.apply : null;
      if (!restartLevel) toast(msg('basic.saved', '已保存'));
    } catch (error) {
      toastError(error);
    } finally {
      committing = false;
      refreshChrome();
    }
    // The user agreed to restart for this change, and the save response is
    // authoritative about whether one is still needed.
    if (restartLevel) await performRestart(restartLevel);
  }

  const onSaveRequest = () => {
    if (!options.root.isConnected) return;
    if (staged.size === 0 && formReady()) options.form.submit();
    else commit();
  };
  handleSaveRequests(onSaveRequest);

  return {
    change,
    commit,
    /** True while `key` holds a change waiting for "保存并重启". */
    isStaged: key => staged.has(key),
    /** Describe the page to the top bar and the app. Call once per render. */
    refreshChrome,
  };
}
