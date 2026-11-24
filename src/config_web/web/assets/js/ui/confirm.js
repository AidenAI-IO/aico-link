/**
 * Confirmation action sheet: the one control every destructive or restarting
 * action asks through, drawn as the design's iOS action sheet.
 *
 *   ┌─────────────────────────────┐
 *   │         title               │  translucent card: title and message
 *   │   message, one or two lines │  centred in the secondary ink,
 *   ├─────────────────────────────┤  a hairline,
 *   │        Confirm action       │  then the action (red when destructive)
 *   └─────────────────────────────┘
 *   ┌─────────────────────────────┐
 *   │           Cancel            │  a separate white card, bold
 *   └─────────────────────────────┘
 *
 * It floats inset from the screen edges above a dimming scrim and slides up
 * on the sheet curve. Tapping the scrim or pressing Escape cancels.
 */

import { el } from './dom.js';
import { msg, text } from './text.js';

/** Dismiss animation length; matches `--motion-sheet`. */
const CLOSE_MS = 320;

/**
 * @param {object} props
 * @param {import('./text.js').TextSpec} props.title
 * @param {import('./text.js').TextSpec} [props.body]
 * @param {import('./text.js').TextSpec} props.confirmLabel
 * @param {boolean} [props.danger] - the action destroys data; its label is red.
 * @param {string} [props.action] - `data-action` of the confirm button.
 * @returns {Promise<boolean>} true when confirmed; dismissing counts as cancel.
 */
export function confirmSheet(props) {
  return new Promise((resolvePromise) => {
    let settled = false;
    const confirm = el(
      'button',
      {
        class: props.danger
          ? 'ds-action-sheet__button ds-action-sheet__button--danger'
          : 'ds-action-sheet__button',
        attrs: { type: 'button' },
        data: { action: props.action || 'confirm' },
      },
      [text(props.confirmLabel)],
    );
    const cancel = el(
      'button',
      {
        class: 'ds-action-sheet__button ds-action-sheet__button--cancel',
        attrs: { type: 'button' },
        data: { action: 'cancel-confirm' },
      },
      [text(msg('action.cancel', '取消'))],
    );

    const panel = el('div', { class: 'ds-action-sheet__panel' }, [
      el('div', { class: 'ds-action-sheet__group' }, [
        el('div', { class: 'ds-action-sheet__header' }, [
          text(props.title, 'ds-action-sheet__title'),
          props.body ? text(props.body, 'ds-action-sheet__message') : null,
        ]),
        confirm,
      ]),
      el('div', { class: 'ds-action-sheet__group ds-action-sheet__group--cancel' }, [cancel]),
    ]);
    const scrim = el('div', { class: 'ds-action-sheet__scrim' });
    const root = el(
      'div',
      { class: 'ds-action-sheet', attrs: { role: 'alertdialog', 'aria-modal': 'true' } },
      [scrim, panel],
    );

    function close(result) {
      if (settled) return;
      settled = true;
      document.removeEventListener('keydown', onKeydown);
      scrim.classList.remove('ds-action-sheet__scrim--open');
      panel.classList.remove('ds-action-sheet__panel--open');
      setTimeout(() => root.remove(), CLOSE_MS);
      resolvePromise(result);
    }
    function onKeydown(event) {
      if (event.key === 'Escape') close(false);
    }

    confirm.addEventListener('click', () => close(true));
    cancel.addEventListener('click', () => close(false));
    scrim.addEventListener('click', () => close(false));
    document.addEventListener('keydown', onKeydown);

    document.body.appendChild(root);
    // Paint the closed state once so the slide-up transition runs.
    void root.offsetHeight;
    scrim.classList.add('ds-action-sheet__scrim--open');
    panel.classList.add('ds-action-sheet__panel--open');
    setTimeout(() => cancel.focus(), 0);
  });
}
