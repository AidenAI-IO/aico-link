/**
 * Swipe-to-delete wrapper for a list row, as native lists do it.
 *
 * Dragging the row left (finger or mouse) uncovers a red "删除" action behind
 * it; releasing past half its width leaves it open, otherwise it springs back.
 * While a row is open, tapping it closes it rather than activating it, and
 * only one row in the page is open at a time. A drag never counts as a tap.
 *
 * A row that must not be deleted (the provider in use) uncovers a grey
 * "使用中" action instead, which explains itself when tapped.
 */

import { el } from './dom.js';
import { msg, text } from './text.js';

const ACTION_WIDTH = 80;
/** Horizontal travel before a press becomes a drag rather than a tap. */
const DRAG_SLOP = 8;

let openRow = null;

/**
 * @param {HTMLElement} content - the row to wrap.
 * @param {object} options
 * @param {() => void} options.onDelete - called when the delete action is tapped.
 * @param {import('./text.js').TextSpec} [options.lockedLabel] - shown instead of
 *   delete when the row cannot be removed.
 * @param {() => void} [options.onLocked] - called when that label is tapped.
 * @param {string} [options.action] - `data-action` of the delete button.
 */
export function swipeRow(content, options) {
  const locked = Boolean(options.lockedLabel);
  const action = el(
    'button',
    {
      class: locked ? 'ds-swipe__action ds-swipe__action--locked' : 'ds-swipe__action',
      attrs: { type: 'button', tabindex: '-1' },
      data: { action: options.action || 'swipe-delete' },
      on: {
        click: () => {
          close();
          if (locked) options.onLocked && options.onLocked();
          else options.onDelete();
        },
      },
    },
    [text(locked ? options.lockedLabel : msg('action.delete', '删除'))],
  );
  content.classList.add('ds-swipe__content');
  const root = el('div', { class: 'ds-swipe' }, [action, content]);

  let offset = 0;
  let drag = null;
  let suppressClick = false;

  // The action grows with the gap the row leaves, so its label stays centred
  // in the visible part instead of being clipped while half revealed.
  const place = (x, animate) => {
    offset = x;
    content.classList.toggle('ds-swipe__content--settling', animate);
    action.classList.toggle('ds-swipe__action--settling', animate);
    content.style.transform = x ? `translateX(${x}px)` : '';
    action.style.width = `${-x}px`;
  };
  function open() {
    if (openRow && openRow !== api) openRow.close();
    place(-ACTION_WIDTH, true);
    openRow = api;
  }
  function close() {
    place(0, true);
    if (openRow === api) openRow = null;
  }

  content.addEventListener('pointerdown', (event) => {
    if (event.button !== 0) return;
    drag = {
      x: event.clientX,
      y: event.clientY,
      start: offset,
      active: false,
      id: event.pointerId,
    };
  });
  content.addEventListener('pointermove', (event) => {
    if (!drag || event.pointerId !== drag.id) return;
    const dx = event.clientX - drag.x;
    const dy = event.clientY - drag.y;
    if (!drag.active) {
      // A vertical move is a scroll; leave it to the page.
      if (Math.abs(dy) > DRAG_SLOP && Math.abs(dy) > Math.abs(dx)) {
        drag = null;
        return;
      }
      if (Math.abs(dx) < DRAG_SLOP) return;
      drag.active = true;
      content.classList.add('ds-swipe__content--dragging');
      content.setPointerCapture(event.pointerId);
    }
    place(Math.max(-ACTION_WIDTH, Math.min(0, drag.start + dx)), false);
  });
  const end = (event) => {
    if (!drag || (event && event.pointerId !== drag.id)) return;
    const wasDrag = drag.active;
    drag = null;
    content.classList.remove('ds-swipe__content--dragging');
    if (!wasDrag) return;
    suppressClick = true;
    setTimeout(() => {
      suppressClick = false;
    }, 0);
    if (offset < -ACTION_WIDTH / 2) open();
    else close();
  };
  content.addEventListener('pointerup', end);
  content.addEventListener('pointercancel', end);
  // Capture phase, so the row's own onPress never sees a drag or a close-tap.
  content.addEventListener(
    'click',
    (event) => {
      if (!suppressClick && offset === 0) return;
      event.stopImmediatePropagation();
      event.preventDefault();
      // The click that ends a drag only needs swallowing; a later tap on an
      // open row closes it.
      if (suppressClick) suppressClick = false;
      else close();
    },
    true,
  );

  place(0, false);
  const api = { el: root, open, close };
  return root;
}
