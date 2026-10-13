/**
 * Bottom sheet.
 *
 * The panel slides up on the design's sheet curve, can be dismissed by tapping
 * the scrim, pressing Escape, or dragging it down past a threshold. The same
 * geometry and curve constants drive `Modal` + `Animated` in React Native.
 */

import {el, append} from './dom.js';
import {text} from './text.js';

/** Fraction of the panel height that must be dragged before release dismisses. */
const DISMISS_RATIO = 0.28;
/** Downward drag in pixels that dismisses regardless of panel height. */
const DISMISS_FLOOR = 80;
/** Movement in pixels after which a press is a drag rather than a tap. */
const CAPTURE_AFTER_PX = 4;
/** Elements whose own taps must not start a drag. */
const INTERACTIVE = 'input, textarea, select, button, a, label, [role="button"], [role="option"]';

/**
 * @param {object} props
 * @param {import('./text.js').TextSpec} [props.title]
 * @param {Node|Node[]} props.body
 * @param {() => void} [props.onClose]
 * @returns {{el: HTMLElement, open: () => void, close: () => void, isOpen: () => boolean, onAfterClose: (fn: () => void) => void}}
 */
export function sheet(props) {
  const panel = el('div', {class: 'ds-sheet__panel'}, [
    el('div', {class: 'ds-sheet__grabber'}, [el('div', {class: 'ds-sheet__handle'})]),
    props.title != null ? text(props.title, 'ds-sheet__title') : null,
    el('div', {class: 'ds-sheet__body'}, props.body),
  ]);

  const scrim = el('div', {class: 'ds-sheet__scrim'});
  const root = el('div', {
    class: 'ds-sheet',
    attrs: {role: 'dialog', 'aria-modal': 'true', hidden: 'hidden'},
  });
  root.appendChild(scrim);
  root.appendChild(panel);

  let open = false;
  let afterClose = null;
  /** Set while the panel is following a finger, which suspends the CSS transition. */
  let dragging = null;

  function setOpen(next) {
    if (next === open) return;
    open = next;
    if (open) {
      root.removeAttribute('hidden');
      document.body.appendChild(root);
      // Force a layout read so the transition runs from the transformed state.
      void root.offsetHeight;
      scrim.classList.add('ds-sheet__scrim--open');
      panel.classList.add('ds-sheet__panel--open');
      document.addEventListener('keydown', onKeydown);
      const focusTarget = panel.querySelector('input, button');
      if (focusTarget) setTimeout(() => focusTarget.focus(), 0);
    } else {
      scrim.classList.remove('ds-sheet__scrim--open');
      panel.classList.remove('ds-sheet__panel--open');
      document.removeEventListener('keydown', onKeydown);
      const finish = () => {
        if (open) return;
        root.setAttribute('hidden', 'hidden');
        if (root.parentNode) root.parentNode.removeChild(root);
        if (afterClose) afterClose();
      };
      // Prefer the transition end, but never hang if the event is missed.
      panel.addEventListener('transitionend', finish, {once: true});
      setTimeout(finish, 400);
    }
    if (props.onClose && !open) props.onClose();
  }

  scrim.addEventListener('click', () => setOpen(false));

  // Drag to dismiss, using pointer events so mouse and touch share one path.
  // Controls and tappable rows keep their taps: a drag never starts on them.
  panel.addEventListener('pointerdown', event => {
    if (!open || event.target.closest(INTERACTIVE)) return;
    dragging = {startY: event.clientY, offset: 0, id: event.pointerId, captured: false};
    panel.style.transition = 'none';
  });
  panel.addEventListener('pointermove', event => {
    if (!dragging || event.pointerId !== dragging.id) return;
    dragging.offset = Math.max(0, event.clientY - dragging.startY);
    panel.style.transform = `translateY(${dragging.offset}px)`;
    // Capture only once this is clearly a drag, so the finger can leave the
    // panel. Capturing on press would retarget a plain tap's click to the panel.
    if (!dragging.captured && dragging.offset > CAPTURE_AFTER_PX && panel.setPointerCapture) {
      dragging.captured = true;
      try {
        panel.setPointerCapture(event.pointerId);
      } catch {
        // A pointer that already ended cannot be captured; the drag still works.
      }
    }
  });

  function endDrag(event) {
    if (!dragging || (event && event.pointerId !== dragging.id)) return;
    const offset = dragging.offset;
    const travelled = dragging;
    dragging = null;
    panel.style.transition = '';
    panel.style.transform = '';
    const threshold = Math.max(DISMISS_FLOOR, panel.offsetHeight * DISMISS_RATIO);
    if (travelled && offset > threshold) setOpen(false);
  }
  panel.addEventListener('pointerup', endDrag);
  panel.addEventListener('pointercancel', endDrag);

  function onKeydown(event) {
    if (event.key !== 'Escape' || !open) return;
    event.stopPropagation();
    setOpen(false);
  }
  // Listened for only while open: choice sheets are built afresh on every tap,
  // and a listener per sheet would otherwise outlive it.

  return {
    el: root,
    open: () => setOpen(true),
    close: () => setOpen(false),
    isOpen: () => open,
    onAfterClose: fn => {
      afterClose = fn;
    },
  };
}

export {append};
