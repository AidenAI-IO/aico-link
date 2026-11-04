/**
 * Transient status message.
 *
 * One toast at a time: a newer message replaces the current one rather than
 * stacking, which is what the native side does with a single overlay.
 */

import {el} from './dom.js';
import {resolve} from './text.js';

const VISIBLE_MS = 2600;

let currentNode = null;
let timer = null;

/**
 * Show a message.
 *
 * @param {import('./text.js').TextSpec} message
 * @param {object} [options]
 * @param {number} [options.durationMs] - auto-dismiss delay; `0` keeps it until
 *   the next toast or an explicit `dismiss()`.
 */
export function toast(message, options = {}) {
  dismiss();
  const node = el('div', {class: 'ds-toast', attrs: {role: 'status'}, text: resolve(message)});
  document.body.appendChild(node);
  currentNode = node;
  const duration = options.durationMs ?? VISIBLE_MS;
  if (duration > 0) timer = setTimeout(dismiss, duration);
  return node;
}

/** Remove the visible toast, if any. */
export function dismiss() {
  if (timer) {
    clearTimeout(timer);
    timer = null;
  }
  if (currentNode && currentNode.parentNode) {
    currentNode.parentNode.removeChild(currentNode);
  }
  currentNode = null;
}
